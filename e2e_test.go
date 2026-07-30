package main

// End-to-end tests: build the real gitty binary once, then drive it as a
// subprocess against a local fake GitLab API server that also serves real git
// repositories over HTTP (git's "dumb" protocol — a static file server over a
// bare repo after git update-server-info). Everything is hermetic: no network,
// no live GitLab, no token requirements beyond what each test configures.
//
// Run with: go test ./...   (skipped under -short)

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

var gittyBin string

func TestMain(m *testing.M) {
	code := func() int {
		tmp, err := os.MkdirTemp("", "gitty-e2e-bin")
		if err != nil {
			fmt.Fprintf(os.Stderr, "creating temp dir for e2e binary: %v\n", err)
			return 1
		}
		defer os.RemoveAll(tmp)

		gittyBin = filepath.Join(tmp, "gitty-e2e")
		out, err := exec.Command("go", "build", "-o", gittyBin, ".").CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "building gitty for e2e tests: %v\n%s", err, out)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping e2e test in -short mode")
	}
}

// runGitty executes the built binary in dir and returns combined output and
// the exit code. Ambient tokens are cleared and the proxy is bypassed for
// localhost so tests stay hermetic; extraEnv entries win over the defaults.
func runGitty(t *testing.T, dir string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(gittyBin, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(),
		"GITLAB_TOKEN=", "CI_JOB_TOKEN=",
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
	), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running gitty %v: %v\n%s", args, err, out)
		}
		return string(out), ee.ExitCode()
	}
	return string(out), 0
}

// gitRun executes git with a fixed identity for repo fixtures.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.com",
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// --- Fake GitLab (API + git-over-HTTP) ---

type apiGroup struct {
	ID       int    `json:"id"`
	FullPath string `json:"full_path"`
}

type apiProject struct {
	ID                int    `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	HTTPURLToRepo     string `json:"http_url_to_repo"`
	SSHURLToRepo      string `json:"ssh_url_to_repo"`
}

type fakeGitLab struct {
	srv     *httptest.Server
	gitRoot string

	// requireToken, when non-empty, makes API calls 401 unless the
	// PRIVATE-TOKEN header matches. pageSize, when > 0, paginates project
	// listings to exercise the client's pagination loop.
	requireToken string
	pageSize     int

	groups      map[string]apiGroup
	subgroups   map[string][]apiGroup
	descendants map[string][]apiGroup
	projects    map[string][]apiProject

	mu        sync.Mutex
	lastToken string
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{
		gitRoot:     t.TempDir(),
		groups:      map[string]apiGroup{},
		subgroups:   map[string][]apiGroup{},
		descendants: map[string][]apiGroup{},
		projects:    map[string][]apiProject{},
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/git/") {
		http.StripPrefix("/git/", http.FileServer(http.Dir(f.gitRoot))).ServeHTTP(w, r)
		return
	}

	if tok := r.Header.Get("PRIVATE-TOKEN"); tok != "" {
		f.mu.Lock()
		f.lastToken = tok
		f.mu.Unlock()
	}
	if f.requireToken != "" && r.Header.Get("PRIVATE-TOKEN") != f.requireToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
		return
	}

	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v4/groups/")
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Not Found"})
		return
	}

	notFound := func() {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Group Not Found"})
	}

	switch {
	case strings.HasSuffix(rest, "/projects"):
		target := strings.TrimSuffix(rest, "/projects")
		if _, known := f.groups[target]; !known {
			notFound()
			return
		}
		var list []apiProject
		nested := r.URL.Query().Get("include_subgroups") == "true"
		for key, projects := range f.projects {
			if key == target || (nested && strings.HasPrefix(key, target+"/")) {
				list = append(list, projects...)
			}
		}
		lo, hi := paginate(w, r, f.pageSize, len(list))
		writeJSON(w, http.StatusOK, list[lo:hi])

	case strings.HasSuffix(rest, "/subgroups"):
		target := strings.TrimSuffix(rest, "/subgroups")
		if _, known := f.groups[target]; !known {
			notFound()
			return
		}
		writeJSON(w, http.StatusOK, orEmptyGroups(f.subgroups[target]))

	case strings.HasSuffix(rest, "/descendant_groups"):
		target := strings.TrimSuffix(rest, "/descendant_groups")
		if _, known := f.groups[target]; !known {
			notFound()
			return
		}
		writeJSON(w, http.StatusOK, orEmptyGroups(f.descendants[target]))

	default:
		if g, known := f.groups[rest]; known {
			writeJSON(w, http.StatusOK, g)
			return
		}
		notFound()
	}
}

func orEmptyGroups(gs []apiGroup) []apiGroup {
	if gs == nil {
		return []apiGroup{}
	}
	return gs
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// paginate computes the slice bounds for the requested page and sets the
// X-Next-Page header the GitLab client uses to drive its pagination loop.
func paginate(w http.ResponseWriter, r *http.Request, pageSize, n int) (int, int) {
	if pageSize <= 0 {
		return 0, n
	}
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	lo := (page - 1) * pageSize
	if lo > n {
		lo = n
	}
	hi := lo + pageSize
	if hi > n {
		hi = n
	}
	if hi < n {
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
	}
	return lo, hi
}

// project builds an apiProject whose clone URL points at this fake server's
// git file server, so host validation passes and clones actually work.
func (f *fakeGitLab) project(id int, pathNS string) apiProject {
	return apiProject{
		ID:                id,
		PathWithNamespace: pathNS,
		HTTPURLToRepo:     f.srv.URL + "/git/" + pathNS + ".git",
		SSHURLToRepo:      "git@example.invalid:" + pathNS + ".git",
	}
}

// addRepo creates a work repo with the given files, publishes it as a bare
// repo served over HTTP at /git/<name>.git, and returns the work repo path so
// tests can push follow-up commits via pushUpdate.
func (f *fakeGitLab) addRepo(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	work := t.TempDir()
	gitRun(t, work, "init", "-b", "main")
	for path, content := range files {
		full := filepath.Join(work, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "initial")

	bare := filepath.Join(f.gitRoot, name+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "clone", "--bare", work, bare)
	gitRun(t, bare, "update-server-info")
	return work
}

// pushUpdate commits a file change in the work repo and publishes it to the
// served bare repo, refreshing the dumb-protocol metadata.
func (f *fakeGitLab) pushUpdate(t *testing.T, work, name, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "update")
	bare := filepath.Join(f.gitRoot, name+".git")
	gitRun(t, work, "push", bare, "main")
	gitRun(t, bare, "update-server-info")
}

// initWorkspace creates a temp workspace and runs `gitty init` against the
// fake server, returning the workspace path.
func initWorkspace(t *testing.T, f *fakeGitLab) string {
	t.Helper()
	ws := t.TempDir()
	out, code := runGitty(t, ws, nil, "init", "--http", "--url="+f.srv.URL)
	if code != 0 {
		t.Fatalf("gitty init failed (exit %d):\n%s", code, out)
	}
	return ws
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// --- Tests ---

func TestE2EInitWritesConfig(t *testing.T) {
	skipIfShort(t)
	ws := t.TempDir()

	out, code := runGitty(t, ws, nil, "init", "--http", "--url=https://gitlab.example.com")
	if code != 0 {
		t.Fatalf("init exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "Initialized gitty root") {
		t.Errorf("init output missing confirmation:\n%s", out)
	}

	var cfg Config
	if err := toml.Unmarshal([]byte(readFileT(t, filepath.Join(ws, ConfigDir, ConfigName))), &cfg); err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.URL != "https://gitlab.example.com" || !cfg.HTTP || cfg.RootPath != "" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestE2EAgentSchemaOutput(t *testing.T) {
	skipIfShort(t)

	out, code := runGitty(t, t.TempDir(), nil, "agent", "schema")
	if code != 0 {
		t.Fatalf("agent schema exit = %d, want 0:\n%s", code, out)
	}

	var schema AgentSchema
	if err := json.Unmarshal([]byte(out), &schema); err != nil {
		t.Fatalf("agent schema output is not valid JSON: %v\n%s", err, out)
	}
	if schema.Name != "gitty" {
		t.Errorf("schema name = %q, want gitty", schema.Name)
	}
	sync, ok := findTool(schema, "sync")
	if !ok {
		t.Fatal("schema missing sync tool")
	}
	if _, ok := sync.InputSchema.Properties["anon"]; !ok {
		t.Error("sync tool schema missing 'anon' property")
	}
}

func TestE2ECLIErrors(t *testing.T) {
	skipIfShort(t)

	t.Run("no arguments", func(t *testing.T) {
		out, code := runGitty(t, t.TempDir(), nil)
		if code != 1 || !strings.Contains(out, "Usage:") {
			t.Errorf("exit = %d, want 1 with usage:\n%s", code, out)
		}
	})

	t.Run("unknown command", func(t *testing.T) {
		out, code := runGitty(t, t.TempDir(), nil, "frobnicate")
		if code != 1 || !strings.Contains(out, "Unknown command") {
			t.Errorf("exit = %d, want 1 with unknown-command message:\n%s", code, out)
		}
	})

	t.Run("sync without workspace config", func(t *testing.T) {
		out, code := runGitty(t, t.TempDir(), nil, "sync", "--path=acme", "--anon")
		if code != 1 || !strings.Contains(out, ".gitty/config") {
			t.Errorf("exit = %d, want 1 mentioning missing config:\n%s", code, out)
		}
	})

	t.Run("sync without token or anon", func(t *testing.T) {
		f := newFakeGitLab(t)
		ws := initWorkspace(t, f)
		out, code := runGitty(t, ws, nil, "sync", "--path=acme")
		if code != 1 || !strings.Contains(out, "token") {
			t.Errorf("exit = %d, want 1 mentioning token requirement:\n%s", code, out)
		}
	})
}

func TestE2ESyncCloneThenPull(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	work := f.addRepo(t, "acme/portal", map[string]string{"README.md": "v1"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(10, "acme/portal")}
	f.requireToken = "sekret"

	ws := initWorkspace(t, f)

	out, code := runGitty(t, ws, nil, "sync", "--path=acme", "--token=sekret")
	if code != 0 {
		t.Fatalf("initial sync exit = %d, want 0:\n%s", code, out)
	}
	readme := filepath.Join(ws, "acme", "portal", "README.md")
	if got := readFileT(t, readme); got != "v1" {
		t.Errorf("cloned README = %q, want v1", got)
	}

	f.mu.Lock()
	seen := f.lastToken
	f.mu.Unlock()
	if seen != "sekret" {
		t.Errorf("server saw token %q, want sekret", seen)
	}

	// Publish an upstream change, then re-sync: the existing checkout must be
	// fast-forwarded, not re-cloned.
	f.pushUpdate(t, work, "acme/portal", "README.md", "v2")
	out, code = runGitty(t, ws, nil, "sync", "--path=acme", "--token=sekret")
	if code != 0 {
		t.Fatalf("re-sync exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "git pull --ff-only") {
		t.Errorf("re-sync output missing ff-only pull:\n%s", out)
	}
	if got := readFileT(t, readme); got != "v2" {
		t.Errorf("pulled README = %q, want v2", got)
	}
}

func TestE2ETokenFromEnvironment(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.requireToken = "envsecret"

	ws := initWorkspace(t, f)
	out, code := runGitty(t, ws, []string{"GITLAB_TOKEN=envsecret"}, "sync", "--path=acme", "--dry-run")
	if code != 0 {
		t.Fatalf("sync with env token exit = %d, want 0:\n%s", code, out)
	}

	f.mu.Lock()
	seen := f.lastToken
	f.mu.Unlock()
	if seen != "envsecret" {
		t.Errorf("server saw token %q, want envsecret", seen)
	}
}

func TestE2EAnonSync(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.addRepo(t, "acme/public", map[string]string{"file.txt": "public"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(11, "acme/public")}

	ws := initWorkspace(t, f)
	out, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 0 {
		t.Fatalf("anon sync exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "Running anonymously") {
		t.Errorf("anon sync output missing anonymous notice:\n%s", out)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "public", "file.txt")); got != "public" {
		t.Errorf("cloned file = %q, want public", got)
	}
}

func TestE2EGroupsSyncAndNestedContext(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.groups["acme/team"] = apiGroup{ID: 2, FullPath: "acme/team"}
	f.groups["acme/team/sub"] = apiGroup{ID: 3, FullPath: "acme/team/sub"}
	f.subgroups["acme"] = []apiGroup{f.groups["acme/team"]}
	f.descendants["acme"] = []apiGroup{f.groups["acme/team"], f.groups["acme/team/sub"]}
	f.addRepo(t, "acme/team/toolrepo", map[string]string{"tool.txt": "tooling"})
	f.projects["acme/team"] = []apiProject{f.project(20, "acme/team/toolrepo")}

	ws := initWorkspace(t, f)

	// Recreate the full group hierarchy with per-directory configs.
	out, code := runGitty(t, ws, nil, "sync", "--path=acme", "--groups", "--nested", "--anon")
	if code != 0 {
		t.Fatalf("groups sync exit = %d, want 0:\n%s", code, out)
	}
	for _, g := range []string{"acme", "acme/team", "acme/team/sub"} {
		confPath := filepath.Join(ws, filepath.FromSlash(g), ConfigDir, ConfigName)
		var cfg Config
		if err := toml.Unmarshal([]byte(readFileT(t, confPath)), &cfg); err != nil {
			t.Fatalf("parsing %s: %v", confPath, err)
		}
		if cfg.RootPath != g {
			t.Errorf("config %s root_path = %q, want %q", confPath, cfg.RootPath, g)
		}
		if cfg.URL != f.srv.URL || !cfg.HTTP {
			t.Errorf("config %s did not inherit url/http: %+v", confPath, cfg)
		}
	}

	// Nested-context sync: run from inside a managed subgroup directory with
	// no --path; the local config's root_path anchors the sync.
	subDir := filepath.Join(ws, "acme", "team")
	out, code = runGitty(t, subDir, nil, "sync", "--repos", "--anon")
	if code != 0 {
		t.Fatalf("nested-context sync exit = %d, want 0:\n%s", code, out)
	}
	if got := readFileT(t, filepath.Join(subDir, "toolrepo", "tool.txt")); got != "tooling" {
		t.Errorf("nested-context cloned file = %q, want tooling", got)
	}
}

func TestE2ENestedRepoSync(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.groups["acme/team"] = apiGroup{ID: 2, FullPath: "acme/team"}
	f.addRepo(t, "acme/rootrepo", map[string]string{"root.txt": "root"})
	f.addRepo(t, "acme/team/toolrepo", map[string]string{"tool.txt": "tool"})
	f.projects["acme"] = []apiProject{f.project(30, "acme/rootrepo")}
	f.projects["acme/team"] = []apiProject{f.project(31, "acme/team/toolrepo")}

	ws := initWorkspace(t, f)

	// Flat sync first: only the immediate project should appear.
	out, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 0 {
		t.Fatalf("flat sync exit = %d, want 0:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(ws, "acme", "team", "toolrepo")); !os.IsNotExist(err) {
		t.Error("flat sync must not clone subgroup projects")
	}

	// Nested sync pulls in the subgroup project too.
	out, code = runGitty(t, ws, nil, "sync", "--path=acme", "--nested", "--anon")
	if code != 0 {
		t.Fatalf("nested sync exit = %d, want 0:\n%s", code, out)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "rootrepo", "root.txt")); got != "root" {
		t.Errorf("root repo file = %q, want root", got)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "team", "toolrepo", "tool.txt")); got != "tool" {
		t.Errorf("subgroup repo file = %q, want tool", got)
	}
}

func TestE2EDryRunCreatesNothing(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.groups["acme/team"] = apiGroup{ID: 2, FullPath: "acme/team"}
	f.descendants["acme"] = []apiGroup{f.groups["acme/team"]}
	f.addRepo(t, "acme/portal", map[string]string{"README.md": "v1"})
	f.projects["acme"] = []apiProject{f.project(40, "acme/portal")}

	ws := initWorkspace(t, f)
	out, code := runGitty(t, ws, nil, "sync", "--path=acme", "--groups", "--repos", "--nested", "--anon", "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{"DRY RUN MODE ENABLED", "Would create group", "Would clone"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}

	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ConfigDir {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dry-run created files: %v (want only %s)", names, ConfigDir)
	}
}

func TestE2EPaginationClonesAllPages(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.pageSize = 1 // force the project listing across multiple pages
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.addRepo(t, "acme/alpha", map[string]string{"a.txt": "alpha"})
	f.addRepo(t, "acme/beta", map[string]string{"b.txt": "beta"})
	f.projects["acme"] = []apiProject{f.project(50, "acme/alpha"), f.project(51, "acme/beta")}

	ws := initWorkspace(t, f)
	out, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 0 {
		t.Fatalf("paginated sync exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "Found 2 projects.") {
		t.Errorf("expected both pages aggregated into 2 projects:\n%s", out)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "alpha", "a.txt")); got != "alpha" {
		t.Errorf("alpha file = %q", got)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "beta", "b.txt")); got != "beta" {
		t.Errorf("beta file = %q", got)
	}
}

func TestE2EUnknownGroupFailsNonZero(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	ws := initWorkspace(t, f)

	out, code := runGitty(t, ws, nil, "sync", "--path=ghost", "--anon")
	if code != 1 {
		t.Errorf("sync of unknown group exit = %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "404") || !strings.Contains(out, "error(s)") {
		t.Errorf("expected 404 and aggregate error in output:\n%s", out)
	}
}

func TestE2EForeignCloneHostRejected(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{{
		ID:                60,
		PathWithNamespace: "acme/hijack",
		HTTPURLToRepo:     "http://evil.invalid/hijack.git",
		SSHURLToRepo:      "git@evil.invalid:hijack.git",
	}}

	ws := initWorkspace(t, f)
	out, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 1 {
		t.Errorf("foreign-host sync exit = %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "does not match the configured instance") {
		t.Errorf("expected host-mismatch message:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "acme", "hijack")); !os.IsNotExist(err) {
		t.Error("foreign-host project must not be cloned")
	}
}

func TestE2EWorkspaceEscapeBlocked(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{{
		ID:                70,
		PathWithNamespace: "../evil",
		HTTPURLToRepo:     f.srv.URL + "/git/evil.git",
		SSHURLToRepo:      "git@example.invalid:evil.git",
	}}

	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatal(err)
	}
	if out, code := runGitty(t, ws, nil, "init", "--http", "--url="+f.srv.URL); code != 0 {
		t.Fatalf("init failed:\n%s", out)
	}

	out, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 1 {
		t.Errorf("escape sync exit = %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "escapes the workspace") {
		t.Errorf("expected escape message:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(base, "evil")); !os.IsNotExist(err) {
		t.Error("escaping project must not be written outside the workspace")
	}
}

func TestE2EDivergedCheckoutFailsPull(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	work := f.addRepo(t, "acme/portal", map[string]string{"README.md": "v1"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(80, "acme/portal")}

	ws := initWorkspace(t, f)
	if out, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon"); code != 0 {
		t.Fatalf("initial sync failed:\n%s", out)
	}

	// Diverge: a different commit upstream and a local commit in the checkout.
	f.pushUpdate(t, work, "acme/portal", "README.md", "v2-upstream")
	local := filepath.Join(ws, "acme", "portal")
	if err := os.WriteFile(filepath.Join(local, "README.md"), []byte("v2-local"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, local, "commit", "-am", "local divergence")

	out, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 1 {
		t.Errorf("diverged sync exit = %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "git pull failed") {
		t.Errorf("expected pull failure message:\n%s", out)
	}
	// The local commit must survive: --ff-only never merges or overwrites.
	if got := readFileT(t, filepath.Join(local, "README.md")); got != "v2-local" {
		t.Errorf("local README = %q, want v2-local preserved", got)
	}
}
