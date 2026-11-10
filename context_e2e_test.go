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

// cloneInto checks a fixture project out by hand, where a sync would put it.
func cloneInto(t *testing.T, f *fakeGitLab, ws string, projects ...string) {
	t.Helper()
	for _, p := range projects {
		gitRun(t, ws, "clone", "-q", filepath.Join(f.gitRoot, filepath.FromSlash(p)+".git"), filepath.FromSlash(p))
	}
}

func TestE2EContext(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	f := fx.f
	ws := initWorkspace(t, f)
	cloneInto(t, f, ws, "acme/app")
	appDir := filepath.Join(ws, "acme", "app")

	t.Log("without --fetch: absent projects are named with the command that fetches them")
	stdout, _ := mustGitty(t, appDir, nil, "context", "--anon")
	wantLines(t, "context", stdout,
		"# acme/app — Customer-facing API",
		"127.0.0.1 · `.` · main · topics: go, api · docs: README.md",
		"",
		"Customer-facing API.",
		"",
		"## Uses (2)",
		"- **acme/ci-templates** — Pipeline templates · *not checked out* (`gitty fetch acme/ci-templates`) · ci-include templates/build.yml (.gitlab-ci.yml:2)",
		"- **acme/lib** — Shared domain types · *not checked out* (`gitty fetch acme/lib`) · depends (package.json:4)",
		"",
		// An empty side is said to be empty, and the blind spot is named.
		"## Used by (0)",
		"- No checked-out project refers to it.",
		"",
		"## References that are not projects here (1)",
		"- acme/old-lib (package.json:5)",
		"",
		"## Notes",
		"- Only checked-out projects are read for links: 1 of 4 here. \"Used by\" may be incomplete.",
	)
	for _, p := range []string{"ci-templates", "lib"} {
		if _, err := os.Stat(filepath.Join(ws, "acme", p)); !os.IsNotExist(err) {
			t.Errorf("context without --fetch created acme/%s", p)
		}
	}

	t.Log("--fetch clones what the project uses, where a sync would put it")
	stdout, stderr := mustGitty(t, filepath.Join(appDir, "src"), nil, "context", "--fetch", "--anon")
	wantLines(t, "context --fetch", stdout,
		"# acme/app — Customer-facing API",
		"127.0.0.1 · `..` · main · topics: go, api · docs: README.md",
		"",
		"Customer-facing API.",
		"",
		"## Uses (2)",
		"- **acme/ci-templates** — Pipeline templates · `../../ci-templates` (fetched) · ci-include templates/build.yml (.gitlab-ci.yml:2)",
		"- **acme/lib** — Shared domain types · `../../lib` (fetched) · depends (package.json:4)",
		"",
		"## Used by (0)",
		"- No checked-out project refers to it.",
		"",
		"## References that are not projects here (1)",
		"- acme/old-lib (package.json:5)",
		"",
		"## Where to read next",
		"- acme/ci-templates: ../../ci-templates/templates/build.yml, ../../ci-templates/README.md",
		"- acme/lib: ../../lib/AGENTS.md, ../../lib/README.md",
		"",
		"## Notes",
		"- Only checked-out projects are read for links: 3 of 4 here. \"Used by\" may be incomplete.",
	)
	// What fetching did is reported with the diagnostics, not in the briefing.
	for _, want := range []string{"clone acme/ci-templates\n", "clone acme/lib\n"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "lib", "lib.txt")); got != "needle in the library\n" {
		t.Errorf("fetched lib.txt = %q", got)
	}
	// Exactly what was named: the fork is related to nothing the app uses.
	if _, err := os.Stat(filepath.Join(ws, "acme", "app-fork")); !os.IsNotExist(err) {
		t.Error("context --fetch cloned a project the subject does not use")
	}

	t.Log("a second --fetch finds everything in place and touches nothing")
	hits := f.gitHits.Load()
	f.requireToken = "nobody-has-this" // and needs no instance either
	stdout, _ = mustGitty(t, appDir, nil, "context", "--fetch")
	if strings.Contains(stdout, "(fetched)") || !strings.Contains(stdout, "· `../lib` · depends (package.json:4)") {
		t.Errorf("second --fetch:\n%s", stdout)
	}
	if f.gitHits.Load() != hits {
		t.Errorf("second --fetch contacted the git server (%d requests)", f.gitHits.Load()-hits)
	}

	t.Log("the other side: what uses a project")
	stdout, _ = mustGitty(t, filepath.Join(ws, "acme", "lib"), nil, "context")
	wantLines(t, "context (lib)", stdout,
		"# acme/lib — Shared domain types",
		"127.0.0.1 · `.` · main · docs: AGENTS.md, README.md",
		"",
		"Shared domain types.",
		"",
		"## Uses (0)",
		"- Nothing in its pipeline configuration or manifests refers to another project of the instance.",
		"",
		"## Used by (1)",
		"- **acme/app** — Customer-facing API · `../app` · depends (package.json:4)",
		"",
		"## Where to read next",
		"- acme/app: ../app/README.md",
		"",
		"## Notes",
		"- Only checked-out projects are read for links: 3 of 4 here. \"Used by\" may be incomplete.",
	)
	stdout, _ = mustGitty(t, ws, nil, "context", "acme/lib", "--direction=uses")
	if strings.Contains(stdout, "Used by") || !strings.Contains(stdout, "## Uses (0)\n") {
		t.Errorf("--direction=uses should print that side and nothing about the other:\n%s", stdout)
	}

	t.Log("--format=json is the same briefing as data")
	stdout, _ = mustGitty(t, appDir, nil, "context", "--format=json")
	var b contract.Briefing
	if err := json.Unmarshal([]byte(stdout), &b); err != nil {
		t.Fatalf("decoding the briefing: %v\n%s", err, stdout)
	}
	if b.Schema != contract.Schema || b.Instance != f.srv.URL || b.Workspace != realDir(t, ws) && b.Workspace != ws {
		t.Errorf("header: schema %d, instance %q, workspace %q", b.Schema, b.Instance, b.Workspace)
	}
	if b.Subject.Kind != "project" || b.Subject.Project != "acme/app" || !b.Subject.Present ||
		b.Subject.RelativePath != "." || !filepath.IsAbs(b.Subject.Path) || b.Subject.DefaultBranch != "main" {
		t.Errorf("subject = %+v", b.Subject)
	}
	if b.UsesTotal != 2 || len(b.Uses) != 2 || b.UsedByTotal != 0 || b.UsedBy == nil {
		t.Fatalf("uses %d/%d, used by %d (%v)", len(b.Uses), b.UsesTotal, b.UsedByTotal, b.UsedBy)
	}
	lib := b.Uses[1]
	if lib.Project != "acme/lib" || !lib.Present || lib.Fetched || lib.RelativePath != "../lib" ||
		lib.Description != "Shared domain types" || strings.Join(lib.Docs, ",") != "AGENTS.md,README.md" ||
		len(lib.Links) != 1 || lib.Links[0].Kind != contract.KindDepends || lib.Links[0].Evidence != "package.json:4" ||
		lib.Links[0].From != "acme/app" || lib.FetchCommand != "" {
		t.Errorf("acme/lib = %+v", lib)
	}
	if got := readFileT(t, filepath.Join(lib.Path, "lib.txt")); got != "needle in the library\n" {
		t.Errorf("the path a briefing gives must lead to the project: %q", got)
	}
	if len(b.Unresolved) != 1 || b.Unresolved[0].Ref != "acme/old-lib" {
		t.Errorf("unresolved = %+v", b.Unresolved)
	}

	t.Log("a group: its projects, the links among them, and what crosses its edge")
	stdout, _ = mustGitty(t, filepath.Join(ws, "acme"), nil, "context")
	for _, want := range []string{
		"# acme (group)\n",
		"127.0.0.1 · `.` · 4 projects, 3 checked out\n",
		"## Projects (4)\n",
		"- **acme/app** — Customer-facing API · `app`\n",
		"- **acme/app-fork** · *not checked out*\n",
		"## Links within the group (2)\n",
		"- acme/app → acme/ci-templates · ci-include templates/build.yml (.gitlab-ci.yml:2)\n",
		"- acme/app → acme/lib · depends (package.json:4)\n",
		"## References that are not projects here (1)\n- acme/old-lib (acme/app: package.json:5)\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("group briefing missing %q:\n%s", want, stdout)
		}
	}

	t.Log("a fetch that fails leaves the project in the briefing, absent, with the reason")
	f.requireToken = ""
	if err := os.RemoveAll(filepath.Join(ws, "acme", "lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.gitRoot, "acme", "lib.git"), filepath.Join(f.gitRoot, "acme", "lib.gone")); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runGitty(t, appDir, nil, "context", "--fetch", "--anon")
	if code != 1 {
		t.Errorf("exit = %d, want 1: the briefing is complete but not everything asked for was done\n%s", code, stderr)
	}
	for _, want := range []string{
		"- **acme/ci-templates** — Pipeline templates · `../ci-templates` · ci-include templates/build.yml (.gitlab-ci.yml:2)\n",
		"- **acme/lib** — Shared domain types · *not checked out* (`gitty fetch acme/lib`) · depends (package.json:4)\n  - fetching it failed: git fetch failed\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("briefing after a failed fetch missing %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "acme", "lib")); !os.IsNotExist(err) {
		t.Error("a failed fetch left a half-made checkout behind")
	}

	t.Log("no credential: the briefing still answers, and says what it could not do")
	stdout, _, code = runGitty(t, appDir, nil, "context", "--fetch", "--format=json")
	if err := json.Unmarshal([]byte(stdout), &b); err != nil {
		t.Fatalf("decoding the briefing: %v\n%s", err, stdout)
	}
	if code != 1 || len(b.Uses) != 2 || b.Uses[1].Present || !strings.Contains(b.Uses[1].FetchError, "--anon") {
		t.Errorf("exit %d, acme/lib = %+v", code, b.Uses[1])
	}

	_, stderr, code = runGitty(t, appDir, nil, "context", "--format=yaml")
	if code != 2 || !strings.Contains(stderr, "--format") {
		t.Errorf("bad format: exit %d\n%s", code, stderr)
	}
	_, stderr, code = runGitty(t, appDir, nil, "context", "one", "two")
	if code != 2 || !strings.Contains(stderr, "at most one") {
		t.Errorf("two subjects: exit %d\n%s", code, stderr)
	}
}

func TestE2EFetch(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	f := fx.f
	ws := initWorkspace(t, f)
	cloneInto(t, f, ws, "acme/app")
	appSrc := filepath.Join(ws, "acme", "app", "src")

	t.Log("an absent project is cloned where a sync would put it")
	stdout, _ := mustGitty(t, appSrc, nil, "fetch", "acme/lib", "--anon")
	assertContextEvents(t, stdout)
	wantLines(t, "fetch acme/lib", stdout,
		"clone acme/lib",
		"summary cloned=1 present=0 errors=0",
	)
	if got := readFileT(t, filepath.Join(ws, "acme", "lib", "lib.txt")); got != "needle in the library\n" {
		t.Errorf("lib.txt = %q", got)
	}

	t.Log("a present project is left exactly as it is; the report says which is which")
	// A local commit a pull would have to merge with: fetch must not touch it.
	libDir := filepath.Join(ws, "acme", "lib")
	if err := os.WriteFile(filepath.Join(libDir, "local.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	head := gitOut(t, libDir, "rev-parse", "HEAD")
	hits := f.gitHits.Load()
	stdout, stderr := mustGitty(t, ws, nil, "fetch", "/acme/lib/", "acme/ci-templates.git", "--anon", "--format=json")
	var report contract.FetchReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout must be the report and nothing else: %v\n%s", err, stdout)
	}
	if report.Schema != contract.Schema || len(report.Results) != 2 {
		t.Fatalf("report = %+v", report)
	}
	lib, ci := report.Results[0], report.Results[1]
	if lib.Project != "acme/lib" || !lib.Present || lib.Fetched || lib.Error != "" || lib.Path != filepath.Join(realDir(t, ws), "acme", "lib") && lib.Path != libDir {
		t.Errorf("acme/lib = %+v", lib)
	}
	if ci.Project != "acme/ci-templates" || !ci.Present || !ci.Fetched || ci.Error != "" {
		t.Errorf("acme/ci-templates = %+v", ci)
	}
	if !strings.Contains(stderr, "skip acme/lib present\n") || !strings.Contains(stderr, "clone acme/ci-templates\n") {
		t.Errorf("with --format=json the events belong on stderr:\n%s", stderr)
	}
	if gitOut(t, libDir, "rev-parse", "HEAD") != head || readFileT(t, filepath.Join(libDir, "local.txt")) != "mine" {
		t.Error("fetch changed a checkout that was already present")
	}
	_ = hits
	// Nothing that was not named.
	if _, err := os.Stat(filepath.Join(ws, "acme", "app-fork")); !os.IsNotExist(err) {
		t.Error("fetch cloned a project that was not named")
	}

	t.Log("--dry-run plans the same and clones nothing")
	stdout, _ = mustGitty(t, ws, nil, "fetch", "acme/app-fork", "acme/lib", "--anon", "--dry-run")
	wantLines(t, "fetch --dry-run", stdout,
		"skip acme/lib present",
		"plan clone acme/app-fork",
		"summary cloned=1 present=1 errors=0",
	)
	if _, err := os.Stat(filepath.Join(ws, "acme", "app-fork")); !os.IsNotExist(err) {
		t.Error("--dry-run cloned")
	}

	t.Log("what cannot be fetched is an error for that project, not for the others")
	stdout, stderr, code := runGitty(t, ws, nil, "fetch", "acme/nope", "acme/app-fork", "--anon")
	if code != 1 {
		t.Errorf("exit = %d, want 1\n%s", code, stderr)
	}
	wantLines(t, "fetch with one unknown project", stdout,
		"error acme/nope looking the project up failed",
		"clone acme/app-fork",
		"summary cloned=1 present=0 errors=1",
	)
	if !strings.Contains(stderr, "no such project on the instance") || !strings.Contains(stderr, "1 failed:") {
		t.Errorf("stderr should say why and carry the recap:\n%s", stderr)
	}

	t.Log("the exclude list is honoured even for a project named outright")
	stdout, _, code = runGitty(t, ws, nil, "fetch", "acme/ci-templates", "--anon", "--exclude=acme/ci-*")
	if code != 1 || !strings.Contains(stdout, "skip acme/ci-templates excluded\n") {
		t.Errorf("exit %d:\n%s", code, stdout)
	}

	t.Log("usage errors")
	for name, args := range map[string][]string{
		"no project":           {"fetch", "--anon"},
		"not a namespace path": {"fetch", "lib", "--anon"},
		"a path that climbs":   {"fetch", "acme/../../etc", "--anon"},
		"bad format":           {"fetch", "acme/lib", "--anon", "--format=xml"},
	} {
		if _, stderr, code := runGitty(t, ws, nil, args...); code != 2 {
			t.Errorf("%s: exit = %d, want 2\n%s", name, code, stderr)
		}
	}
	// No credential, and something to clone: said once, before anything.
	if err := os.RemoveAll(filepath.Join(ws, "acme", "app-fork")); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runGitty(t, ws, nil, "fetch", "acme/app-fork")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--anon") {
		t.Errorf("no credential: exit %d, stdout %q\n%s", code, stdout, stderr)
	}
	// Everything named already here: nothing to ask the instance for.
	stdout, _ = mustGitty(t, ws, nil, "fetch", "acme/lib")
	wantLines(t, "fetch of a present project, no credential", stdout,
		"skip acme/lib present",
		"summary cloned=0 present=1 errors=0",
	)
}

// TestE2EFetchStaysInsideTheWorkspace: a workspace rooted at a group has no
// place for a project of another namespace, and fetch does not invent one.
func TestE2EFetchStaysInsideTheWorkspace(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	ws := t.TempDir()
	writeConfigAt(t, ws, Config{URL: fx.f.srv.URL, HTTP: true, RootPath: "acme"})

	stdout, _, code := runGitty(t, ws, nil, "fetch", "partner/tool", "acme/lib", "--anon")
	if code != 1 || !strings.Contains(stdout, "error partner/tool outside this workspace\n") || !strings.Contains(stdout, "clone acme/lib\n") {
		t.Errorf("exit %d:\n%s", code, stdout)
	}
	if got := readFileT(t, filepath.Join(ws, "lib", "lib.txt")); got != "needle in the library\n" {
		t.Errorf("lib.txt = %q", got)
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 { // .gitty and lib
		t.Errorf("workspace root holds %v, want only .gitty and lib", entries)
	}
}

// TestE2EChangeDirectory: `gitty -C <dir> <command>` behaves as if started in
// <dir> — the way something that cannot choose its own working directory
// says which workspace, and which project in it, it means.
func TestE2EChangeDirectory(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	ws := initWorkspace(t, fx.f)
	cloneInto(t, fx.f, ws, "acme/app", "acme/lib")
	mustGitty(t, ws, nil, "index", "--anon")
	elsewhere := t.TempDir()

	stdout, _ := mustGitty(t, elsewhere, nil, "-C", filepath.Join(ws, "acme", "lib"), "related")
	wantLines(t, "gitty -C <checkout> related", stdout,
		"related-by acme/lib depends acme/app package.json:4",
		"summary uses=0 used-by=1 unresolved=0",
	)
	// Several are applied in order, each relative to the one before.
	stdout, _ = mustGitty(t, elsewhere, nil, "-C", ws, "-C", "acme/app", "related", "--direction=uses")
	if !strings.Contains(stdout, "related acme/app depends acme/lib package.json:4\n") {
		t.Errorf("gitty -C a -C b:\n%s", stdout)
	}
	// A plugin started this way starts there too, and is told so.
	pathDir := filepath.Join(elsewhere, "bin")
	writeStub(t, pathDir, "gitty-where", `echo "pwd=$(pwd)"; echo "group=$GITTY_GROUP"`)
	stdout, _ = mustGitty(t, elsewhere, []string{"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")},
		"-C", filepath.Join(ws, "acme", "lib"), "where")
	if !strings.Contains(stdout, "group=acme\n") || !strings.Contains(stdout, filepath.Join("acme", "lib")+"\n") {
		t.Errorf("plugin after -C:\n%s", stdout)
	}

	for name, args := range map[string][]string{
		"no directory":       {"-C"},
		"not a directory":    {"-C", filepath.Join(elsewhere, "missing"), "related"},
		"nothing after -C x": {"-C", ws},
	} {
		if _, stderr, code := runGitty(t, elsewhere, nil, args...); code != 2 {
			t.Errorf("%s: exit = %d, want 2\n%s", name, code, stderr)
		}
	}
}

// TestE2EContextReachesOtherGroups: a project in a group this workspace has
// never cloned or listed is still a project. A pipeline that includes from
// one gets it as a link — and, with --fetch, as a checkout — not as "a
// reference that is not a project here".
func TestE2EContextReachesOtherGroups(t *testing.T) {
	skipIfShort(t)
	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.groups["platform"] = apiGroup{ID: 2, FullPath: "platform"}
	f.addRepo(t, "acme/app", map[string]string{
		".gitlab-ci.yml": "include:\n  - project: platform/ci/templates\n    file: /build.yml\n  - project: platform/ci/gone\n    file: /x.yml\n",
	})
	f.addRepo(t, "platform/ci/templates", map[string]string{"README.md": "# templates\n\nPipeline templates for everyone.\n", "build.yml": "build:\n  script: [make]\n"})
	f.projects["acme"] = []apiProject{f.project(1, "acme/app")}
	tmpl := f.project(2, "platform/ci/templates")
	tmpl.Description = "Shared pipeline templates"
	f.projects["platform/ci"] = []apiProject{tmpl}

	ws := initWorkspace(t, f)
	cloneInto(t, f, ws, "acme/app")
	appDir := filepath.Join(ws, "acme", "app")

	// Only acme is listed: it is the only group the workspace holds. The
	// other project is found by asking for it.
	stdout, _ := mustGitty(t, ws, nil, "index", "--anon")
	wantLines(t, "index", stdout,
		"index acme/app new",
		"link acme/app ci-include platform/ci/templates .gitlab-ci.yml:2",
		"unresolved acme/app platform/ci/gone .gitlab-ci.yml:4",
		"summary projects=2 checkouts=1 scanned=1 links=1 unresolved=1 errors=0",
	)
	idx := readIndex(t, ws)
	if strings.Join(idx.Namespaces, ",") != "acme" || idx.Projects["platform/ci/templates"] == nil ||
		idx.Projects["platform/ci/templates"].Description != "Shared pipeline templates" ||
		strings.Join(idx.NotFound, ",") != "platform/ci/gone" {
		t.Errorf("index: namespaces %v, not found %v, projects %v", idx.Namespaces, idx.NotFound, idx.Projects)
	}

	stdout, stderr := mustGitty(t, appDir, nil, "context", "--fetch", "--anon")
	for _, want := range []string{
		"- **platform/ci/templates** — Shared pipeline templates · `../../platform/ci/templates` (fetched) · ci-include build.yml (.gitlab-ci.yml:2)\n",
		"## References that are not projects here (1)\n- platform/ci/gone (.gitlab-ci.yml:4)\n",
		"- platform/ci/templates: ../../platform/ci/templates/build.yml, ../../platform/ci/templates/README.md\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("briefing missing %q:\n%s\nstderr:\n%s", want, stdout, stderr)
		}
	}
	if got := readFileT(t, filepath.Join(ws, "platform", "ci", "templates", "build.yml")); !strings.Contains(got, "make") {
		t.Errorf("the included template was not fetched: %q", got)
	}

	// A reference written after the index was built, to a project the
	// listing never covered: --fetch asks about it then and there.
	f.addRepo(t, "platform/tools/lint", map[string]string{"README.md": "# lint\n"})
	f.projects["platform/tools"] = []apiProject{f.project(3, "platform/tools/lint")}
	ci := readFileT(t, filepath.Join(appDir, ".gitlab-ci.yml")) + "lint:\n  trigger:\n    project: platform/tools/lint\n"
	if err := os.WriteFile(filepath.Join(appDir, ".gitlab-ci.yml"), []byte(ci), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _ = mustGitty(t, appDir, nil, "context", "--anon")
	if !strings.Contains(stdout, "- platform/tools/lint (.gitlab-ci.yml:8)\n") {
		t.Errorf("without --fetch there is no asking, so it is unresolved for now:\n%s", stdout)
	}
	stdout, _ = mustGitty(t, appDir, nil, "context", "--fetch", "--anon")
	if !strings.Contains(stdout, "- **platform/tools/lint** · `../../platform/tools/lint` (fetched) · ci-trigger (.gitlab-ci.yml:8)\n") {
		t.Errorf("--fetch should have found and fetched it:\n%s", stdout)
	}
}

// TestE2ESyncRelated: `gitty sync --related` is a sync of the projects a
// project is linked to — the absent ones cloned, the present ones pulled, and
// no other project in the group touched.
func TestE2ESyncRelated(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	f := fx.f
	ws := initWorkspace(t, f)
	cloneInto(t, f, ws, "acme/app")
	appDir := filepath.Join(ws, "acme", "app")
	libDir := filepath.Join(ws, "acme", "lib")

	t.Log("--dry-run plans the sync and writes nothing, not even the index")
	stdout, _ := mustGitty(t, filepath.Join(appDir, "src"), nil, "sync", "--related", "--anon", "--dry-run", "--jobs=1")
	assertContextEvents(t, stdout)
	wantLines(t, "sync --related --dry-run", stdout,
		"plan clone acme/ci-templates",
		"plan clone acme/lib",
		"summary cloned=2 pulled=0 skipped=0 errors=0",
	)
	if _, err := os.Stat(libDir); !os.IsNotExist(err) {
		t.Error("--dry-run cloned")
	}
	if _, err := os.Stat(indexPath(ws)); !os.IsNotExist(err) {
		t.Error("--dry-run wrote the index")
	}

	t.Log("the real run does what the dry run said: what the project uses, and no sibling")
	stdout, stderr := mustGitty(t, filepath.Join(appDir, "src"), nil, "sync", "--related", "--anon", "--jobs=1")
	assertContextEvents(t, stdout)
	wantLines(t, "sync --related", stdout,
		"clone acme/ci-templates",
		"clone acme/lib",
		"summary cloned=2 pulled=0 skipped=0 errors=0",
	)
	if !strings.Contains(stderr, "Syncing projects related to acme/app (uses)") {
		t.Errorf("stderr should say what is being synced:\n%s", stderr)
	}
	if got := readFileT(t, filepath.Join(libDir, "lib.txt")); got != "needle in the library\n" {
		t.Errorf("lib.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "acme", "app-fork")); !os.IsNotExist(err) {
		t.Error("a project that is not related was cloned")
	}

	t.Log("a second run pulls: what is already there is brought up to date")
	work := t.TempDir()
	gitRun(t, work, "clone", "-q", filepath.Join(f.gitRoot, "acme", "lib.git"), ".")
	f.pushUpdate(t, work, "acme/lib", "lib.txt", "a newer needle\n")
	stdout, _ = mustGitty(t, appDir, nil, "sync", "--related", "--anon", "--jobs=1")
	wantLines(t, "sync --related, again", stdout,
		"pull acme/ci-templates",
		"pull acme/lib",
		"summary cloned=0 pulled=2 skipped=0 errors=0",
	)
	if got := readFileT(t, filepath.Join(libDir, "lib.txt")); got != "a newer needle\n" {
		t.Errorf("after the pull lib.txt = %q", got)
	}

	t.Log("--related=used-by is the other side; a subject can be named, by path or by directory")
	stdout, _ = mustGitty(t, ws, nil, "sync", "acme/lib", "--related=used-by", "--anon", "--jobs=1")
	wantLines(t, "sync acme/lib --related=used-by", stdout,
		"pull acme/app",
		"summary cloned=0 pulled=1 skipped=0 errors=0",
	)
	cloneInto(t, f, ws, "acme/app-fork")
	stdout, _ = mustGitty(t, ws, nil, "sync", "--related=both", "--anon", "--jobs=1", "--dry-run", filepath.Join("acme", "app"))
	wantLines(t, "sync --related=both --dry-run <dir>", stdout,
		"plan pull acme/app-fork",
		"plan pull acme/ci-templates",
		"plan pull acme/lib",
		"summary cloned=0 pulled=3 skipped=0 errors=0",
	)

	t.Log("--exclude leaves a related project out of this run")
	stdout, _ = mustGitty(t, appDir, nil, "sync", "--related", "--anon", "--jobs=1", "--exclude=acme/lib")
	wantLines(t, "sync --related --exclude", stdout,
		"skip acme/lib excluded",
		"pull acme/ci-templates",
		"summary cloned=0 pulled=1 skipped=1 errors=0",
	)
	if idx := readIndex(t, ws); len(idx.Exclude) != 0 {
		t.Errorf("a one-run --exclude was recorded in the index: %v", idx.Exclude)
	}

	t.Log("a pull that cannot fast-forward is that project's error, and the run's exit status")
	gitRun(t, libDir, "commit", "-q", "--allow-empty", "-m", "local work")
	gitRun(t, work, "pull", "-q", "origin", "main")
	f.pushUpdate(t, work, "acme/lib", "lib.txt", "the newest needle\n")
	stdout, stderr, code := runGitty(t, appDir, nil, "sync", "--related", "--anon", "--jobs=1")
	if code != 1 {
		t.Errorf("exit = %d, want 1\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "pull acme/ci-templates\n") || !strings.Contains(stdout, "error acme/lib ") ||
		!strings.Contains(stdout, "summary cloned=0 pulled=1 skipped=0 errors=1\n") {
		t.Errorf("sync --related with a diverged checkout:\n%s", stdout)
	}

	t.Log("what does not go with --related is refused before anything happens")
	for name, args := range map[string][]string{
		"an unknown direction": {"sync", "--related=sideways", "--anon"},
		"--groups":             {"sync", "--related", "--groups", "--anon"},
		"--nested":             {"sync", "--related", "--nested", "--anon"},
		"--prune":              {"sync", "--related", "--prune", "--anon"},
		"no credential":        {"sync", "--related"},
	} {
		if _, stderr, code := runGitty(t, appDir, []string{"GITLAB_TOKEN=", "CI_JOB_TOKEN="}, args...); code != 2 {
			t.Errorf("%s: exit = %d, want 2\n%s", name, code, stderr)
		}
	}
	// The direction is given after an equals sign; apart from it, it would be
	// read as the subject. The error says so.
	_, stderr, code = runGitty(t, appDir, nil, "sync", "--related", "used-by", "--anon")
	if code != 2 || !strings.Contains(stderr, "--related=used-by") {
		t.Errorf("sync --related used-by: exit %d\n%s", code, stderr)
	}
}

// TestE2ESyncRelatedInAGroup: in a group's directory the subject is every
// checkout of the group, and what one of them uses is synced even when it is
// itself a member of the group.
func TestE2ESyncRelatedInAGroup(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	ws := initWorkspace(t, fx.f)
	cloneInto(t, fx.f, ws, "acme/app")

	stdout, _ := mustGitty(t, filepath.Join(ws, "acme"), nil, "sync", "--related", "--anon", "--jobs=1")
	wantLines(t, "sync --related in a group", stdout,
		"clone acme/ci-templates",
		"clone acme/lib",
		"summary cloned=2 pulled=0 skipped=0 errors=0",
	)
}

// TestE2ESyncRelatedReachesOtherGroups: a link into a group the workspace has
// never listed is followed when the workspace has room for the project, and
// passed over — not failed on — when it is anchored at a group that does not.
func TestE2ESyncRelatedReachesOtherGroups(t *testing.T) {
	skipIfShort(t)
	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.addRepo(t, "acme/app", map[string]string{
		".gitlab-ci.yml": "include:\n  - project: platform/ci/templates\n    file: /build.yml\n  - project: platform/ci/gone\n    file: /x.yml\n",
	})
	f.addRepo(t, "platform/ci/templates", map[string]string{"build.yml": "build:\n  script: [make]\n"})
	f.projects["acme"] = []apiProject{f.project(1, "acme/app")}
	f.projects["platform/ci"] = []apiProject{f.project(2, "platform/ci/templates")}

	t.Log("a workspace of the whole instance has a place for it")
	ws := initWorkspace(t, f)
	cloneInto(t, f, ws, "acme/app")
	appDir := filepath.Join(ws, "acme", "app")
	stdout, _ := mustGitty(t, appDir, nil, "sync", "--related", "--anon", "--dry-run")
	wantLines(t, "sync --related --dry-run", stdout,
		"plan clone platform/ci/templates",
		"summary cloned=1 pulled=0 skipped=0 errors=0",
	)
	stdout, _ = mustGitty(t, appDir, nil, "sync", "--related", "--anon")
	wantLines(t, "sync --related", stdout,
		"clone platform/ci/templates",
		"summary cloned=1 pulled=0 skipped=0 errors=0",
	)
	if got := readFileT(t, filepath.Join(ws, "platform", "ci", "templates", "build.yml")); !strings.Contains(got, "make") {
		t.Errorf("the included template was not cloned: %q", got)
	}

	t.Log("a workspace of one group does not")
	ws = t.TempDir()
	writeConfigAt(t, ws, Config{URL: f.srv.URL, HTTP: true, RootPath: "acme"})
	gitRun(t, ws, "clone", "-q", filepath.Join(f.gitRoot, "acme", "app.git"), "app")
	stdout, _ = mustGitty(t, filepath.Join(ws, "app"), nil, "sync", "--related", "--anon")
	wantLines(t, "sync --related in a workspace of one group", stdout,
		"skip platform/ci/templates outside-workspace",
		"summary cloned=0 pulled=0 skipped=1 errors=0",
	)
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 { // .gitty and app
		t.Errorf("workspace root holds %v, want only .gitty and app", entries)
	}
}
