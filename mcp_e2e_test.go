package main

// End-to-end tests of the MCP plugin: the real gitty finding and starting the
// real gitty-mcp, a client talking to it over stdin and stdout, real git, and
// the fake GitLab behind it all.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitty/internal/contract"
)

var (
	mcpBuildOnce sync.Once
	mcpGittyBin  string
	mcpBuildErr  error
)

// gittyWithMCP returns a gitty that has the MCP plugin installed the way a
// user installs it: gitty-mcp sitting next to gitty. It is a copy of the
// shared e2e binary in a directory of its own, so that the other tests keep
// a gitty with no plugins.
func gittyWithMCP(t *testing.T) string {
	t.Helper()
	mcpBuildOnce.Do(func() {
		dir := filepath.Join(e2eBinDir, "with-mcp")
		if mcpBuildErr = os.MkdirAll(dir, 0o755); mcpBuildErr != nil {
			return
		}
		var data []byte
		if data, mcpBuildErr = os.ReadFile(gittyBin); mcpBuildErr != nil {
			return
		}
		mcpGittyBin = filepath.Join(dir, "gitty")
		if mcpBuildErr = os.WriteFile(mcpGittyBin, data, 0o755); mcpBuildErr != nil {
			return
		}
		out, err := exec.Command("go", "build", "-o", filepath.Join(dir, "gitty-mcp"), "./cmd/gitty-mcp").CombinedOutput()
		if err != nil {
			mcpBuildErr = err
			t.Logf("building gitty-mcp: %s", out)
		}
	})
	if mcpBuildErr != nil {
		t.Fatalf("setting up gitty with the MCP plugin: %v", mcpBuildErr)
	}
	return mcpGittyBin
}

// mcpSession is a running `gitty mcp` and the client end of its pipes.
type mcpSession struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *bufio.Reader
	stderr *bytes.Buffer
	nextID int
}

// startMCP starts `gitty <args>` in dir — args end in "mcp" and its flags —
// and performs the handshake.
func startMCP(t *testing.T, dir string, args ...string) *mcpSession {
	t.Helper()
	publishFixtures()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gittyWithMCP(t), args...)
	cmd.Dir = dir
	cmd.Env = gittyEnv(nil)
	cmd.Stdin, cmd.Stdout = inR, outW
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting gitty %v: %v", args, err)
	}
	inR.Close()
	outW.Close()
	s := &mcpSession{t: t, cmd: cmd, stdin: inW, stdout: bufio.NewReaderSize(outR, 1<<20), stderr: stderr}
	t.Cleanup(func() {
		inW.Close()
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			cmd.Process.Kill()
			t.Errorf("the server did not exit when its input was closed\nstderr:\n%s", stderr.String())
		}
		outR.Close()
	})

	var init struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	s.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "e2e", "version": "0"},
	}, &init)
	if init.ServerInfo.Name != "gitty-mcp" {
		t.Fatalf("handshake: server is %q\nstderr:\n%s", init.ServerInfo.Name, stderr.String())
	}
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return s
}

func (s *mcpSession) send(msg map[string]any) {
	s.t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.stdin.Write(append(data, '\n')); err != nil {
		s.t.Fatalf("writing to the server: %v\nstderr:\n%s", err, s.stderr.String())
	}
}

// request sends a request and decodes its result into out. Everything the
// server writes to stdout must be a protocol message: a line that is not
// fails the test.
func (s *mcpSession) request(method string, params any, out any) {
	s.t.Helper()
	s.nextID++
	msg := map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method}
	if params != nil {
		msg["params"] = params
	}
	s.send(msg)

	type reply struct {
		line []byte
		err  error
	}
	ch := make(chan reply, 1)
	go func() {
		line, err := s.stdout.ReadBytes('\n')
		ch <- reply{line, err}
	}()
	var r reply
	select {
	case r = <-ch:
	case <-time.After(60 * time.Second):
		s.t.Fatalf("no answer to %s\nstderr:\n%s", method, s.stderr.String())
	}
	if r.err != nil {
		s.t.Fatalf("reading the answer to %s: %v\nstderr:\n%s", method, r.err, s.stderr.String())
	}
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.line, &resp); err != nil || resp.JSONRPC != "2.0" {
		s.t.Fatalf("stdout carried something that is not a protocol message: %q", r.line)
	}
	if string(resp.ID) != strconv.Itoa(s.nextID) {
		s.t.Fatalf("answer to %s has id %s, want %d", method, resp.ID, s.nextID)
	}
	if resp.Error != nil {
		s.t.Fatalf("%s: error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
	if out != nil {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			s.t.Fatalf("decoding the result of %s: %v\n%s", method, err, resp.Result)
		}
	}
}

// mcpToolResult is a tool call's result as a client sees it.
type mcpToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func (r mcpToolResult) text() string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

// call invokes a tool; into, when given, receives its structured content.
func (s *mcpSession) call(tool string, args any, into any) mcpToolResult {
	s.t.Helper()
	params := map[string]any{"name": tool}
	if args != nil {
		params["arguments"] = args
	}
	var r mcpToolResult
	s.request("tools/call", params, &r)
	if into != nil {
		if r.IsError {
			s.t.Fatalf("%s failed: %s\nstderr:\n%s", tool, r.text(), s.stderr.String())
		}
		if err := json.Unmarshal(r.StructuredContent, into); err != nil {
			s.t.Fatalf("decoding what %s returned: %v\n%s", tool, err, r.StructuredContent)
		}
	}
	return r
}

func (s *mcpSession) toolNames() string {
	s.t.Helper()
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	s.request("tools/list", nil, &list)
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	return strings.Join(names, " ")
}

// TestE2EMCPContextFetches is the loop the feature exists for: an agent in
// one checkout asks what else matters, and gets back paths that exist —
// because the projects were cloned into place — with the reason for each.
func TestE2EMCPContextFetches(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	f := fx.f
	ws := initWorkspace(t, f)
	cloneInto(t, f, ws, "acme/app")
	appDir := filepath.Join(ws, "acme", "app")

	s := startMCP(t, filepath.Join(appDir, "src"), "mcp", "--anon")
	if got := s.toolNames(); got != "gitty_context gitty_related gitty_projects gitty_fetch gitty_search gitty_read" {
		t.Errorf("tools = %s", got)
	}

	t.Log("one call: the related projects are cloned and their paths returned")
	var b contract.Briefing
	s.call("gitty_context", nil, &b)
	if b.Subject.Project != "acme/app" || !b.Subject.Present || len(b.Uses) != 2 {
		t.Fatalf("briefing = %+v", b)
	}
	ci, lib := b.Uses[0], b.Uses[1]
	for _, r := range []contract.Related{ci, lib} {
		if !r.Present || !r.Fetched || !filepath.IsAbs(r.Path) {
			t.Errorf("%s = present %v, fetched %v, path %q; want it cloned by this call", r.Project, r.Present, r.Fetched, r.Path)
		}
	}
	if lib.Project != "acme/lib" || lib.Description != "Shared domain types" ||
		len(lib.Links) != 1 || lib.Links[0].Kind != contract.KindDepends || lib.Links[0].Evidence != "package.json:4" ||
		strings.Join(lib.Docs, ",") != "AGENTS.md,README.md" {
		t.Errorf("acme/lib = %+v", lib)
	}
	if ci.Project != "acme/ci-templates" || ci.Links[0].Kind != contract.KindCIInclude || ci.Links[0].Evidence != ".gitlab-ci.yml:2" {
		t.Errorf("acme/ci-templates = %+v", ci)
	}
	// The path leads to the project, in the place a sync would have put it.
	if lib.Path != filepath.Join(b.Workspace, "acme", "lib") {
		t.Errorf("acme/lib is at %q, want its namespace path under %q", lib.Path, b.Workspace)
	}
	if got := readFileT(t, filepath.Join(lib.Path, "lib.txt")); got != "needle in the library\n" {
		t.Errorf("lib.txt at the returned path = %q", got)
	}
	if len(b.Unresolved) != 1 || b.Unresolved[0].Ref != "acme/old-lib" {
		t.Errorf("unresolved = %+v", b.Unresolved)
	}
	if b.Coverage.Projects != 4 || b.Coverage.CheckedOut != 3 {
		t.Errorf("coverage = %+v", b.Coverage)
	}

	t.Log("a second call finds them in place and clones nothing")
	hits := f.gitHits.Load()
	s.call("gitty_context", nil, &b)
	if len(b.Uses) != 2 || b.Uses[0].Fetched || b.Uses[1].Fetched || !b.Uses[1].Present {
		t.Errorf("second call: %+v", b.Uses)
	}
	if f.gitHits.Load() != hits {
		t.Errorf("the second call contacted the git server (%d requests)", f.gitHits.Load()-hits)
	}

	t.Log("the other side, for another project, by name and by directory")
	for _, project := range []string{"acme/lib", lib.Path} {
		var view struct {
			Subject struct {
				Project string `json:"project"`
			} `json:"subject"`
			UsedBy []struct {
				Project string          `json:"project"`
				Links   []contract.Link `json:"links"`
			} `json:"used_by"`
		}
		s.call("gitty_related", map[string]any{"project": project, "direction": "used-by"}, &view)
		if view.Subject.Project != "acme/lib" || len(view.UsedBy) != 1 || view.UsedBy[0].Project != "acme/app" ||
			view.UsedBy[0].Links[0].Evidence != "package.json:4" {
			t.Errorf("gitty_related(%s) = %+v", project, view)
		}
	}

	t.Log("looking inside the related projects")
	var found struct {
		Matches []struct {
			Project string `json:"project"`
			File    string `json:"file"`
			Line    int    `json:"line"`
			Text    string `json:"text"`
		} `json:"matches"`
		Searched int `json:"searched"`
	}
	s.call("gitty_search", map[string]any{"pattern": "needle"}, &found)
	if len(found.Matches) != 1 || found.Matches[0].Project != "acme/lib" || found.Matches[0].File != "lib.txt" ||
		found.Matches[0].Line != 1 || found.Searched != 3 {
		t.Errorf("gitty_search = %+v", found)
	}
	if got := s.call("gitty_read", map[string]any{"project": "acme/lib", "path": "AGENTS.md"}, nil); got.IsError ||
		got.text() != "acme/lib:AGENTS.md (1 lines)\nRun `make test` before committing.\n" {
		t.Errorf("gitty_read = %q (error %v)", got.text(), got.IsError)
	}
	if got := s.call("gitty_read", map[string]any{"project": "acme/lib", "path": "../app/package.json"}, nil); !got.IsError {
		t.Errorf("gitty_read left the checkout: %q", got.text())
	}
	if got := s.call("gitty_read", map[string]any{"project": "acme/lib", "path": ".git/config"}, nil); !got.IsError {
		t.Errorf("gitty_read read the repository's own files: %q", got.text())
	}

	t.Log("finding a project that is not here, and fetching it")
	var projects struct {
		Total    int `json:"total"`
		Projects []struct {
			Project string `json:"project"`
			Present bool   `json:"present"`
			Path    string `json:"path"`
		} `json:"projects"`
	}
	s.call("gitty_projects", map[string]any{"query": "fork"}, &projects)
	if projects.Total != 1 || projects.Projects[0].Project != "acme/app-fork" || projects.Projects[0].Present {
		t.Errorf("gitty_projects = %+v", projects)
	}
	var report contract.FetchReport
	s.call("gitty_fetch", map[string]any{"projects": []string{"acme/app-fork", "acme/lib"}}, &report)
	if len(report.Results) != 2 || !report.Results[0].Fetched || !report.Results[0].Present ||
		report.Results[1].Fetched || !report.Results[1].Present {
		t.Errorf("gitty_fetch = %+v", report.Results)
	}
	if got := readFileT(t, filepath.Join(report.Results[0].Path, "README.md")); got != "# app, forked\n" {
		t.Errorf("the fetched fork's README = %q", got)
	}

	t.Log("a tool error is an answer, not a broken connection")
	if got := s.call("gitty_context", map[string]any{"project": "acme/nope"}, nil); !got.IsError || !strings.Contains(got.text(), "acme/nope") {
		t.Errorf("unknown project: error %v, %q", got.IsError, got.text())
	}
	s.call("gitty_context", map[string]any{"fetch": false}, &b)
	if b.Subject.Project != "acme/app" {
		t.Errorf("the server stopped answering after a tool error: %+v", b.Subject)
	}

	// Nothing a token could hide in was produced; and what gitty reported
	// while cloning went to stderr, never into the protocol.
	if !strings.Contains(s.stderr.String(), "clone acme/lib\n") {
		t.Errorf("stderr should carry gitty's events:\n%s", s.stderr.String())
	}
}

// TestE2EMCPReadOnly: a server started with --no-fetch reports what is
// absent and clones nothing, whatever it is asked.
func TestE2EMCPReadOnly(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	ws := initWorkspace(t, fx.f)
	cloneInto(t, fx.f, ws, "acme/app")

	s := startMCP(t, filepath.Join(ws, "acme", "app"), "mcp", "--no-fetch", "--anon")
	if got := s.toolNames(); got != "gitty_context gitty_related gitty_projects gitty_search gitty_read" {
		t.Errorf("tools = %s", got)
	}
	hits := fx.f.gitHits.Load()
	var b contract.Briefing
	s.call("gitty_context", map[string]any{"fetch": true}, &b)
	if len(b.Uses) != 2 {
		t.Fatalf("uses = %+v", b.Uses)
	}
	for _, r := range b.Uses {
		if r.Present || r.Fetched || r.Path != "" || r.FetchCommand != "gitty fetch "+r.Project {
			t.Errorf("%s = %+v; want absent, with the command that would fetch it", r.Project, r)
		}
	}
	if !strings.Contains(strings.Join(b.Notes, "\n"), "read-only") {
		t.Errorf("notes = %v", b.Notes)
	}
	for _, p := range []string{"lib", "ci-templates"} {
		if _, err := os.Stat(filepath.Join(ws, "acme", p)); !os.IsNotExist(err) {
			t.Errorf("a read-only server cloned acme/%s", p)
		}
	}
	if fx.f.gitHits.Load() != hits {
		t.Error("a read-only server contacted the git server")
	}
}

// TestE2EMCPStartedElsewhere: an agent host rarely starts a server in the
// project the agent is working in. `gitty -C <dir> mcp` says which one.
func TestE2EMCPStartedElsewhere(t *testing.T) {
	skipIfShort(t)
	fx := newLinkedFixture(t)
	ws := initWorkspace(t, fx.f)
	cloneInto(t, fx.f, ws, "acme/app", "acme/lib")
	elsewhere := t.TempDir()

	s := startMCP(t, elsewhere, "-C", filepath.Join(ws, "acme", "lib"), "mcp", "--no-fetch", "--anon")
	var b contract.Briefing
	s.call("gitty_context", nil, &b)
	if b.Subject.Project != "acme/lib" || len(b.UsedBy) != 1 || b.UsedBy[0].Project != "acme/app" {
		t.Errorf("briefing = %+v", b)
	}

	// Outside any workspace there is nothing to serve, and gitty says how to
	// point it at one — on stderr, with nothing on stdout a client would
	// mistake for protocol.
	cmd := exec.Command(gittyWithMCP(t), "mcp")
	cmd.Dir = elsewhere
	cmd.Env = gittyEnv(nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "gitty -C") {
		t.Errorf("outside a workspace: err %v, stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
}

// TestE2EAgentSchemaListsPlugins: the schema an agent reads to learn gitty
// says that `gitty mcp` exists, whether it is installed, and — when it is —
// what the server offers.
func TestE2EAgentSchemaListsPlugins(t *testing.T) {
	skipIfShort(t)
	decode := func(stdout string) []AgentPlugin {
		t.Helper()
		var schema AgentSchema
		if err := json.Unmarshal([]byte(stdout), &schema); err != nil {
			t.Fatalf("decoding the schema: %v", err)
		}
		return schema.Plugins
	}
	// A PATH with no plugins on it, whatever the machine running the tests has.
	emptyPath := []string{"PATH=" + t.TempDir()}

	stdout, _, code := runGittyAt(t, gittyBin, t.TempDir(), "", emptyPath, "agent", "schema")
	plugins := decode(stdout)
	if code != 0 || len(plugins) != 1 || plugins[0].Name != "mcp" || plugins[0].Installed ||
		!strings.Contains(plugins[0].Install, releasesURL) || len(plugins[0].Tools) != 0 ||
		strings.Join(plugins[0].Command, " ") != "gitty mcp" {
		t.Errorf("without the plugin (exit %d): %+v", code, plugins)
	}

	bin := gittyWithMCP(t)
	stdout, _, code = runGittyAt(t, bin, t.TempDir(), "", emptyPath, "agent", "schema")
	plugins = decode(stdout)
	if code != 0 || len(plugins) != 1 || !plugins[0].Installed || plugins[0].Install != "" ||
		plugins[0].Path != filepath.Join(filepath.Dir(bin), "gitty-mcp") {
		t.Fatalf("with the plugin (exit %d): %+v", code, plugins)
	}
	var names []string
	for _, tool := range plugins[0].Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
	}
	if got := strings.Join(names, " "); got != "gitty_context gitty_related gitty_projects gitty_fetch gitty_search gitty_read" {
		t.Errorf("the plugin's tools = %s", got)
	}

	// `gitty mcp --version` is the plugin's, reached through gitty.
	stdout, _, code = runGittyAt(t, bin, t.TempDir(), "", emptyPath, "mcp", "--version")
	if code != 0 || strings.TrimSpace(stdout) == "" {
		t.Errorf("gitty mcp --version: exit %d, %q", code, stdout)
	}
}
