package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// The hub, end to end: the real gitty, the real gitty-up, real git, and the
// fake instance between them.

var (
	upBuildOnce sync.Once
	upBuildErr  error
	upGittyBin  string // a gitty with gitty-up installed beside it
)

// gittyWithUp returns a gitty that has gitty-up installed the way a user
// installs it: sitting next to gitty. It is a copy of the shared e2e binary
// in a directory of its own, so that the other tests keep a gitty with no
// plugins.
func gittyWithUp(t *testing.T) string {
	t.Helper()
	upBuildOnce.Do(func() {
		dir := filepath.Join(e2eBinDir, "with-up")
		if upBuildErr = os.MkdirAll(dir, 0o755); upBuildErr != nil {
			return
		}
		var data []byte
		if data, upBuildErr = os.ReadFile(gittyBin); upBuildErr != nil {
			return
		}
		upGittyBin = filepath.Join(dir, "gitty")
		if upBuildErr = os.WriteFile(upGittyBin, data, 0o755); upBuildErr != nil {
			return
		}
		out, err := exec.Command("go", "build", "-o", filepath.Join(dir, "gitty-up"), "./cmd/gitty-up").CombinedOutput()
		if err != nil {
			upBuildErr = fmt.Errorf("%v: %s", err, out)
		}
	})
	if upBuildErr != nil {
		t.Fatalf("setting up gitty with gitty-up: %v", upBuildErr)
	}
	return upGittyBin
}

// runningHub is a `gitty up` started for a test.
type runningHub struct {
	t      *testing.T
	url    string
	cmd    *exec.Cmd
	mu     sync.Mutex
	stderr strings.Builder
}

var hubAddressRe = regexp.MustCompile(`serving MCP at (http://[^/\s]+)/mcp`)

// startHub runs `gitty up` against the fake instance, on a port of its own
// choosing, and waits until it has listed the instance.
func startHub(t *testing.T, f *fakeGitLab, args ...string) *runningHub {
	t.Helper()
	publishFixtures()
	bin := gittyWithUp(t)
	all := append([]string{"up", "--url=" + f.srv.URL, "--listen=127.0.0.1:0", "--data=" + t.TempDir()}, args...)
	cmd := exec.Command(bin, all...)
	cmd.Dir = t.TempDir()
	cmd.Env = gittyEnv([]string{"GITLAB_TOKEN=hub-token"})
	pipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &runningHub{t: t, cmd: cmd}
	t.Cleanup(h.stop)
	addr, listed := make(chan string, 1), make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			line := sc.Text()
			h.mu.Lock()
			h.stderr.WriteString(line + "\n")
			h.mu.Unlock()
			if m := hubAddressRe.FindStringSubmatch(line); m != nil {
				addr <- m[1]
			}
			if strings.Contains(line, " listed ") {
				select {
				case listed <- true:
				default:
				}
			}
		}
	}()
	select {
	case h.url = <-addr:
	case <-time.After(20 * time.Second):
		t.Fatalf("gitty up did not start:\n%s", h.log())
	}
	select {
	case <-listed:
	case <-time.After(20 * time.Second):
		t.Fatalf("gitty up did not list the instance:\n%s", h.log())
	}
	return h
}

func (h *runningHub) log() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stderr.String()
}

func (h *runningHub) stop() {
	if h.cmd.Process != nil && h.cmd.ProcessState == nil {
		h.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { h.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			h.cmd.Process.Kill()
			<-done
		}
	}
}

// tool calls one of the hub's tools the way an agent's host does today:
// one POST, a bearer token, an answer.
func (h *runningHub) tool(token, name, args string) string {
	h.t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args)
	req, _ := http.NewRequest(http.MethodPost, h.url+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s: %v\n%s", name, err, h.log())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Result.Content) == 0 {
		h.t.Fatalf("%s %s: HTTP %d: %s", name, args, resp.StatusCode, raw)
	}
	return out.Result.Content[0].Text
}

// open makes a fixture's groups and projects visible to a hub: it covers
// only what any user of the instance could browse.
func openToAll(f *fakeGitLab) {
	for name, g := range f.groups {
		g.Visibility = "internal"
		f.groups[name] = g
	}
	for _, ps := range f.projects {
		for i := range ps {
			ps[i].Visibility = "internal"
			ps[i].LastActivityAt = time.Now().UTC().Format(time.RFC3339)
		}
	}
}

func TestE2EHub(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	f := fx.f
	openToAll(f)
	delete(f.shared, "acme") // a sync of the group should find only its own
	f.authUser = "dev"
	hub := startHub(t, f, "--group=acme")
	dev := []string{"GITLAB_TOKEN=dev-token"}

	// One developer's machine: only the application is checked out.
	ws1 := t.TempDir()
	if _, stderr, code := runGitty(t, ws1, dev, "init", "--http", "--url="+f.srv.URL, "--hub="+hub.url+"/"); code != 0 {
		t.Fatalf("init --hub: %s", stderr)
	}
	if cfg := readFileT(t, filepath.Join(ws1, ConfigDir, ConfigName)); !strings.Contains(cfg, "hub = '"+hub.url+"'") {
		t.Errorf("config:\n%s", cfg)
	}
	cloneInto(t, f, ws1, "acme/app")
	head := gitOut(t, filepath.Join(ws1, "acme", "app"), "rev-parse", "HEAD")[:9]

	t.Log("a dry run asks the hub what it has, says what would be sent, and sends nothing")
	stdout, _ := mustGitty(t, ws1, dev, "share", "--dry-run")
	wantLines(t, "share --dry-run", stdout, "plan share acme/app "+head, "summary shared=1 skipped=0 errors=0")
	if out := hub.tool("dev-token", "gitty_outline", `{"path":"acme/lib"}`); !strings.Contains(out, "used by 0 projects") {
		t.Errorf("a dry run told the hub something:\n%s", out)
	}

	t.Log("the real run sends what the checkout says; the next has nothing new to say")
	stdout, stderr := mustGitty(t, filepath.Join(ws1, "acme", "app", "src"), dev, "share")
	wantLines(t, "share", stdout, "share acme/app "+head, "summary shared=1 skipped=0 errors=0")
	if !strings.Contains(stderr, "Sharing what the checkouts here say with "+hub.url) {
		t.Errorf("stderr:\n%s", stderr)
	}
	stdout, _ = mustGitty(t, ws1, dev, "share")
	wantLines(t, "share again", stdout, "skip acme/app known", "summary shared=0 skipped=1 errors=0")

	t.Log("an agent anywhere now sees, from the library's side, what uses it and where")
	lib := hub.tool("dev-token", "gitty_outline", `{"path":"acme/lib"}`)
	for _, want := range []string{
		"# acme/lib\nShared domain types  [its description]",
		"Standing: used by 1 project",
		"## Used by (1): each is a place to see how\n- acme/app · depends (package.json:4)",
		"Its repository has not been read",
		"gitty fetch acme/lib",
	} {
		if !strings.Contains(lib, want) {
			t.Errorf("outline of acme/lib is missing %q:\n%s", want, lib)
		}
	}
	tmpl := hub.tool("dev-token", "gitty_outline", `{"path":"acme/ci-templates"}`)
	if !strings.Contains(tmpl, "- acme/app · ci-include templates/build.yml (.gitlab-ci.yml:2)") {
		t.Errorf("outline of acme/ci-templates:\n%s", tmpl)
	}

	t.Log("another machine syncs the group: what it cloned is shared on the way out, and sync's own output is untouched")
	ws2 := t.TempDir()
	mustGitty(t, ws2, dev, "init", "--http", "--url="+f.srv.URL, "--hub="+hub.url)
	stdout, stderr = mustGitty(t, ws2, dev, "sync", "acme", "--jobs=1")
	if !strings.HasSuffix(stdout, "summary cloned=4 pulled=0 skipped=0 errors=0\n") || strings.Contains(stdout, "share") {
		t.Errorf("sync stdout:\n%s", stdout)
	}
	// The application is already known at this commit; the other three are new.
	if !strings.Contains(stderr, "Shared what 3 checkouts say with the hub at "+hub.url) {
		t.Errorf("sync stderr should say what was shared:\n%s", stderr)
	}
	lib = hub.tool("dev-token", "gitty_outline", `{"path":"acme/lib"}`)
	if !strings.Contains(lib, "Shared domain types.  [its README, at ") || !strings.Contains(lib, "Its repository was read at ") {
		t.Errorf("after the second machine shared, the library's own repository speaks:\n%s", lib)
	}
	app := hub.tool("dev-token", "gitty_outline", `{"path":"acme/app"}`)
	for _, want := range []string{
		"## Uses (2)\n- acme/ci-templates · ci-include templates/build.yml (.gitlab-ci.yml:2)\n- acme/lib · depends (package.json:4)",
		"- acme/app-fork · fork (api:forked_from_project)",
		`"Used by" counts the projects whose repositories have been read: 4 of 4 here so far.`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("outline of acme/app is missing %q:\n%s", want, app)
		}
	}
	browse := hub.tool("dev-token", "gitty_browse", `{"path":"acme"}`)
	if !strings.Contains(browse, "- lib — Shared domain types · used by 1") {
		t.Errorf("browse:\n%s", browse)
	}

	t.Log("only a repository's own account is sent: not a branch in progress, not an uncommitted edit")
	libDir := filepath.Join(ws2, "acme", "lib")
	gitRun(t, libDir, "commit", "-q", "--allow-empty", "-m", "local work, not pushed")
	fx.f.pushUpdate(t, fx.app, "acme/app", "src/main.txt", "changed upstream\n")
	appDir := filepath.Join(ws2, "acme", "app")
	gitRun(t, appDir, "pull", "-q", "--ff-only")
	if err := os.WriteFile(filepath.Join(appDir, "README.md"), []byte("# app\n\nEdited, not committed.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _ = mustGitty(t, ws2, dev, "share")
	wantLines(t, "share with work in progress", stdout,
		"skip acme/app uncommitted-changes",
		"skip acme/app-fork known",
		"skip acme/ci-templates known",
		"skip acme/lib not-default-branch-tip",
		"summary shared=0 skipped=4 errors=0",
	)
	gitRun(t, appDir, "checkout", "-q", "--", "README.md")
	newHead := gitOut(t, appDir, "rev-parse", "HEAD")[:9]
	stdout, _ = mustGitty(t, ws2, dev, "share")
	if !strings.Contains(stdout, "share acme/app "+newHead+"\n") {
		t.Errorf("once clean, the new commit is shared:\n%s", stdout)
	}

	t.Log("--no-share: a sync that tells the hub nothing")
	fx.f.pushUpdate(t, fx.app, "acme/app", "src/main.txt", "changed again\n")
	if _, stderr := mustGitty(t, ws2, dev, "sync", "acme", "--jobs=1", "--no-share"); strings.Contains(stderr, "hub") {
		t.Errorf("sync --no-share mentioned the hub:\n%s", stderr)
	}
	if out := hub.tool("dev-token", "gitty_outline", `{"path":"acme/app"}`); !strings.Contains(out, "Its repository was read at "+newHead) {
		t.Errorf("sync --no-share told the hub about the new commit:\n%s", out)
	}
	// A dry run shares nothing either: there is nothing it has done to tell.
	if _, stderr := mustGitty(t, ws2, dev, "sync", "acme", "--jobs=1", "--dry-run"); strings.Contains(stderr, "hub") {
		t.Errorf("sync --dry-run mentioned the hub:\n%s", stderr)
	}

	t.Log("an agent leaves what it learned, signed with the name the instance gives its token")
	said := hub.tool("dev-token", "gitty_note", `{"path":"acme/lib","kind":"caveat","text":"Types here are generated: edit the schema, not the Go files.","evidence":[{"file":"README.md","line":3}]}`)
	if !strings.Contains(said, "Recorded as #1 on acme/lib (caveat).") {
		t.Errorf("gitty_note:\n%s", said)
	}
	lib = hub.tool("dev-token", "gitty_outline", `{"path":"acme/lib"}`)
	if !strings.Contains(lib, `- caveat #1: "Types here are generated: edit the schema, not the Go files." — @dev, today · cites README.md:3 (still stands)`) {
		t.Errorf("the note, as the next agent reads it:\n%s", lib)
	}

	t.Log("a caller the instance does not know is turned away")
	f.authUser = ""
	publishFixtures()
	req, _ := http.NewRequest(http.MethodPost, hub.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer someone-else")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unknown token: %d", resp.StatusCode)
	}
	if _, stderr, code := runGitty(t, ws2, []string{"GITLAB_TOKEN=someone-else"}, "share"); code != 1 || !strings.Contains(stderr, "answered 401") {
		t.Errorf("share with a token the instance does not accept: exit %d\n%s", code, stderr)
	}
	f.authUser = "dev"
	publishFixtures()

	t.Log("with the hub gone, a sync is exactly what it was; only a share fails")
	hub.stop()
	gitRun(t, libDir, "reset", "-q", "--hard", "origin/main")
	stdout, stderr = mustGitty(t, ws2, dev, "sync", "acme", "--jobs=1")
	if !strings.HasSuffix(stdout, "summary cloned=0 pulled=4 skipped=0 errors=0\n") {
		t.Errorf("sync with the hub down, stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "note: not shared with the hub: could not reach the hub") {
		t.Errorf("sync with the hub down should say so and carry on:\n%s", stderr)
	}
	if _, stderr, code := runGitty(t, ws2, dev, "share"); code != 1 || !strings.Contains(stderr, "could not reach the hub at "+hub.url) {
		t.Errorf("share with the hub down: exit %d\n%s", code, stderr)
	}
}

// TestE2EShareRefusals: what is wrong with the invocation is said before
// anything is sent anywhere.
func TestE2EShareRefusals(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	f := fx.f
	openToAll(f)
	f.authUser = "dev"
	ws := initWorkspace(t, f)
	cloneInto(t, f, ws, "acme/app")
	dev := []string{"GITLAB_TOKEN=dev-token"}

	cases := map[string]struct {
		env  []string
		args []string
		want string
	}{
		"no hub named":            {dev, []string{"share"}, "names no hub"},
		"not an address":          {dev, []string{"share", "--hub=gitty.example.com"}, "not the address of a hub"},
		"a token over plain http": {dev, []string{"share", "--hub=http://gitty.example.com"}, "plain http"},
		"no credential":           {nil, []string{"share", "--hub=https://gitty.example.com"}, "token"},
		"an argument":             {dev, []string{"share", "acme/app"}, "takes no arguments"},
		"jobs":                    {dev, []string{"share", "--jobs=0"}, "--jobs"},
		"init with a bad hub":     {dev, []string{"init", "--force", "--hub=nonsense"}, "not the address of a hub"},
	}
	for name, c := range cases {
		if _, stderr, code := runGitty(t, ws, c.env, c.args...); code != 2 || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exit %d, want 2 and %q\n%s", name, code, c.want, stderr)
		}
	}

	// A hub that serves another instance is not this workspace's hub.
	other := newFakeGitLab(t)
	other.groups["acme"] = apiGroup{ID: 1, FullPath: "acme", Visibility: "internal"}
	hub := startHub(t, other, "--group=acme", "--no-auth")
	if _, stderr, code := runGitty(t, ws, dev, "share", "--hub="+hub.url); code != 2 || !strings.Contains(stderr, "is for "+other.srv.URL) {
		t.Errorf("a hub for another instance: exit %d\n%s", code, stderr)
	}
	// The environment names a hub when the config does not, and a hub that
	// asks nobody who they are is shared with by name alone.
	mine := startHub(t, f, "--group=acme", "--no-auth")
	stdout, _ := mustGitty(t, ws, []string{"GITTY_HUB=" + mine.url}, "share", "--anon")
	if !strings.Contains(stdout, "share acme/app ") || !strings.Contains(stdout, "summary shared=1 skipped=0 errors=0") {
		t.Errorf("share --anon with GITTY_HUB:\n%s", stdout)
	}
	if !strings.Contains(mine.log(), "facts: acme/app at ") || !strings.Contains(mine.log(), "from @anonymous") {
		t.Errorf("the hub's log:\n%s", mine.log())
	}
}

// TestE2EGittyUpIsAPlugin: `gitty up` is gitty-up, found beside gitty.
func TestE2EGittyUpIsAPlugin(t *testing.T) {
	skipIfShort(t)
	bin := gittyWithUp(t)
	out, err := exec.Command(bin, "up", "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "dev" {
		t.Errorf("gitty up --version: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = gittyEnv(nil)
	usage, _ := cmd.CombinedOutput()
	if !strings.Contains(string(usage), "  up ") {
		t.Errorf("gitty with gitty-up beside it should list it:\n%s", usage)
	}
	// Without it, the message says what was looked for and where to get it.
	_, stderr, code := runGitty(t, t.TempDir(), []string{"PATH=" + t.TempDir()}, "up")
	if code != 2 || !strings.Contains(stderr, "gitty-up") {
		t.Errorf("gitty up without the plugin: exit %d\n%s", code, stderr)
	}
}

// --- gitty up --stdio: the server an agent's host starts itself ---

// upSession is a `gitty up --stdio` started the way a host starts it: a
// command, a working directory, an environment, and its stdin and stdout.
type upSession struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	mu     sync.Mutex
	stderr strings.Builder
	lines  []string // every line stdout carried
	next   int
	exited chan struct{}
}

func startUpStdio(t *testing.T, dir string, env []string, args ...string) *upSession {
	t.Helper()
	publishFixtures()
	cmd := exec.Command(gittyWithUp(t), append([]string{"up", "--stdio"}, args...)...)
	cmd.Dir = dir
	cmd.Env = gittyEnv(env)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &upSession{t: t, cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20), exited: make(chan struct{})}
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		sc := bufio.NewScanner(errPipe)
		for sc.Scan() {
			s.mu.Lock()
			s.stderr.WriteString(sc.Text() + "\n")
			s.mu.Unlock()
		}
	}()
	go func() {
		<-logged
		cmd.Wait()
		close(s.exited)
	}()
	t.Cleanup(func() { s.close() })
	return s
}

func (s *upSession) log() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stderr.String()
}

// waitLog waits until the server has said something on stderr.
func (s *upSession) waitLog(want string) {
	s.t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(s.log(), want); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			s.t.Fatalf("gitty up --stdio never logged %q:\n%s", want, s.log())
		}
	}
}

// send writes one message and returns the one line that answers it.
func (s *upSession) send(method, params string) map[string]json.RawMessage {
	s.t.Helper()
	s.next++
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, s.next, method, params)
	if _, err := io.WriteString(s.in, msg+"\n"); err != nil {
		s.t.Fatalf("writing to gitty up --stdio: %v\n%s", err, s.log())
	}
	line, err := s.out.ReadString('\n')
	if err != nil {
		s.t.Fatalf("reading from gitty up --stdio: %v\n%s", err, s.log())
	}
	s.lines = append(s.lines, line)
	var answer map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &answer); err != nil || string(answer["id"]) != fmt.Sprint(s.next) {
		s.t.Fatalf("stdout carried something that is not the answer to request %d: %q\n%s", s.next, line, s.log())
	}
	return answer
}

// tool calls a tool and returns its text.
func (s *upSession) tool(name, args string) string {
	s.t.Helper()
	answer := s.send("tools/call", fmt.Sprintf(`{"name":%q,"arguments":%s}`, name, args))
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(answer["result"], &result); err != nil || len(result.Content) == 0 {
		s.t.Fatalf("%s %s: %s\n%s", name, args, s.lines[len(s.lines)-1], s.log())
	}
	return result.Content[0].Text
}

// close ends the session the way a host does — it closes the server's
// stdin — and returns the exit code.
func (s *upSession) close() int {
	s.in.Close()
	select {
	case <-s.exited:
	case <-time.After(15 * time.Second):
		s.cmd.Process.Kill()
		<-s.exited
		s.t.Errorf("gitty up --stdio did not exit when its stdin closed:\n%s", s.log())
	}
	return s.cmd.ProcessState.ExitCode()
}

// TestE2EUpStdio: one line in a host's configuration — `gitty up --stdio` —
// and an agent has the instance's context, with nothing deployed.
func TestE2EUpStdio(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	f := fx.f
	openToAll(f)
	delete(f.shared, "acme")
	f.authUser = "dev"
	f.topLevel = []apiGroup{f.groups["acme"]}
	dev := []string{"GITLAB_TOKEN=dev-token"}

	ws := initWorkspace(t, f)
	mustGitty(t, ws, dev, "sync", "acme", "--jobs=1")
	data := t.TempDir()
	own := append([]string{"GITTY_UP_DATA=" + data}, dev...)

	t.Log("in a workspace that names no hub, it is a hub of its own: it lists the instance and reads the workspace's checkouts")
	// Started where a host starts it: in the project being worked on.
	s := startUpStdio(t, filepath.Join(ws, "acme", "app", "src"), own)
	init := s.send("initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}`)
	if !strings.Contains(string(init["result"]), `"name":"gitty-up"`) {
		t.Fatalf("initialize: %s", init["result"])
	}
	if list := s.send("tools/list", `{}`); !strings.Contains(string(list["result"]), `"gitty_outline"`) {
		t.Errorf("tools/list: %s", list["result"])
	}
	s.waitLog("read the workspace's checkouts: shared=4 skipped=0 errors=0")
	lib := s.tool("gitty_outline", `{"path":"acme/lib"}`)
	for _, want := range []string{
		"Shared domain types.  [its README, at ",
		"## Used by (1): each is a place to see how\n- acme/app · depends (package.json:4)",
		"Its repository was read at ",
	} {
		if !strings.Contains(lib, want) {
			t.Errorf("outline of acme/lib is missing %q:\n%s", want, lib)
		}
	}
	if said := s.tool("gitty_note", `{"path":"acme/lib","kind":"caveat","text":"Generated types: edit the schema.","evidence":[{"file":"README.md","line":3}]}`); !strings.Contains(said, "Recorded as #1 on acme/lib (caveat).") {
		t.Errorf("gitty_note:\n%s", said)
	}
	if code := s.close(); code != 0 {
		t.Errorf("exit %d\n%s", code, s.log())
	}
	if log := s.log(); strings.Contains(log, "dev-token") || !strings.Contains(log, "notes are signed @dev") {
		t.Errorf("the log:\n%s", log)
	}
	// Whoever watches the server sees what the agent did, a line each.
	for _, want := range []string{
		" hello: e2e 0, MCP 2025-06-18\n",
		" list: 5 tools\n",
		` call @dev gitty_outline {"path":"acme/lib"}: `,
		` call @dev gitty_note {"path":"acme/lib","kind":"caveat","text":"Generated types: edit the schema.","evidence":[{"file":"README.md","line":3}]}: `,
	} {
		if !strings.Contains(s.log(), want) {
			t.Errorf("the log is missing %q:\n%s", want, s.log())
		}
	}
	// It reads checkouts through gitty, and writes into none of them.
	for _, p := range []string{"app", "lib", "ci-templates", "app-fork"} {
		if dirty := gitOut(t, filepath.Join(ws, "acme", p), "status", "--porcelain"); dirty != "" {
			t.Errorf("acme/%s was written to:\n%s", p, dirty)
		}
	}

	t.Log("the next session finds what the last one learned, and learns what has changed since")
	fx.f.pushUpdate(t, fx.app, "acme/app", "src/main.txt", "changed upstream\n")
	mustGitty(t, ws, dev, "sync", "acme", "--jobs=1")
	watched := filepath.Join(t.TempDir(), "up.log")
	s = startUpStdio(t, ws, append([]string{"GITTY_UP_LOG=" + watched}, own...))
	if lib := s.tool("gitty_outline", `{"path":"acme/lib"}`); !strings.Contains(lib, `- caveat #1: "Generated types: edit the schema." — @dev, today · cites README.md:3 (still stands)`) {
		t.Errorf("the note, in the next session:\n%s", lib)
	}
	s.waitLog("read the workspace's checkouts: shared=1 skipped=3 errors=0")
	head := gitOut(t, filepath.Join(ws, "acme", "app"), "rev-parse", "HEAD")[:9]
	if app := s.tool("gitty_outline", `{"path":"acme/app"}`); !strings.Contains(app, "Its repository was read at "+head) {
		t.Errorf("the new commit of acme/app:\n%s", app)
	}
	s.close()
	// A host keeps a server's stderr to itself; the file named in the
	// environment has the same lines, for `tail -f`.
	if file := readFileT(t, watched); !strings.Contains(file, `] call @dev gitty_outline {"path":"acme/app"}: `) ||
		!strings.Contains(file, "] read the workspace's checkouts: shared=1 skipped=3 errors=0\n") || strings.Contains(file, "dev-token") {
		t.Errorf("the log file:\n%s", file)
	}

	t.Log("in a workspace that names a hub, it serves from that hub, as the token's owner")
	team := startHub(t, f, "--group=acme")
	ws2 := t.TempDir()
	mustGitty(t, ws2, dev, "init", "--http", "--url="+f.srv.URL, "--hub="+team.url)
	s = startUpStdio(t, ws2, own)
	s.waitLog("bridging to gitty-up dev at " + team.url)
	// The team's hub has been told nothing yet, and what the other
	// workspace's own hub learned stays in it.
	if lib := s.tool("gitty_outline", `{"path":"acme/lib"}`); strings.Contains(lib, "Generated types") || !strings.Contains(lib, "Its repository has not been read") {
		t.Errorf("outline from the team's hub:\n%s", lib)
	}
	if said := s.tool("gitty_note", `{"path":"acme/lib","kind":"usage","text":"Import the v2 package."}`); !strings.Contains(said, "Recorded as #1 on acme/lib (usage).") {
		t.Errorf("gitty_note through the bridge:\n%s", said)
	}
	if lib := team.tool("dev-token", "gitty_outline", `{"path":"acme/lib"}`); !strings.Contains(lib, `"Import the v2 package." — @dev`) {
		t.Errorf("the note, in the team's hub:\n%s", lib)
	}
	if code := s.close(); code != 0 {
		t.Errorf("exit %d\n%s", code, s.log())
	}

	t.Log("--own keeps to itself even there")
	s = startUpStdio(t, ws2, own, "--own")
	s.waitLog("listed ")
	if lib := s.tool("gitty_outline", `{"path":"acme/lib"}`); !strings.Contains(lib, "Generated types: edit the schema.") {
		t.Errorf("outline from the hub of one's own:\n%s", lib)
	}
	s.close()

	t.Log("what is wrong is said on stderr, and stdout stays the protocol's")
	for name, c := range map[string]struct {
		dir  string
		env  []string
		args []string
		want string
	}{
		"no token":            {ws, nil, nil, "a token to list the instance with is required"},
		"no token for a hub":  {ws2, nil, nil, "a GitLab token is required to speak to the hub"},
		"outside a workspace": {t.TempDir(), dev, nil, "Start this server inside a gitty workspace"},
	} {
		cmd := exec.Command(gittyWithUp(t), append([]string{"up", "--stdio"}, c.args...)...)
		cmd.Dir = c.dir
		cmd.Env = gittyEnv(c.env)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		cmd.Run()
		if code := cmd.ProcessState.ExitCode(); code != 2 || !strings.Contains(stderr.String(), c.want) || stdout.Len() != 0 {
			t.Errorf("%s: exit %d, stdout %q\n%s", name, code, stdout.String(), stderr.String())
		}
	}
}
