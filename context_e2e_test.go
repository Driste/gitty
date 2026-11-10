package main

// End-to-end tests for cross-repository context: the index, `related`,
// `context`, `fetch`. They share e2e_test.go's harness — the real binary,
// real git, the fake GitLab.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gitty/internal/contract"
)

// contextEventRe is the stdout grammar of the context commands.
var contextEventRe = regexp.MustCompile(`^(index|link|unresolved|related|related-by|clone|pull|reclone|skip|error|plan|summary) `)

func assertContextEvents(t *testing.T, stdout string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if line != "" && !contextEventRe.MatchString(line) {
			t.Errorf("stdout line is not a stable event: %q", line)
		}
	}
}

// wantLines fails unless stdout is exactly these lines, in this order.
func wantLines(t *testing.T, what, stdout string, want ...string) {
	t.Helper()
	got := strings.TrimRight(stdout, "\n")
	if got != strings.Join(want, "\n") {
		t.Errorf("%s stdout:\n%s\n--- want:\n%s", what, got, strings.Join(want, "\n"))
	}
}

// linkedFixture publishes four projects on a fake instance:
//
//	acme/app           includes CI from acme/ci-templates, depends on acme/lib
//	                   and refers to acme/old-lib, which does not exist
//	acme/ci-templates  pipeline templates
//	acme/lib           a library, with agent instructions
//	acme/app-fork      a fork of acme/app
type linkedFixture struct {
	f   *fakeGitLab
	app string // work repo of acme/app, for pushing updates
}

const appCI = `include:
  - project: acme/ci-templates
    file: /templates/build.yml

build:
  script:
    - make
`

func appPackageJSON(instance string) string {
	return `{
  "name": "app",
  "dependencies": {
    "lib": "git+` + instance + `/acme/lib.git#v1.2.0",
    "old": "git+` + instance + `/acme/old-lib.git"
  }
}
`
}

func newLinkedFixture(t *testing.T) *linkedFixture {
	t.Helper()
	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}

	app := f.addRepo(t, "acme/app", map[string]string{
		".gitlab-ci.yml": appCI,
		"package.json":   appPackageJSON(f.srv.URL),
		"README.md":      "# app\n\nCustomer-facing API.\n",
		"src/main.txt":   "main\n",
	})
	f.addRepo(t, "acme/ci-templates", map[string]string{
		"README.md":           "# CI templates\n\n[![badge](x)](y)\n\nShared pipeline templates for every\nacme project.\n\n## Use\n\nInclude them.\n",
		"templates/build.yml": "build:\n  script: [make]\n",
	})
	f.addRepo(t, "acme/app-fork", map[string]string{"README.md": "# app, forked\n"})
	f.addRepo(t, "acme/lib", map[string]string{
		"README.md":    "# lib\n\nShared domain types.\n",
		"AGENTS.md":    "Run `make test` before committing.\n",
		"CHANGELOG.md": "## 1.2.0\n",
		"lib.txt":      "needle in the library\n",
	})

	appP, ci, lib := f.project(1, "acme/app"), f.project(2, "acme/ci-templates"), f.project(3, "acme/lib")
	appP.Description = "Customer-facing API"
	appP.Topics = []string{"go", "api"}
	ci.Description = "Pipeline templates"
	lib.Description = "Shared domain types"
	fork := f.project(4, "acme/app-fork")
	fork.ForkedFromProject = &apiForkParent{PathWithNamespace: "acme/app"}
	f.projects["acme"] = []apiProject{appP, ci, lib, fork}
	// A project of another namespace, shared with the group. It is not part
	// of this namespace and must never show up in its index.
	f.shared["acme"] = []apiProject{f.project(99, "partner/tool")}
	return &linkedFixture{f: f, app: app}
}

// readIndex decodes a workspace's index file.
func readIndex(t *testing.T, ws string) contract.Index {
	t.Helper()
	var idx contract.Index
	if err := json.Unmarshal([]byte(readFileT(t, indexPath(ws))), &idx); err != nil {
		t.Fatalf("decoding the index: %v", err)
	}
	return idx
}

// mustGitty runs gitty and fails the test unless it exits 0.
func mustGitty(t *testing.T, dir string, env []string, args ...string) (string, string) {
	t.Helper()
	stdout, stderr, code := runGitty(t, dir, env, args...)
	if code != 0 {
		t.Fatalf("gitty %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout, stderr
}

func TestE2EIndexAndRelated(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	f := fx.f
	ws := initWorkspace(t, f)

	for _, p := range []string{"app", "app-fork", "ci-templates", "lib"} {
		gitRun(t, ws, "clone", "-q", filepath.Join(f.gitRoot, "acme", p+".git"), filepath.Join("acme", p))
	}

	t.Log("index: a first run reads every checkout")
	stdout, _ := mustGitty(t, ws, nil, "index", "--anon")
	assertContextEvents(t, stdout)
	wantLines(t, "first index", stdout,
		"index acme/app new",
		"link acme/app ci-include acme/ci-templates .gitlab-ci.yml:2",
		"link acme/app depends acme/lib package.json:4",
		"unresolved acme/app acme/old-lib package.json:5",
		"index acme/app-fork new",
		"index acme/ci-templates new",
		"index acme/lib new",
		// Where a checkout was forked from is not in its files: the API says.
		"link acme/app-fork fork acme/app api:forked_from_project",
		"summary projects=4 checkouts=4 scanned=4 links=3 unresolved=1 errors=0",
	)

	idx := readIndex(t, ws)
	if idx.Version != contract.IndexVersion || idx.Instance != f.srv.URL || idx.Root != "" {
		t.Errorf("index header = version %d, instance %q, root %q", idx.Version, idx.Instance, idx.Root)
	}
	app := idx.Projects["acme/app"]
	if app == nil || app.ID != 1 || app.Description != "Customer-facing API" || len(app.Topics) != 2 ||
		app.DefaultBranch != "main" {
		t.Errorf("acme/app metadata = %+v", app)
	}
	if idx.Registry != fakeRegistryHost {
		t.Errorf("registry = %q, want %q", idx.Registry, fakeRegistryHost)
	}
	if _, ok := idx.Projects["partner/tool"]; ok {
		t.Error("a project merely shared with the group was indexed as part of its namespace")
	}
	if app.Local == nil || app.Local.Dir != "acme/app" || len(app.Local.Head) != 40 || app.Local.Summary != "Customer-facing API." {
		t.Errorf("acme/app local = %+v", app.Local)
	}
	lib := idx.Projects["acme/lib"]
	if lib == nil || lib.Local == nil || strings.Join(lib.Local.Docs, ",") != "AGENTS.md,README.md" {
		t.Errorf("acme/lib docs = %+v, want agent instructions first, README last, no changelog", lib)
	}
	if got := idx.Projects["acme/ci-templates"].Local.Summary; got != "Shared pipeline templates for every acme project." {
		t.Errorf("ci-templates summary = %q, want the first prose paragraph", got)
	}
	if fork := idx.Projects["acme/app-fork"]; fork == nil || fork.Local == nil || fork.ForkedFrom != "acme/app" {
		t.Errorf("acme/app-fork = %+v, want its fork origin looked up", fork)
	}

	t.Log("index: nothing moved, so nothing is read again")
	stdout, _ = mustGitty(t, ws, nil, "index", "--anon")
	wantLines(t, "second index", stdout,
		"index acme/app unchanged",
		"index acme/app-fork unchanged",
		"index acme/ci-templates unchanged",
		"index acme/lib unchanged",
		"summary projects=4 checkouts=4 scanned=0 links=3 unresolved=1 errors=0",
	)

	t.Log("related: answered from the index, with no token and no instance")
	f.requireToken = "nobody-has-this" // any API call from here on fails
	stdout, _ = mustGitty(t, ws, nil, "related", "acme/app")
	assertContextEvents(t, stdout)
	wantLines(t, "related acme/app", stdout,
		"related acme/app ci-include acme/ci-templates .gitlab-ci.yml:2",
		"related acme/app depends acme/lib package.json:4",
		"unresolved acme/app acme/old-lib package.json:5",
		"related-by acme/app fork acme/app-fork api:forked_from_project",
		"summary uses=2 used-by=1 unresolved=1",
	)

	// From inside a checkout, the subject is that project.
	stdout, _ = mustGitty(t, filepath.Join(ws, "acme", "lib"), nil, "related")
	wantLines(t, "related (inside lib)", stdout,
		"related-by acme/lib depends acme/app package.json:4",
		"summary uses=0 used-by=1 unresolved=0",
	)
	// A directory names the project checked out there; so does a path
	// relative to the current group.
	for _, arg := range []string{"../app", "app", "/acme/app"} {
		stdout, _ = mustGitty(t, filepath.Join(ws, "acme"), nil, "related", arg, "--direction=uses")
		wantLines(t, "related "+arg+" --direction=uses", stdout,
			"related acme/app ci-include acme/ci-templates .gitlab-ci.yml:2",
			"related acme/app depends acme/lib package.json:4",
			"unresolved acme/app acme/old-lib package.json:5",
			"summary uses=2 used-by=0 unresolved=1",
		)
	}
	stdout, _ = mustGitty(t, ws, nil, "related", "acme/app", "--direction=used-by")
	wantLines(t, "related --direction=used-by", stdout,
		"related-by acme/app fork acme/app-fork api:forked_from_project",
		"summary uses=0 used-by=1 unresolved=0",
	)
	// A group: links among its projects, and none crossing its edge here.
	stdout, _ = mustGitty(t, filepath.Join(ws, "acme"), nil, "related")
	wantLines(t, "related (group)", stdout,
		"related acme/app ci-include acme/ci-templates .gitlab-ci.yml:2",
		"related acme/app depends acme/lib package.json:4",
		"related acme/app-fork fork acme/app api:forked_from_project",
		"unresolved acme/app acme/old-lib package.json:5",
		"summary uses=3 used-by=0 unresolved=1",
	)

	_, stderr, code := runGitty(t, ws, nil, "related", "acme/nope")
	if code != 2 || !strings.Contains(stderr, "acme/nope") {
		t.Errorf("unknown subject: exit %d, stderr:\n%s", code, stderr)
	}
	_, stderr, code = runGitty(t, ws, nil, "related", "--direction=sideways")
	if code != 2 || !strings.Contains(stderr, "--direction") {
		t.Errorf("bad direction: exit %d, stderr:\n%s", code, stderr)
	}

	t.Log("an uncommitted edit to the project being asked about is seen")
	appDir := filepath.Join(ws, "acme", "app")
	ciWithTrigger := appCI + "\ndeploy:\n  trigger:\n    project: acme/lib\n"
	if err := os.WriteFile(filepath.Join(appDir, ".gitlab-ci.yml"), []byte(ciWithTrigger), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _ = mustGitty(t, filepath.Join(appDir, "src"), nil, "related", "--direction=uses")
	if !strings.Contains(stdout, "related acme/app ci-trigger acme/lib .gitlab-ci.yml:11\n") {
		t.Errorf("uncommitted trigger not found:\n%s", stdout)
	}

	t.Log("index: a moved commit is read again; --rescan reads everything")
	f.requireToken = ""
	if err := os.WriteFile(filepath.Join(appDir, "package.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, appDir, "commit", "-q", "-am", "drop the dependencies")
	stdout, _ = mustGitty(t, ws, nil, "index", "--anon")
	wantLines(t, "index after a commit", stdout,
		"index acme/app changed",
		"link acme/app ci-include acme/ci-templates .gitlab-ci.yml:2",
		"link acme/app ci-trigger acme/lib .gitlab-ci.yml:11",
		"index acme/app-fork unchanged",
		"index acme/ci-templates unchanged",
		"index acme/lib unchanged",
		"summary projects=4 checkouts=4 scanned=1 links=3 unresolved=0 errors=0",
	)
	stdout, _ = mustGitty(t, ws, nil, "index", "--anon", "--rescan")
	if !strings.Contains(stdout, "scanned=4 ") || strings.Contains(stdout, " changed\n") {
		t.Errorf("--rescan should read all four and find nothing new:\n%s", stdout)
	}

	t.Log("index --dry-run reports the same and writes nothing")
	if err := os.Remove(indexPath(ws)); err != nil {
		t.Fatal(err)
	}
	stdout, _ = mustGitty(t, ws, nil, "index", "--anon", "--dry-run")
	assertContextEvents(t, stdout)
	for _, want := range []string{
		"plan index acme/app new\n",
		"link acme/app ci-include acme/ci-templates .gitlab-ci.yml:2\n",
		"summary projects=4 checkouts=4 scanned=4 links=3 unresolved=0 errors=0\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry run stdout missing %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(indexPath(ws)); !os.IsNotExist(err) {
		t.Errorf("--dry-run wrote the index (stat err = %v)", err)
	}
	entries, err := os.ReadDir(filepath.Join(ws, ConfigDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ConfigName {
		t.Errorf("--dry-run left files in .gitty: %v", entries)
	}

	t.Log("an excluded project is neither read nor linked to")
	cfgPath := filepath.Join(ws, ConfigDir, ConfigName)
	if err := os.WriteFile(cfgPath, []byte(readFileT(t, cfgPath)+"exclude = [\"acme/lib\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _ = mustGitty(t, ws, nil, "index", "--anon")
	wantLines(t, "index with an exclusion", stdout,
		"skip acme/lib excluded",
		"index acme/app new",
		"link acme/app ci-include acme/ci-templates .gitlab-ci.yml:2",
		"index acme/app-fork new",
		"index acme/ci-templates new",
		"link acme/app-fork fork acme/app api:forked_from_project",
		"summary projects=4 checkouts=3 scanned=3 links=2 unresolved=0 errors=0",
	)
	// Lifting the exclusion for one run brings the link back: the index
	// follows the exclude list in effect.
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(readFileT(t, cfgPath), "exclude = [\"acme/lib\"]\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _ = mustGitty(t, ws, nil, "related", "acme/lib", "--anon")
	wantLines(t, "related after lifting the exclusion", stdout,
		"related-by acme/lib ci-trigger acme/app .gitlab-ci.yml:11",
		"summary uses=0 used-by=1 unresolved=0",
	)
	stdout, _ = mustGitty(t, ws, nil, "index", "--anon", "--exclude=acme/ci-*")
	if !strings.Contains(stdout, "skip acme/ci-templates excluded\n") || strings.Contains(stdout, "acme/ci-templates .gitlab-ci.yml") {
		t.Errorf("--exclude glob not honoured:\n%s", stdout)
	}
}

// TestE2EIndexNeedsTheInstanceOnce: with no index there is nothing to answer
// from, and building one needs a credential or --anon — said plainly, as a
// usage error, never papered over with an empty answer.
func TestE2EIndexNeedsTheInstanceOnce(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	ws := initWorkspace(t, fx.f)
	gitRun(t, ws, "clone", "-q", filepath.Join(fx.f.gitRoot, "acme", "app.git"), filepath.Join("acme", "app"))

	stdout, stderr, code := runGitty(t, ws, nil, "related", "acme/app")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--anon") || !strings.Contains(stderr, "index") {
		t.Errorf("no index, no credential: exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
	stdout, stderr, code = runGitty(t, ws, nil, "index")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--anon") {
		t.Errorf("index without a credential: exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}

	// `related` builds the index by itself when it may reach the instance.
	stdout, _ = mustGitty(t, ws, nil, "related", "acme/app", "--anon", "--direction=uses")
	if !strings.Contains(stdout, "related acme/app ci-include acme/ci-templates .gitlab-ci.yml:2\n") {
		t.Errorf("related did not build the index:\n%s", stdout)
	}
	if _, err := os.Stat(indexPath(ws)); err != nil {
		t.Errorf("index not written: %v", err)
	}
}

// TestE2EIndexTokenNeverStored: the index is built with a token and holds
// none.
func TestE2EIndexTokenNeverStored(t *testing.T) {
	skipIfShort(t)
	const token = "glpat-index-sekret"
	fx := newLinkedFixture(t)
	fx.f.requireToken = token
	ws := initWorkspace(t, fx.f)
	gitRun(t, ws, "clone", "-q", filepath.Join(fx.f.gitRoot, "acme", "app.git"), filepath.Join("acme", "app"))

	stdout, stderr := mustGitty(t, ws, []string{"GITLAB_TOKEN=" + token}, "index", "--verbose")
	if !strings.Contains(stdout, "index acme/app new\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	assertNoTokenAnywhere(t, token, filepath.Join(ws, ConfigDir), stdout, stderr)
}

// TestE2EIndexIsOnePerWorkspace: `sync --groups` leaves a config in every
// group directory, but the index is kept once, at the top, and sees links
// between groups whichever directory a command is started in.
func TestE2EIndexIsOnePerWorkspace(t *testing.T) {
	skipIfShort(t)
	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.groups["acme/team"] = apiGroup{ID: 2, FullPath: "acme/team"}
	f.subgroups["acme"] = []apiGroup{f.groups["acme/team"]}
	f.descendants["acme"] = []apiGroup{f.groups["acme/team"]}
	f.addRepo(t, "acme/lib", map[string]string{"README.md": "# lib\n"})
	f.addRepo(t, "acme/team/svc", map[string]string{
		"go.mod": "module " + strings.TrimPrefix(f.srv.URL, "http://") + "/acme/team/svc\n\nrequire " +
			strings.TrimPrefix(f.srv.URL, "http://") + "/acme/lib/v2 v2.1.0\n",
	})
	f.projects["acme"] = []apiProject{f.project(1, "acme/lib")}
	f.projects["acme/team"] = []apiProject{f.project(2, "acme/team/svc")}

	// A workspace rooted at the group itself, as a team would set one up.
	ws := t.TempDir()
	writeConfigAt(t, ws, Config{URL: f.srv.URL, HTTP: true, RootPath: "acme"})
	mustGitty(t, ws, nil, "sync", "--groups", "--repos", "--nested", "--anon")
	if _, err := os.Stat(filepath.Join(ws, "team", ConfigDir, ConfigName)); err != nil {
		t.Fatalf("expected a config in the subgroup directory: %v", err)
	}

	// Started deep inside a checkout in the subgroup.
	svc := filepath.Join(ws, "team", "svc")
	stdout, _ := mustGitty(t, svc, nil, "index", "--anon")
	wantLines(t, "index from a subgroup checkout", stdout,
		"index acme/lib new",
		"index acme/team/svc new",
		"link acme/team/svc depends acme/lib go.mod:3",
		"summary projects=2 checkouts=2 scanned=2 links=1 unresolved=0 errors=0",
	)
	if _, err := os.Stat(indexPath(ws)); err != nil {
		t.Errorf("the index belongs at the workspace root: %v", err)
	}
	if _, err := os.Stat(indexPath(filepath.Join(ws, "team"))); !os.IsNotExist(err) {
		t.Errorf("a second index was written in the subgroup directory (stat err = %v)", err)
	}
	idx := readIndex(t, ws)
	if idx.Root != "acme" || idx.Projects["acme/team/svc"].Local.Dir != "team/svc" {
		t.Errorf("index root %q, svc dir %q", idx.Root, idx.Projects["acme/team/svc"].Local.Dir)
	}

	stdout, _ = mustGitty(t, svc, nil, "related")
	wantLines(t, "related from the subgroup checkout", stdout,
		"related acme/team/svc depends acme/lib go.mod:3",
		"summary uses=1 used-by=0 unresolved=0",
	)
	// In the subgroup's directory the subject is the subgroup: what leaves it.
	stdout, _ = mustGitty(t, filepath.Join(ws, "team"), nil, "related")
	wantLines(t, "related for the subgroup", stdout,
		"related acme/team/svc depends acme/lib go.mod:3",
		"summary uses=1 used-by=0 unresolved=0",
	)
	stdout, _ = mustGitty(t, filepath.Join(ws, "team"), nil, "related", "/acme/lib")
	wantLines(t, "related /acme/lib from the subgroup", stdout,
		"related-by acme/lib depends acme/team/svc go.mod:3",
		"summary uses=0 used-by=1 unresolved=0",
	)
}
