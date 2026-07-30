package main

// End-to-end tests: build the real gitty binary once, then drive it as a
// subprocess against a local fake GitLab API server that also serves real git
// repositories over HTTP (git's "dumb" protocol — a static file server over a
// bare repo after git update-server-info). Everything is hermetic: no network,
// no live GitLab, no token requirements beyond what each test configures.
//
// Run with: go test ./...   (skipped under -short)

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// gittyEnv builds the subprocess environment: ambient tokens are cleared and
// the proxy is bypassed for localhost so tests stay hermetic; extraEnv entries
// win over the defaults.
func gittyEnv(extraEnv []string) []string {
	return append(append(os.Environ(),
		"GITLAB_TOKEN=", "CI_JOB_TOKEN=",
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
	), extraEnv...)
}

// runGitty executes the built binary in dir and returns its stdout, stderr,
// and exit code separately — event lines live on stdout, diagnostics on
// stderr, and tests assert against the correct stream.
func runGitty(t *testing.T, dir string, extraEnv []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(gittyBin, args...)
	cmd.Dir = dir
	cmd.Env = gittyEnv(extraEnv)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running gitty %v: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
		}
		return stdout.String(), stderr.String(), ee.ExitCode()
	}
	return stdout.String(), stderr.String(), 0
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

	// gitDelayNs slows every /git/ request so tests can catch a sync mid-git
	// (SIGINT, concurrency); atomic because tests adjust it while the server
	// handles requests. gitHits counts /git/ requests; gitInflight and
	// gitMaxInflight track a concurrency high-water mark.
	gitDelayNs     atomic.Int64
	gitHits        atomic.Int32
	gitInflight    atomic.Int32
	gitMaxInflight atomic.Int32

	// gitAuthUser/gitAuthPass, when set (before the server starts), require
	// HTTP Basic auth on every /git/ request — the e2e proof that askpass
	// credential injection reaches the git transport.
	gitAuthUser string
	gitAuthPass string

	groups      map[string]apiGroup
	subgroups   map[string][]apiGroup
	descendants map[string][]apiGroup
	projects    map[string][]apiProject

	mu          sync.Mutex
	lastToken   string
	lastGitAuth string // "user:pass" of the last authenticated /git/ request
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
		f.gitHits.Add(1)
		cur := f.gitInflight.Add(1)
		defer f.gitInflight.Add(-1)
		for {
			max := f.gitMaxInflight.Load()
			if cur <= max || f.gitMaxInflight.CompareAndSwap(max, cur) {
				break
			}
		}
		if d := f.gitDelayNs.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
		if f.gitAuthUser != "" {
			user, pass, ok := r.BasicAuth()
			if ok {
				f.mu.Lock()
				f.lastGitAuth = user + ":" + pass
				f.mu.Unlock()
			}
			if !ok || user != f.gitAuthUser || pass != f.gitAuthPass {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
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

// startGitty launches the binary without waiting, for tests that signal it
// mid-run. The returned buffers fill as the process writes.
func startGitty(t *testing.T, dir string, extraEnv []string, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(gittyBin, args...)
	cmd.Dir = dir
	cmd.Env = gittyEnv(extraEnv)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting gitty %v: %v", args, err)
	}
	return cmd, &stdout, &stderr
}

// initWorkspace creates a temp workspace and runs `gitty init` against the
// fake server, returning the workspace path.
func initWorkspace(t *testing.T, f *fakeGitLab) string {
	t.Helper()
	ws := t.TempDir()
	stdout, stderr, code := runGitty(t, ws, nil, "init", "--http", "--url="+f.srv.URL)
	if code != 0 {
		t.Fatalf("gitty init failed (exit %d):\n%s\n%s", code, stdout, stderr)
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

	stdout, stderr, code := runGitty(t, ws, nil, "init", "--http", "--url=https://gitlab.example.com")
	if code != 0 {
		t.Fatalf("init exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Initialized gitty root") {
		t.Errorf("init stdout missing confirmation:\n%s", stdout)
	}

	var cfg Config
	if err := toml.Unmarshal([]byte(readFileT(t, filepath.Join(ws, ConfigDir, ConfigName))), &cfg); err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.URL != "https://gitlab.example.com" || !cfg.HTTP || cfg.RootPath != "" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestE2EInitRefusesClobber(t *testing.T) {
	skipIfShort(t)
	ws := t.TempDir()

	if stdout, stderr, code := runGitty(t, ws, nil, "init", "--http", "--url=https://one.example.com"); code != 0 {
		t.Fatalf("first init failed:\n%s\n%s", stdout, stderr)
	}
	before := readFileT(t, filepath.Join(ws, ConfigDir, ConfigName))

	_, stderr, code := runGitty(t, ws, nil, "init", "--url=https://two.example.com")
	if code != 2 {
		t.Fatalf("second init should exit 2 without --force, got %d:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "--force") {
		t.Errorf("refusal should mention --force:\n%s", stderr)
	}
	if after := readFileT(t, filepath.Join(ws, ConfigDir, ConfigName)); after != before {
		t.Error("refused init modified the config")
	}

	if stdout, stderr, code := runGitty(t, ws, nil, "init", "--url=https://two.example.com", "--force"); code != 0 {
		t.Fatalf("forced init failed:\n%s\n%s", stdout, stderr)
	}
	if !strings.Contains(readFileT(t, filepath.Join(ws, ConfigDir, ConfigName)), "two.example.com") {
		t.Error("forced init did not take effect")
	}
}

func TestE2EInitRejectsBadURL(t *testing.T) {
	skipIfShort(t)
	_, stderr, code := runGitty(t, t.TempDir(), nil, "init", "--url=not-a-url")
	if code != 2 || !strings.Contains(stderr, "invalid --url") {
		t.Errorf("exit = %d, want 2 with invalid-url message:\n%s", code, stderr)
	}
}

func TestE2EAgentSchemaOutput(t *testing.T) {
	skipIfShort(t)

	stdout, stderr, code := runGitty(t, t.TempDir(), nil, "agent", "schema")
	if code != 0 {
		t.Fatalf("agent schema exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
	}

	var schema AgentSchema
	if err := json.Unmarshal([]byte(stdout), &schema); err != nil {
		t.Fatalf("agent schema stdout is not valid JSON: %v\n%s", err, stdout)
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
		_, stderr, code := runGitty(t, t.TempDir(), nil)
		if code != 2 || !strings.Contains(stderr, "Usage:") {
			t.Errorf("exit = %d, want 2 with usage on stderr:\n%s", code, stderr)
		}
	})

	t.Run("unknown command", func(t *testing.T) {
		_, stderr, code := runGitty(t, t.TempDir(), nil, "frobnicate")
		if code != 2 || !strings.Contains(stderr, "Unknown command") {
			t.Errorf("exit = %d, want 2 with unknown-command message on stderr:\n%s", code, stderr)
		}
	})

	t.Run("sync without workspace config", func(t *testing.T) {
		_, stderr, code := runGitty(t, t.TempDir(), nil, "sync", "--path=acme", "--anon")
		if code != 2 || !strings.Contains(stderr, ".gitty/config") {
			t.Errorf("exit = %d, want 2 mentioning missing config on stderr:\n%s", code, stderr)
		}
	})

	t.Run("sync without token or anon", func(t *testing.T) {
		f := newFakeGitLab(t)
		ws := initWorkspace(t, f)
		_, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme")
		if code != 2 || !strings.Contains(stderr, "token") {
			t.Errorf("exit = %d, want 2 mentioning token requirement on stderr:\n%s", code, stderr)
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

	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--token=sekret")
	if code != 0 {
		t.Fatalf("initial sync exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "clone acme/portal\n") {
		t.Errorf("stdout missing clone event:\n%s", stdout)
	}
	assertGreppableStdout(t, stdout)
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
	stdout, stderr, code = runGitty(t, ws, nil, "sync", "--path=acme", "--token=sekret")
	if code != 0 {
		t.Fatalf("re-sync exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "pull acme/portal\n") {
		t.Errorf("stdout missing pull event:\n%s", stdout)
	}
	assertGreppableStdout(t, stdout)
	if got := readFileT(t, readme); got != "v2" {
		t.Errorf("pulled README = %q, want v2", got)
	}
}

// assertGreppableStdout enforces the constitution's output contract: every
// stdout line is one machine-readable event with a stable prefix.
func assertGreppableStdout(t *testing.T, stdout string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !e2eEventRe.MatchString(line) {
			t.Errorf("stdout line is not a stable event: %q", line)
		}
	}
}

var e2eEventRe = regexp.MustCompile(`^(clone|pull|group|project|reclone|skip|status|error|plan|summary) `)

func TestE2ETokenFromEnvironment(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.requireToken = "envsecret"

	ws := initWorkspace(t, f)
	stdout, stderr, code := runGitty(t, ws, []string{"GITLAB_TOKEN=envsecret"}, "sync", "--path=acme", "--dry-run")
	if code != 0 {
		t.Fatalf("sync with env token exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
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
	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 0 {
		t.Fatalf("anon sync exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "Running anonymously") {
		t.Errorf("anon notice should be on stderr:\n%s", stderr)
	}
	if !strings.Contains(stdout, "clone acme/public\n") {
		t.Errorf("stdout missing clone event:\n%s", stdout)
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
	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--groups", "--nested", "--anon")
	if code != 0 {
		t.Fatalf("groups sync exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "group acme/team/sub\n") {
		t.Errorf("stdout missing group event:\n%s", stdout)
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
	stdout, stderr, code = runGitty(t, subDir, nil, "sync", "--repos", "--anon")
	if code != 0 {
		t.Fatalf("nested-context sync exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
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
	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 0 {
		t.Fatalf("flat sync exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(ws, "acme", "team", "toolrepo")); !os.IsNotExist(err) {
		t.Error("flat sync must not clone subgroup projects")
	}

	// Nested sync pulls in the subgroup project too.
	stdout, stderr, code = runGitty(t, ws, nil, "sync", "--path=acme", "--nested", "--anon")
	if code != 0 {
		t.Fatalf("nested sync exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
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
	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--groups", "--repos", "--nested", "--anon", "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "DRY RUN MODE ENABLED") {
		t.Errorf("dry-run banner should be on stderr:\n%s", stderr)
	}
	for _, want := range []string{"plan group acme\n", "plan clone acme/portal\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run stdout missing %q:\n%s", want, stdout)
		}
	}
	assertGreppableStdout(t, stdout)

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
	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 0 {
		t.Fatalf("paginated sync exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "Found 2 projects.") {
		t.Errorf("expected both pages aggregated into 2 projects:\n%s", stderr)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "alpha", "a.txt")); got != "alpha" {
		t.Errorf("alpha file = %q", got)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "beta", "b.txt")); got != "beta" {
		t.Errorf("beta file = %q", got)
	}
}

func TestE2EStatus(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.addRepo(t, "acme/statusrepo", map[string]string{"README.md": "v1"})
	cleanWork := f.addRepo(t, "acme/cleanrepo", map[string]string{"c.txt": "c"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(120, "acme/statusrepo"), f.project(121, "acme/cleanrepo")}

	ws := initWorkspace(t, f)
	if stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon"); code != 0 {
		t.Fatalf("setup sync failed:\n%s\n%s", stdout, stderr)
	}

	// Freshly cloned: clean, on a branch, in sync with its upstream.
	stdout, stderr, code := runGitty(t, ws, nil, "status")
	if code != 0 {
		t.Fatalf("status exit = %d:\n%s\n%s", code, stdout, stderr)
	}
	assertGreppableStdout(t, stdout)
	if !strings.Contains(stdout, "status acme/cleanrepo branch=main ahead=0 behind=0 dirty=false\n") {
		t.Errorf("unexpected clean status line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "summary repos=2 dirty=0 ahead=0 behind=0 errors=0") {
		t.Errorf("unexpected summary:\n%s", stdout)
	}

	// Dirty the tree and add a local commit: dirty=true and ahead=1.
	local := filepath.Join(ws, "acme", "statusrepo")
	if err := os.WriteFile(filepath.Join(local, "README.md"), []byte("local edit"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, local, "commit", "-am", "local commit")
	if err := os.WriteFile(filepath.Join(local, "untracked.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	stdout, _, code = runGitty(t, ws, nil, "status")
	if code != 0 {
		t.Fatalf("status exit = %d:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "status acme/statusrepo branch=main ahead=1 behind=0 dirty=true\n") {
		t.Errorf("expected ahead=1 dirty=true for the edited repo:\n%s", stdout)
	}
	if !strings.Contains(stdout, "summary repos=2 dirty=1 ahead=1 behind=0 errors=0") {
		t.Errorf("unexpected summary:\n%s", stdout)
	}

	// An upstream commit is invisible until --fetch refreshes the refs.
	f.pushUpdate(t, cleanWork, "acme/cleanrepo", "c.txt", "c2")
	stdout, _, code = runGitty(t, ws, nil, "status")
	if code != 0 {
		t.Fatalf("status exit = %d:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "status acme/cleanrepo branch=main ahead=0 behind=0 dirty=false\n") {
		t.Errorf("stale status should not show behind before --fetch:\n%s", stdout)
	}

	stdout, stderr, code = runGitty(t, ws, nil, "status", "--fetch", "--anon")
	if code != 0 {
		t.Fatalf("status --fetch exit = %d:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "status acme/cleanrepo branch=main ahead=0 behind=1 dirty=false\n") {
		t.Errorf("--fetch should reveal behind=1:\n%s", stdout)
	}
}

func TestE2EStatusReportsNoUpstream(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.addRepo(t, "acme/norepo", map[string]string{"a.txt": "a"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(122, "acme/norepo")}

	ws := initWorkspace(t, f)
	if _, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon"); code != 0 {
		t.Fatalf("setup sync failed:\n%s", stderr)
	}
	// A branch with no upstream must say so rather than reporting 0/0.
	gitRun(t, filepath.Join(ws, "acme", "norepo"), "checkout", "-b", "orphan-branch")

	stdout, _, code := runGitty(t, ws, nil, "status")
	if code != 0 {
		t.Fatalf("status exit = %d:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "status acme/norepo branch=orphan-branch ahead=0 behind=0 dirty=false upstream=none\n") {
		t.Errorf("expected upstream=none:\n%s", stdout)
	}
}

func TestE2ELs(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.addRepo(t, "acme/alpha", map[string]string{"a.txt": "a"})
	f.addRepo(t, "acme/team/beta", map[string]string{"b.txt": "b"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.groups["acme/team"] = apiGroup{ID: 2, FullPath: "acme/team"}
	f.descendants["acme"] = []apiGroup{f.groups["acme/team"]}
	f.projects["acme"] = []apiProject{f.project(130, "acme/alpha")}
	f.projects["acme/team"] = []apiProject{f.project(131, "acme/team/beta")}

	ws := initWorkspace(t, f)

	// Nothing cloned yet: every project is new, and ls must not create anything.
	stdout, stderr, code := runGitty(t, ws, nil, "ls", "--path=acme", "--nested", "--anon")
	if code != 0 {
		t.Fatalf("ls exit = %d:\n%s\n%s", code, stdout, stderr)
	}
	assertGreppableStdout(t, stdout)
	for _, want := range []string{
		"group acme projects=1\n",
		"project acme/alpha new\n",
		"group acme/team projects=1\n",
		"project acme/team/beta new\n",
		"summary groups=2 projects=2 new=2 present=0\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("ls output missing %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "acme", "alpha")); !os.IsNotExist(err) {
		t.Error("ls must not create anything in the workspace")
	}

	// After a sync the same listing reports them present.
	if _, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--nested", "--anon"); code != 0 {
		t.Fatalf("sync failed:\n%s", stderr)
	}
	stdout, _, code = runGitty(t, ws, nil, "ls", "--path=acme", "--nested", "--anon")
	if code != 0 {
		t.Fatalf("ls exit = %d:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "project acme/alpha present\n") ||
		!strings.Contains(stdout, "summary groups=2 projects=2 new=0 present=2\n") {
		t.Errorf("ls should report both projects present:\n%s", stdout)
	}

	// Tree format: indented, with counts and +/= markers.
	stdout, _, code = runGitty(t, ws, nil, "ls", "--path=acme", "--nested", "--anon", "--format=tree")
	if code != 0 {
		t.Fatalf("ls --format=tree exit = %d:\n%s", code, stdout)
	}
	for _, want := range []string{"acme (1 project)\n", "  = alpha\n", "  team (1 project)\n", "    = beta\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tree output missing %q:\n%s", want, stdout)
		}
	}

	// JSON format parses and carries the same facts.
	stdout, _, code = runGitty(t, ws, nil, "ls", "--path=acme", "--nested", "--anon", "--format=json")
	if code != 0 {
		t.Fatalf("ls --format=json exit = %d:\n%s", code, stdout)
	}
	var report lsReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("ls json output is not valid JSON: %v\n%s", err, stdout)
	}
	if report.Target != "acme" || report.Summary.Projects != 2 || report.Summary.Present != 2 {
		t.Errorf("json report = %+v", report)
	}

	// An unknown format is a usage error.
	_, stderr, code = runGitty(t, ws, nil, "ls", "--path=acme", "--anon", "--format=yaml")
	if code != 2 || !strings.Contains(stderr, "--format") {
		t.Errorf("bad format exit = %d, want 2 mentioning --format:\n%s", code, stderr)
	}
}

func TestE2EVerbose(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.addRepo(t, "acme/vrepo", map[string]string{"v.txt": "v"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(95, "acme/vrepo")}

	ws := initWorkspace(t, f)
	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon", "--verbose")
	if code != 0 {
		t.Fatalf("verbose sync exit = %d:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "exec git clone") {
		t.Errorf("verbose exec line missing from stderr:\n%s", stderr)
	}
	if strings.Contains(stdout, "exec git") {
		t.Errorf("verbose exec line leaked to stdout:\n%s", stdout)
	}
	assertGreppableStdout(t, stdout)
}

func TestE2ESummaryLine(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.addRepo(t, "acme/one", map[string]string{"a.txt": "a"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(90, "acme/one")}

	ws := initWorkspace(t, f)

	lastLine := func(s string) string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		return lines[len(lines)-1]
	}

	// Dry run and real run must produce the identical summary (parity).
	dryOut, _, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon", "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run exit = %d", code)
	}
	realOut, _, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 0 {
		t.Fatalf("real sync exit = %d", code)
	}
	want := "summary cloned=1 pulled=0 skipped=0 errors=0"
	if got := lastLine(dryOut); got != want {
		t.Errorf("dry-run summary = %q, want %q", got, want)
	}
	if got := lastLine(realOut); got != want {
		t.Errorf("real summary = %q, want %q", got, want)
	}

	// A second run pulls instead of cloning.
	stdout, _, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 0 {
		t.Fatalf("re-sync exit = %d", code)
	}
	if got := lastLine(stdout); got != "summary cloned=0 pulled=1 skipped=0 errors=0" {
		t.Errorf("re-sync summary = %q", got)
	}
}

func TestE2EUnknownGroupFailsNonZero(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	ws := initWorkspace(t, f)

	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=ghost", "--anon")
	if code != 1 {
		t.Errorf("sync of unknown group exit = %d, want 1:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "error ghost listing projects failed") {
		t.Errorf("expected listing error event on stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "404") || !strings.Contains(stderr, "error(s)") {
		t.Errorf("expected 404 detail and aggregate error on stderr:\n%s", stderr)
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
	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 1 {
		t.Errorf("foreign-host sync exit = %d, want 1:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "error acme/hijack clone URL host does not match") {
		t.Errorf("expected host-mismatch error event on stdout:\n%s", stdout)
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
	if stdout, stderr, code := runGitty(t, ws, nil, "init", "--http", "--url="+f.srv.URL); code != 0 {
		t.Fatalf("init failed:\n%s\n%s", stdout, stderr)
	}

	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 1 {
		t.Errorf("escape sync exit = %d, want 1:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "error ../evil resolved path escapes the workspace") {
		t.Errorf("expected escape error event on stdout:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(base, "evil")); !os.IsNotExist(err) {
		t.Error("escaping project must not be written outside the workspace")
	}
}

func TestE2EConcurrentJobs(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.gitDelayNs.Store(int64(50 * time.Millisecond))
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("acme/conc%d", i)
		f.addRepo(t, name, map[string]string{"f.txt": name})
		f.projects["acme"] = append(f.projects["acme"], f.project(100+i, name))
	}

	ws := initWorkspace(t, f)
	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon", "--jobs=4")
	if code != 0 {
		t.Fatalf("concurrent sync exit = %d:\n%s\n%s", code, stdout, stderr)
	}
	for i := 0; i < 6; i++ {
		if !strings.Contains(stdout, fmt.Sprintf("clone acme/conc%d\n", i)) {
			t.Errorf("missing clone event for conc%d:\n%s", i, stdout)
		}
	}
	assertGreppableStdout(t, stdout)
	// The high-water mark proves genuine overlap without flaky wall-clock
	// timing assertions.
	if max := f.gitMaxInflight.Load(); max < 2 {
		t.Errorf("git request high-water mark = %d, want >= 2 (no concurrency observed)", max)
	}

	// Out-of-range --jobs is a usage error.
	_, stderr, code = runGitty(t, ws, nil, "sync", "--path=acme", "--anon", "--jobs=0")
	if code != 2 || !strings.Contains(stderr, "--jobs") {
		t.Errorf("jobs=0 exit = %d, want 2 mentioning --jobs:\n%s", code, stderr)
	}

	// --jobs=1 remains valid serial behavior.
	f.gitDelayNs.Store(0)
	stdout, _, code = runGitty(t, ws, nil, "sync", "--path=acme", "--anon", "--jobs=1")
	if code != 0 {
		t.Fatalf("jobs=1 sync exit = %d:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "summary cloned=0 pulled=6 skipped=0 errors=0") {
		t.Errorf("jobs=1 re-sync summary unexpected:\n%s", stdout)
	}
}

func TestE2EBrokenCheckoutRecovery(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.addRepo(t, "acme/portal", map[string]string{"README.md": "v1"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(97, "acme/portal")}

	ws := initWorkspace(t, f)
	if stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon"); code != 0 {
		t.Fatalf("initial sync failed:\n%s\n%s", stdout, stderr)
	}

	// Break the checkout: a directory with content but no .git wedges a plain
	// pull forever.
	local := filepath.Join(ws, "acme", "portal")
	if err := os.RemoveAll(filepath.Join(local, ".git")); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 1 {
		t.Errorf("broken checkout sync exit = %d, want 1:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "error acme/portal broken checkout") {
		t.Errorf("expected broken-checkout error event:\n%s", stdout)
	}

	// Dry-run with the flag plans the reclone but must not move anything.
	stdout, _, code = runGitty(t, ws, nil, "sync", "--path=acme", "--anon", "--reclone-broken", "--dry-run")
	if code != 0 {
		t.Errorf("dry-run reclone exit = %d, want 0:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "plan reclone acme/portal") {
		t.Errorf("expected plan reclone event:\n%s", stdout)
	}
	if _, err := os.Stat(local + ".gitty-broken-1"); !os.IsNotExist(err) {
		t.Error("dry-run must not create the aside directory")
	}

	// Real reclone: the broken dir is renamed aside (preserved) and a fresh
	// clone takes its place.
	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon", "--reclone-broken")
	if code != 0 {
		t.Fatalf("reclone sync exit = %d, want 0:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "reclone acme/portal\n") {
		t.Errorf("expected reclone event:\n%s", stdout)
	}
	if got := readFileT(t, filepath.Join(local+".gitty-broken-1", "README.md")); got != "v1" {
		t.Errorf("aside dir should preserve original contents, got %q", got)
	}
	if got := readFileT(t, filepath.Join(local, "README.md")); got != "v1" {
		t.Errorf("fresh clone content = %q", got)
	}
	gitRun(t, local, "rev-parse", "--is-inside-work-tree")
}

func TestE2ESIGINTInterruptsAndRecovers(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.gitDelayNs.Store(int64(200 * time.Millisecond))
	f.addRepo(t, "acme/first", map[string]string{"a.txt": "a"})
	f.addRepo(t, "acme/second", map[string]string{"b.txt": "b"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(98, "acme/first"), f.project(99, "acme/second")}

	ws := initWorkspace(t, f)
	cmd, stdout, stderr := startGitty(t, ws, nil, "sync", "--path=acme", "--anon")

	// Wait until the sync is demonstrably mid-git, then interrupt. Polling the
	// server's hit counter makes this deterministic — no timing guesses.
	deadline := time.Now().Add(15 * time.Second)
	for f.gitHits.Load() < 1 {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("timed out waiting for git traffic")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signaling gitty: %v", err)
	}

	err := cmd.Wait()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("waiting for gitty: %v", err)
		}
		code = ee.ExitCode()
	}
	if code != 130 {
		t.Errorf("interrupted exit = %d, want 130\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "interrupted") {
		t.Errorf("stderr missing interrupted notice:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "summary ") {
		t.Errorf("summary line must still print on interrupt:\n%s", stdout.String())
	}

	// Recovery proof: a follow-up sync completes clean and both repos exist.
	f.gitDelayNs.Store(0)
	out, stderrStr, rcode := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if rcode != 0 {
		t.Fatalf("recovery sync exit = %d, want 0:\n%s\n%s", rcode, out, stderrStr)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "first", "a.txt")); got != "a" {
		t.Errorf("first repo content = %q", got)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "second", "b.txt")); got != "b" {
		t.Errorf("second repo content = %q", got)
	}
}

// assertNoTokenAnywhere fails if the token appears in the given output
// streams or in any file under root (covers .git/config, credential stores,
// FETCH_HEAD — everything).
func assertNoTokenAnywhere(t *testing.T, token, root string, streams ...string) {
	t.Helper()
	for i, s := range streams {
		if strings.Contains(s, token) {
			t.Errorf("token leaked into output stream %d:\n%s", i, s)
		}
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr != nil || info.Size() > 1<<20 {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr == nil && bytes.Contains(data, []byte(token)) {
			t.Errorf("token found on disk at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

func TestE2EHTTPAuthClonePull(t *testing.T) {
	skipIfShort(t)

	const token = "glpat-e2e-sekret"
	f := newFakeGitLab(t)
	f.gitAuthUser, f.gitAuthPass = "oauth2", token
	f.requireToken = token
	work := f.addRepo(t, "acme/authrepo", map[string]string{"README.md": "v1"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(110, "acme/authrepo")}

	ws := initWorkspace(t, f)

	// Clone against the auth-requiring git server, with --verbose to maximize
	// the leak surface under test.
	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--token="+token, "--verbose")
	if code != 0 {
		t.Fatalf("authenticated clone exit = %d:\n%s\n%s", code, stdout, stderr)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "authrepo", "README.md")); got != "v1" {
		t.Errorf("cloned README = %q", got)
	}
	f.mu.Lock()
	gitAuth := f.lastGitAuth
	f.mu.Unlock()
	if gitAuth != "oauth2:"+token {
		t.Errorf("git server saw auth %q, want oauth2:%s", gitAuth, token)
	}
	assertNoTokenAnywhere(t, token, ws, stdout, stderr)

	// Authenticated pull: upstream change picked up through the injected path.
	f.pushUpdate(t, work, "acme/authrepo", "README.md", "v2")
	stdout, stderr, code = runGitty(t, ws, nil, "sync", "--path=acme", "--token="+token, "--verbose")
	if code != 0 {
		t.Fatalf("authenticated pull exit = %d:\n%s\n%s", code, stdout, stderr)
	}
	if got := readFileT(t, filepath.Join(ws, "acme", "authrepo", "README.md")); got != "v2" {
		t.Errorf("pulled README = %q, want v2", got)
	}
	if !strings.Contains(stdout, "pull acme/authrepo\n") {
		t.Errorf("missing pull event:\n%s", stdout)
	}
	assertNoTokenAnywhere(t, token, ws, stdout, stderr)
}

func TestE2ECIJobTokenUsername(t *testing.T) {
	skipIfShort(t)

	const token = "ci-job-tok-123"
	f := newFakeGitLab(t)
	f.gitAuthUser, f.gitAuthPass = "gitlab-ci-token", token
	f.requireToken = token
	f.addRepo(t, "acme/cirepo", map[string]string{"ci.txt": "ci"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(111, "acme/cirepo")}

	ws := initWorkspace(t, f)
	stdout, stderr, code := runGitty(t, ws, []string{"CI_JOB_TOKEN=" + token}, "sync", "--path=acme")
	if code != 0 {
		t.Fatalf("CI-token clone exit = %d:\n%s\n%s", code, stdout, stderr)
	}
	f.mu.Lock()
	gitAuth := f.lastGitAuth
	f.mu.Unlock()
	if gitAuth != "gitlab-ci-token:"+token {
		t.Errorf("git server saw auth %q, want gitlab-ci-token:%s", gitAuth, token)
	}
	assertNoTokenAnywhere(t, token, ws, stdout, stderr)
}

func TestE2EBadTokenFailsFast(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	f.gitAuthUser, f.gitAuthPass = "oauth2", "right-token"
	f.addRepo(t, "acme/lockedrepo", map[string]string{"x.txt": "x"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(112, "acme/lockedrepo")}

	ws := initWorkspace(t, f)
	// Wrong token: the API side accepts (requireToken unset) but the git
	// server 401s. Must fail promptly — no hanging on a terminal prompt —
	// with an error event and exit 1.
	stdout, _, code := runGitty(t, ws, nil, "sync", "--path=acme", "--token=wrong-token")
	if code != 1 {
		t.Errorf("bad-token sync exit = %d, want 1:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "error acme/lockedrepo git clone failed") {
		t.Errorf("expected clone failure event:\n%s", stdout)
	}
}

func TestE2EAmbientCredentialHelperIgnored(t *testing.T) {
	skipIfShort(t)

	const token = "glpat-helper-test"
	f := newFakeGitLab(t)
	f.gitAuthUser, f.gitAuthPass = "oauth2", token
	f.addRepo(t, "acme/helperrepo", map[string]string{"h.txt": "h"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(113, "acme/helperrepo")}

	// A fake HOME with a configured `store` credential helper holding WRONG
	// credentials for our host. The empty-helper reset must ignore it on read
	// AND prevent it from capturing our token on write.
	home := t.TempDir()
	gitconfig := "[credential]\n\thelper = store\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0644); err != nil {
		t.Fatal(err)
	}
	credFile := filepath.Join(home, ".git-credentials")
	host := strings.TrimPrefix(f.srv.URL, "http://")
	if err := os.WriteFile(credFile, []byte("http://oauth2:stale-wrong@"+host+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ws := initWorkspace(t, f)
	env := []string{"HOME=" + home, "XDG_CONFIG_HOME="}
	stdout, stderr, code := runGitty(t, ws, env, "sync", "--path=acme", "--token="+token)
	if code != 0 {
		t.Fatalf("sync with ambient helper exit = %d:\n%s\n%s", code, stdout, stderr)
	}
	// The store helper must not have captured our token.
	if creds := readFileT(t, credFile); strings.Contains(creds, token) {
		t.Errorf("ambient store helper captured the injected token:\n%s", creds)
	}
	assertNoTokenAnywhere(t, token, ws, stdout, stderr)
}

func TestE2EDivergedCheckoutFailsPull(t *testing.T) {
	skipIfShort(t)

	f := newFakeGitLab(t)
	work := f.addRepo(t, "acme/portal", map[string]string{"README.md": "v1"})
	f.groups["acme"] = apiGroup{ID: 1, FullPath: "acme"}
	f.projects["acme"] = []apiProject{f.project(80, "acme/portal")}

	ws := initWorkspace(t, f)
	if stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon"); code != 0 {
		t.Fatalf("initial sync failed:\n%s\n%s", stdout, stderr)
	}

	// Diverge: a different commit upstream and a local commit in the checkout.
	f.pushUpdate(t, work, "acme/portal", "README.md", "v2-upstream")
	local := filepath.Join(ws, "acme", "portal")
	if err := os.WriteFile(filepath.Join(local, "README.md"), []byte("v2-local"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, local, "commit", "-am", "local divergence")

	stdout, stderr, code := runGitty(t, ws, nil, "sync", "--path=acme", "--anon")
	if code != 1 {
		t.Errorf("diverged sync exit = %d, want 1:\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "error acme/portal git pull failed") {
		t.Errorf("expected pull failure event on stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "--- git pull --ff-only for acme/portal failed") {
		t.Errorf("expected attributed git output block on stderr:\n%s", stderr)
	}
	// The local commit must survive: --ff-only never merges or overwrites.
	if got := readFileT(t, filepath.Join(local, "README.md")); got != "v2-local" {
		t.Errorf("local README = %q, want v2-local preserved", got)
	}
}
