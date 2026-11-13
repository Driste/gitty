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

// cloneInto checks a fixture project out by hand, where a sync would put it.
func cloneInto(t *testing.T, f *fakeGitLab, ws string, projects ...string) {
	t.Helper()
	for _, p := range projects {
		gitRun(t, ws, "clone", "-q", filepath.Join(f.gitRoot, filepath.FromSlash(p)+".git"), filepath.FromSlash(p))
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

// TestE2EIndexIsBuiltWhenNeeded: nothing asks for the index to be built. The
// first command that needs it builds it — which takes the instance, and so a
// credential or --anon — and it is kept at the workspace root.
func TestE2EIndexIsBuiltWhenNeeded(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	ws := initWorkspace(t, fx.f)
	cloneInto(t, fx.f, ws, "acme/app")
	appDir := filepath.Join(ws, "acme", "app")

	stdout, stderr, code := runGitty(t, appDir, nil, "sync", "--related")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--anon") {
		t.Errorf("no index, no credential: exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
	if _, err := os.Stat(indexPath(ws)); !os.IsNotExist(err) {
		t.Errorf("a refused run left an index behind (stat err = %v)", err)
	}
	mustGitty(t, appDir, nil, "sync", "--related", "--anon")
	idx := readIndex(t, ws)
	if idx.Projects["acme/app"] == nil || idx.Projects["acme/app"].Local == nil || len(idx.Links) == 0 {
		t.Errorf("the index after the first run that needed it: %+v", idx.Projects["acme/app"])
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
	cloneInto(t, fx.f, ws, "acme/app")

	stdout, stderr := mustGitty(t, filepath.Join(ws, "acme", "app"), []string{"GITLAB_TOKEN=" + token}, "sync", "--related", "--verbose")
	if !strings.Contains(stdout, "clone acme/lib\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if _, err := os.Stat(indexPath(ws)); err != nil {
		t.Fatalf("no index was written: %v", err)
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
	stdout, _ := mustGitty(t, svc, nil, "sync", "--related", "--anon")
	wantLines(t, "sync --related from a subgroup checkout", stdout,
		"pull acme/lib",
		"summary cloned=0 pulled=1 skipped=0 errors=0",
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
	if got := linkSet(idx.Links); got != "depends acme/lib go.mod:3" {
		t.Errorf("links = %q", got)
	}

	// In the subgroup's directory the subject is the subgroup: what leaves it.
	stdout, _ = mustGitty(t, filepath.Join(ws, "team"), nil, "sync", "--related", "--anon", "--dry-run")
	wantLines(t, "sync --related for the subgroup", stdout,
		"plan pull acme/lib",
		"summary cloned=0 pulled=1 skipped=0 errors=0",
	)
	// A subject can be named from the instance's root, wherever one is.
	stdout, _ = mustGitty(t, filepath.Join(ws, "team"), nil, "sync", "/acme/lib", "--related=used-by", "--anon", "--dry-run")
	wantLines(t, "sync /acme/lib --related=used-by from the subgroup", stdout,
		"plan pull acme/team/svc",
		"summary cloned=0 pulled=1 skipped=0 errors=0",
	)
}

// TestE2EChangeDirectory: `gitty -C <dir> <command>` behaves as if started in
// <dir> — the way something that cannot choose its own working directory
// says which workspace, and which project in it, it means.
func TestE2EChangeDirectory(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	ws := initWorkspace(t, fx.f)
	cloneInto(t, fx.f, ws, "acme/app", "acme/lib")
	elsewhere := t.TempDir()

	stdout, _ := mustGitty(t, elsewhere, nil, "-C", filepath.Join(ws, "acme", "lib"), "sync", "--related=used-by", "--anon", "--dry-run")
	wantLines(t, "gitty -C <checkout> sync --related=used-by", stdout,
		"plan pull acme/app",
		"summary cloned=0 pulled=1 skipped=0 errors=0",
	)
	// Several are applied in order, each relative to the one before.
	stdout, _ = mustGitty(t, elsewhere, nil, "-C", ws, "-C", "acme/app", "sync", "--related", "--anon", "--dry-run")
	if !strings.Contains(stdout, "plan pull acme/lib\n") {
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
		"not a directory":    {"-C", filepath.Join(elsewhere, "missing"), "status"},
		"nothing after -C x": {"-C", ws},
	} {
		if _, stderr, code := runGitty(t, elsewhere, nil, args...); code != 2 {
			t.Errorf("%s: exit = %d, want 2\n%s", name, code, stderr)
		}
	}
}
