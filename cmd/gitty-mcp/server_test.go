package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitty/internal/contract"
)

// The server is tested against a stand-in for gitty: this test binary, run
// again with an environment variable that makes it behave like one. It
// records the arguments it was given and prints whatever the test laid out
// for that command, which is all the server ever sees of gitty.
const stubEnv = "GITTY_MCP_TEST_STUB_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(stubEnv); dir != "" {
		stubGitty(dir)
		return
	}
	os.Exit(m.Run())
}

// stubGitty is the stand-in. For the command "x" (its first argument) it
// appends its arguments to <dir>/calls, copies <dir>/x.stdout and x.stderr
// to its own, and exits with the code in <dir>/x.exit. If <dir>/x.block
// exists it writes <dir>/x.started and waits to be interrupted, as a gitty
// in the middle of a clone would. If <dir>/x.index exists it is installed as
// the workspace index first, as `gitty index` would write one.
func stubGitty(dir string) {
	args := os.Args[1:]
	if f, err := os.OpenFile(filepath.Join(dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	name := args[0]
	if data, err := os.ReadFile(filepath.Join(dir, name+".index")); err == nil {
		os.WriteFile(filepath.Join(os.Getenv("GITTY_MCP_TEST_WORKSPACE"), ".gitty", "index.json"), data, 0o644)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".block")); err == nil {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		os.WriteFile(filepath.Join(dir, name+".started"), nil, 0o644)
		select {
		case <-sig:
			os.WriteFile(filepath.Join(dir, name+".interrupted"), nil, 0o644)
			os.Exit(130)
		case <-time.After(30 * time.Second):
			os.Exit(3)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, name+".stderr")); err == nil {
		os.Stderr.Write(data)
	}
	if data, err := os.ReadFile(filepath.Join(dir, name+".stdout")); err == nil {
		os.Stdout.Write(data)
	}
	code := 0
	if data, err := os.ReadFile(filepath.Join(dir, name+".exit")); err == nil {
		code, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	os.Exit(code)
}

// fixture is a server over a temporary workspace and a stub gitty.
type fixture struct {
	t    *testing.T
	s    *server
	stub string // the stub's directory
	ws   string // the workspace root
	logs *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".gitty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".gitty", "config"), []byte("url = \"https://gitlab.example.com\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := t.TempDir()
	t.Setenv(stubEnv, stub)
	t.Setenv("GITTY_MCP_TEST_WORKSPACE", ws)
	logs := &bytes.Buffer{}
	return &fixture{
		t: t, stub: stub, ws: ws, logs: logs,
		s: &server{gittyBin: exe, workspace: ws, dir: ws, errOut: logs},
	}
}

// lay sets what the stub prints for a command.
func (f *fixture) lay(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.stub, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// layJSON sets what the stub prints on stdout for a command, as JSON.
func (f *fixture) layJSON(command string, v any) {
	f.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	f.lay(command+".stdout", string(data))
}

// calls returns the command lines the stub has been run with.
func (f *fixture) calls() []string {
	data, err := os.ReadFile(filepath.Join(f.stub, "calls"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// lastCall returns the most recent command line, or "".
func (f *fixture) lastCall() string {
	c := f.calls()
	if len(c) == 0 {
		return ""
	}
	return c[len(c)-1]
}

// writeIndex installs a workspace index.
func (f *fixture) writeIndex(idx contract.Index) {
	f.t.Helper()
	data, err := json.Marshal(idx)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.ws, ".gitty", "index.json"), data, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// client talks to a running server the way a host does: lines in, lines out.
type client struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Reader
	done   chan error
	nextID int
}

type wireResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (f *fixture) connect() *client {
	f.t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{t: f.t, in: inW, out: bufio.NewReaderSize(outR, 1<<20), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		c.done <- f.s.serve(ctx, inR, outW)
		outW.Close()
	}()
	f.t.Cleanup(func() {
		inW.Close()
		cancel()
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
			f.t.Error("the server did not stop")
		}
	})
	return c
}

// send writes one raw line. A server that has stopped reading fails the
// test rather than hanging it.
func (c *client) send(line string) {
	c.t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(c.in, line+"\n")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			c.t.Fatalf("writing to the server: %v", err)
		}
	case <-time.After(20 * time.Second):
		c.t.Fatal("the server is not reading its input")
	}
}

// read returns the next line the server wrote, decoded.
func (c *client) read() wireResponse {
	c.t.Helper()
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := c.out.ReadBytes('\n')
		ch <- result{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			c.t.Fatalf("reading from the server: %v", r.err)
		}
		if bytes.Count(r.line, []byte("\n")) != 1 {
			c.t.Fatalf("a message must be exactly one line: %q", r.line)
		}
		var resp wireResponse
		if err := json.Unmarshal(r.line, &resp); err != nil {
			c.t.Fatalf("the server wrote something that is not a message: %q (%v)", r.line, err)
		}
		return resp
	case <-time.After(20 * time.Second):
		c.t.Fatal("no answer from the server")
	}
	return wireResponse{}
}

// request sends a request and returns its response.
func (c *client) request(method string, params any) wireResponse {
	c.t.Helper()
	c.nextID++
	msg := map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	c.send(string(data))
	resp := c.read()
	if string(resp.ID) != strconv.Itoa(c.nextID) {
		c.t.Fatalf("response id = %s, want %d", resp.ID, c.nextID)
	}
	return resp
}

// result is a decoded tool result.
type result struct {
	Content           []toolContent   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func (r result) text() string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

// call invokes a tool and returns its result; a protocol error fails the test.
func (c *client) call(name string, args any) result {
	c.t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	resp := c.request("tools/call", params)
	if resp.Error != nil {
		c.t.Fatalf("tools/call %s: protocol error %d: %s", name, resp.Error.Code, resp.Error.Message)
	}
	var r result
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		c.t.Fatalf("decoding the result of %s: %v", name, err)
	}
	if len(r.Content) == 0 || r.Content[0].Type != "text" {
		c.t.Fatalf("%s returned no text content: %s", name, resp.Result)
	}
	return r
}

// mustFail calls a tool and requires a tool error mentioning want.
func (c *client) mustFail(name string, args any, want string) {
	c.t.Helper()
	r := c.call(name, args)
	if !r.IsError || !strings.Contains(r.text(), want) {
		c.t.Errorf("%s(%v): isError=%v, text %q; want an error mentioning %q", name, args, r.IsError, r.text(), want)
	}
}

// --- the protocol ---

func TestHandshakeAndToolList(t *testing.T) {
	f := newFixture(t)
	c := f.connect()

	resp := c.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct{} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(resp.Result, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != "2025-06-18" || init.Capabilities.Tools == nil ||
		init.ServerInfo.Name != "gitty-mcp" || init.ServerInfo.Version == "" || !strings.Contains(init.Instructions, "gitty_context") {
		t.Errorf("initialize result = %s", resp.Result)
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	if resp := c.request("ping", nil); resp.Error != nil || string(resp.Result) != "{}" {
		t.Errorf("ping = %s / %v", resp.Result, resp.Error)
	}

	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"inputSchema"`
			Annotations struct {
				ReadOnlyHint    bool `json:"readOnlyHint"`
				DestructiveHint bool `json:"destructiveHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.request("tools/list", nil).Result, &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	readOnly := map[string]bool{}
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		readOnly[tool.Name] = tool.Annotations.ReadOnlyHint
		if tool.Description == "" || tool.InputSchema.Type != "object" || tool.Annotations.DestructiveHint {
			t.Errorf("tool %s is not fully described: %+v", tool.Name, tool)
		}
	}
	if got := strings.Join(names, " "); got != "gitty_context gitty_related gitty_projects gitty_fetch gitty_search gitty_read" {
		t.Errorf("tools = %s", got)
	}
	// Only the two that can clone are not read-only.
	for name, want := range map[string]bool{"gitty_context": false, "gitty_fetch": false, "gitty_related": true, "gitty_search": true, "gitty_read": true, "gitty_projects": true} {
		if readOnly[name] != want {
			t.Errorf("%s readOnlyHint = %v, want %v", name, readOnly[name], want)
		}
	}
}

func TestVersionNegotiation(t *testing.T) {
	for requested, want := range map[string]string{
		"2025-11-25": "2025-11-25",
		"2025-06-18": "2025-06-18",
		"2024-11-05": "2024-11-05",
		// A revision this server does not know: it offers the newest it does.
		"2099-01-01": protocolVersions[0],
		"":           protocolVersions[0],
	} {
		if got := negotiateVersion(requested); got != want {
			t.Errorf("negotiateVersion(%q) = %q, want %q", requested, got, want)
		}
	}
}

func TestReadOnlyServerOffersNoFetch(t *testing.T) {
	f := newFixture(t)
	f.s.noFetch = true
	c := f.connect()
	resp := c.request("tools/list", nil)
	if strings.Contains(string(resp.Result), `"gitty_fetch"`) {
		t.Error("a read-only server must not offer gitty_fetch")
	}
	if strings.Contains(string(resp.Result), `"readOnlyHint":false`) {
		t.Errorf("every tool of a read-only server is read-only: %s", resp.Result)
	}
	// And refuses it if called anyway.
	resp = c.request("tools/call", map[string]any{"name": "gitty_fetch", "arguments": map[string]any{"projects": []string{"acme/lib"}}})
	if resp.Error == nil || resp.Error.Code != codeInvalidParams {
		t.Errorf("gitty_fetch on a read-only server: %s / %+v", resp.Result, resp.Error)
	}
	if len(f.calls()) != 0 {
		t.Errorf("gitty was run: %v", f.calls())
	}
}

func TestProtocolErrors(t *testing.T) {
	f := newFixture(t)
	c := f.connect()

	expect := func(what string, wantCode int, wantID string) {
		t.Helper()
		resp := c.read()
		if resp.Error == nil || resp.Error.Code != wantCode || string(resp.ID) != wantID {
			t.Errorf("%s: id %s, error %+v; want code %d, id %s", what, resp.ID, resp.Error, wantCode, wantID)
		}
	}

	c.send(`{"jsonrpc":"2.0","id":1,"method":`)
	expect("malformed JSON", codeParseError, "null")
	c.send(`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`)
	expect("a method this server does not have", codeMethodNotFound, "2")
	c.send(`{"jsonrpc":"2.0","id":3}`)
	expect("no method", codeInvalidRequest, "3")
	c.send(`{"jsonrpc":"1.0","id":4,"method":"ping"}`)
	expect("wrong jsonrpc version", codeInvalidRequest, "4")
	c.send(`{"jsonrpc":"2.0","id":5,"method":42}`)
	expect("a method that is not a string", codeInvalidRequest, "null")
	c.send(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"gitty_nope"}}`)
	expect("an unknown tool", codeInvalidParams, "6")
	c.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{}}`)
	expect("a tool call without a name", codeInvalidParams, "7")
	c.send(`[]`)
	expect("an empty batch", codeInvalidRequest, "null")
	c.send(`"just a string"`)
	expect("not an object", codeInvalidRequest, "null")
	c.send(`{"jsonrpc":"2.0","id":"abc","method":"ping"}`)
	if resp := c.read(); resp.Error != nil || string(resp.ID) != `"abc"` {
		t.Errorf("a string id must be echoed as it came: %s / %+v", resp.ID, resp.Error)
	}

	// Things that get no answer at all: notifications, known or not, and a
	// response to a request this server never made. The ping after them is
	// the next thing answered.
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.send(`{"jsonrpc":"2.0","method":"notifications/whatever","params":{"x":1}}`)
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":999}}`)
	c.send(`{"jsonrpc":"2.0","id":77,"result":{}}`)
	c.send(``)
	c.send(`   `)
	if resp := c.request("ping", nil); resp.Error != nil {
		t.Errorf("ping after notifications: %+v", resp.Error)
	}

	// A batch is answered as one, notifications in it left out.
	c.send(`[{"jsonrpc":"2.0","id":10,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":11,"method":"nope"}]`)
	line, err := c.out.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var batch []wireResponse
	if err := json.Unmarshal(line, &batch); err != nil || len(batch) != 2 ||
		string(batch[0].ID) != "10" || batch[0].Error != nil || string(batch[1].ID) != "11" || batch[1].Error == nil {
		t.Errorf("batch response = %s (%v)", line, err)
	}

	// A line too long to be a message is refused, and the stream stays in step.
	c.send(`{"jsonrpc":"2.0","id":20,"method":"ping","params":{"pad":"` + strings.Repeat("x", maxMessageBytes) + `"}}`)
	expect("an oversized message", codeParseError, "null")
	if resp := c.request("ping", nil); resp.Error != nil {
		t.Errorf("ping after an oversized message: %+v", resp.Error)
	}

	if len(f.calls()) != 0 {
		t.Errorf("gitty was run during protocol errors: %v", f.calls())
	}
}

// --- gitty_context, gitty_related: gitty's answer, passed on ---

func sampleBriefing() contract.Briefing {
	return contract.Briefing{
		Schema:    contract.Schema,
		Instance:  "https://gitlab.example.com",
		Workspace: "/ws",
		Subject:   contract.Subject{Kind: "project", Project: "acme/app", Present: true, Path: "/ws/acme/app", RelativePath: "."},
		Uses: []contract.Related{{
			Project: "acme/lib", Description: "Shared types", Present: true, Fetched: true, Path: "/ws/acme/lib", RelativePath: "../lib",
			Links: []contract.Link{{From: "acme/app", To: "acme/lib", Kind: contract.KindDepends, Evidence: "go.mod:12"}},
			Docs:  []string{"AGENTS.md", "README.md"}, Summary: "Types.",
		}},
		UsedBy: []contract.Related{{
			Project: "acme/web", Present: true, Path: "/ws/acme/web",
			Links: []contract.Link{{From: "acme/web", To: "acme/app", Kind: contract.KindCITrigger, Evidence: ".gitlab-ci.yml:40"}},
		}},
		UsesTotal: 1, UsedByTotal: 1,
		Unresolved: []contract.Unresolved{{From: "acme/app", Ref: "acme/old", Kind: contract.KindDepends, Evidence: "go.mod:14"}},
		Coverage:   contract.Coverage{Projects: 40, CheckedOut: 3},
	}
}

func TestContextTool(t *testing.T) {
	f := newFixture(t)
	f.layJSON("context", sampleBriefing())
	c := f.connect()

	r := c.call("gitty_context", nil)
	if r.IsError {
		t.Fatalf("gitty_context: %s", r.text())
	}
	var b contract.Briefing
	if err := json.Unmarshal(r.StructuredContent, &b); err != nil {
		t.Fatal(err)
	}
	lib := b.Uses[0]
	if b.Subject.Project != "acme/app" || lib.Path != "/ws/acme/lib" || !lib.Fetched ||
		lib.Links[0].Kind != contract.KindDepends || lib.Links[0].Evidence != "go.mod:12" || b.Coverage.Projects != 40 {
		t.Errorf("structured content = %s", r.StructuredContent)
	}
	// The text is the same data, for clients that only pass text on.
	var again contract.Briefing
	if err := json.Unmarshal([]byte(r.text()), &again); err != nil || again.Uses[0].Path != "/ws/acme/lib" {
		t.Errorf("text content is not the briefing: %v\n%s", err, r.text())
	}
	// By default the server fetches; and it runs gitty where it was started.
	if got := f.lastCall(); got != "context --format=json --fetch" {
		t.Errorf("gitty was run as %q", got)
	}

	for name, tt := range map[string]struct {
		args any
		want string
	}{
		"fetch declined":         {map[string]any{"fetch": false}, "context --format=json"},
		"a project and one side": {map[string]any{"project": "acme/lib", "direction": "used-by"}, "context --format=json --direction=used-by --fetch -- acme/lib"},
		"a directory as project": {map[string]any{"project": "/ws/acme/lib/pkg"}, "context --format=json --fetch -- /ws/acme/lib/pkg"},
		"empty arguments object": {map[string]any{}, "context --format=json --fetch"},
		"explicit fetch":         {map[string]any{"fetch": true}, "context --format=json --fetch"},
		"a project with a space": {map[string]any{"project": "my group/app", "fetch": false}, "context --format=json -- my group/app"},
	} {
		if r := c.call("gitty_context", tt.args); r.IsError || f.lastCall() != tt.want {
			t.Errorf("%s: gitty was run as %q (error=%v), want %q", name, f.lastCall(), r.IsError, tt.want)
		}
	}

	t.Run("bad arguments never reach gitty", func(t *testing.T) {
		before := len(f.calls())
		c.mustFail("gitty_context", map[string]any{"projekt": "acme/lib"}, "projekt")
		c.mustFail("gitty_context", map[string]any{"direction": "sideways"}, "direction")
		c.mustFail("gitty_context", map[string]any{"project": "--token=x"}, "invalid project")
		c.mustFail("gitty_context", map[string]any{"project": "-C"}, "invalid project")
		c.mustFail("gitty_context", map[string]any{"project": "a\nb"}, "invalid project")
		c.mustFail("gitty_context", map[string]any{"fetch": "yes"}, "invalid arguments")
		c.mustFail("gitty_context", []string{"acme/lib"}, "invalid arguments")
		if after := len(f.calls()); after != before {
			t.Errorf("gitty was run %d times for arguments that are not valid: %v", after-before, f.calls()[before:])
		}
	})

	t.Run("a failed fetch is still a briefing", func(t *testing.T) {
		b := sampleBriefing()
		b.Uses[0].Present, b.Uses[0].Fetched, b.Uses[0].Path = false, false, ""
		b.Uses[0].FetchError = "git fetch failed"
		f.layJSON("context", b)
		f.lay("context.exit", "1")
		f.lay("context.stderr", "error acme/lib git fetch failed\n")
		r := c.call("gitty_context", nil)
		if r.IsError || !strings.Contains(r.text(), "git fetch failed") {
			t.Errorf("isError=%v\n%s", r.IsError, r.text())
		}
		// gitty's diagnostics end up in the server's log, not in the protocol.
		if !strings.Contains(f.logs.String(), "error acme/lib git fetch failed") {
			t.Errorf("server log: %q", f.logs.String())
		}
	})

	t.Run("a gitty that fails is an error the model can read", func(t *testing.T) {
		f.lay("context.stdout", "")
		f.lay("context.exit", "2")
		f.lay("context.stderr", "Error: no project or group \"acme/nope\" in this workspace's index\n")
		c.mustFail("gitty_context", map[string]any{"project": "acme/nope"}, `no project or group "acme/nope"`)
	})

	t.Run("a briefing from another release is refused, not guessed at", func(t *testing.T) {
		b := sampleBriefing()
		b.Schema = 99
		f.layJSON("context", b)
		f.lay("context.exit", "0")
		c.mustFail("gitty_context", nil, "version 99")
		f.lay("context.stdout", "this is not JSON")
		c.mustFail("gitty_context", nil, "did not print a briefing")
	})
}

func TestContextToolServerSettings(t *testing.T) {
	f := newFixture(t)
	f.layJSON("context", sampleBriefing())
	f.s.noFetch, f.s.anon, f.s.jobs = true, true, 3
	sub := filepath.Join(f.ws, "acme", "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f.s.dir = sub
	// The stub prints its working directory nowhere, so check it by making
	// the answer depend on it: gitty is run in the server's directory.
	c := f.connect()

	r := c.call("gitty_context", map[string]any{"fetch": true})
	if r.IsError {
		t.Fatal(r.text())
	}
	// Read-only: no --fetch whatever was asked, and the answer says so.
	if got := f.lastCall(); got != "context --anon --format=json --jobs=3" {
		t.Errorf("gitty was run as %q", got)
	}
	if !strings.Contains(r.text(), "read-only") {
		t.Errorf("the briefing should note that nothing was cloned:\n%s", r.text())
	}
}

func TestRelatedTool(t *testing.T) {
	f := newFixture(t)
	f.layJSON("context", sampleBriefing())
	c := f.connect()

	r := c.call("gitty_related", map[string]any{"project": "acme/app"})
	if r.IsError {
		t.Fatal(r.text())
	}
	// Never fetches.
	if got := f.lastCall(); got != "context --format=json -- acme/app" {
		t.Errorf("gitty was run as %q", got)
	}
	var view relatedView
	if err := json.Unmarshal(r.StructuredContent, &view); err != nil {
		t.Fatal(err)
	}
	if view.Subject.Project != "acme/app" || len(view.Uses) != 1 || view.Uses[0].Path != "/ws/acme/lib" ||
		view.Uses[0].Links[0].Evidence != "go.mod:12" || len(view.UsedBy) != 1 || view.UsedByTotal != 1 ||
		len(view.Unresolved) != 1 || view.Coverage.CheckedOut != 3 {
		t.Errorf("view = %s", r.StructuredContent)
	}
	// The links, and not the prose.
	for _, absent := range []string{"Shared types", "AGENTS.md", "Types."} {
		if strings.Contains(r.text(), absent) {
			t.Errorf("gitty_related should not carry %q:\n%s", absent, r.text())
		}
	}
}

func TestFetchTool(t *testing.T) {
	f := newFixture(t)
	f.layJSON("fetch", contract.FetchReport{Schema: contract.Schema, Results: []contract.FetchResult{
		{Project: "acme/lib", Path: "/ws/acme/lib", Present: true, Fetched: true},
		{Project: "acme/gone", Error: "looking the project up failed"},
	}})
	f.lay("fetch.exit", "1") // one of them failed: still a report
	c := f.connect()

	r := c.call("gitty_fetch", map[string]any{"projects": []string{"acme/lib", "acme/gone"}})
	if r.IsError {
		t.Fatal(r.text())
	}
	if got := f.lastCall(); got != "fetch --format=json -- acme/lib acme/gone" {
		t.Errorf("gitty was run as %q", got)
	}
	var report contract.FetchReport
	if err := json.Unmarshal(r.StructuredContent, &report); err != nil || len(report.Results) != 2 ||
		report.Results[0].Path != "/ws/acme/lib" || report.Results[1].Error == "" {
		t.Errorf("report = %s (%v)", r.StructuredContent, err)
	}

	before := len(f.calls())
	c.mustFail("gitty_fetch", map[string]any{"projects": []string{}}, "between 1 and 25")
	c.mustFail("gitty_fetch", nil, "between 1 and 25")
	c.mustFail("gitty_fetch", map[string]any{"projects": []string{"acme/lib", "--dry-run"}}, "invalid project")
	c.mustFail("gitty_fetch", map[string]any{"projects": []string{""}}, "invalid project")
	c.mustFail("gitty_fetch", map[string]any{"projects": make([]string, 26)}, "between 1 and 25")
	if len(f.calls()) != before {
		t.Errorf("gitty was run for invalid arguments: %v", f.calls()[before:])
	}
}

// --- gitty_projects: the index file ---

func sampleIndex() contract.Index {
	return contract.Index{
		Version:  contract.IndexVersion,
		Instance: "https://gitlab.example.com",
		Projects: map[string]*contract.Project{
			"acme/app":          {ID: 1, Description: "Customer-facing API", Topics: []string{"go"}, Local: &contract.Local{Dir: "acme/app"}},
			"acme/lib":          {ID: 2, Description: "Shared domain types", Local: &contract.Local{Dir: "acme/lib"}},
			"acme/web":          {ID: 3, Description: "Web frontend", Topics: []string{"typescript"}},
			"acme/ops/deploy":   {ID: 4, Description: "Deployment pipelines", Archived: true},
			"acme/ops/api-gate": {ID: 5, Description: "The API gateway"},
		},
	}
}

func TestProjectsTool(t *testing.T) {
	f := newFixture(t)
	f.writeIndex(sampleIndex())
	c := f.connect()

	type answer struct {
		Total     int            `json:"total"`
		Truncated bool           `json:"truncated"`
		Projects  []projectEntry `json:"projects"`
	}
	ask := func(args any) answer {
		t.Helper()
		r := c.call("gitty_projects", args)
		if r.IsError {
			t.Fatalf("gitty_projects(%v): %s", args, r.text())
		}
		var a answer
		if err := json.Unmarshal(r.StructuredContent, &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	names := func(a answer) string {
		var out []string
		for _, p := range a.Projects {
			out = append(out, p.Project)
		}
		return strings.Join(out, " ")
	}

	all := ask(nil)
	// Checked-out projects first: they are the ones that can be opened.
	if all.Total != 5 || names(all) != "acme/app acme/lib acme/ops/api-gate acme/ops/deploy acme/web" {
		t.Errorf("all = %d: %s", all.Total, names(all))
	}
	if app := all.Projects[0]; !app.Present || app.Path != filepath.Join(f.ws, "acme", "app") || app.Description != "Customer-facing API" {
		t.Errorf("acme/app = %+v", app)
	}
	if web := all.Projects[4]; web.Present || web.Path != "" {
		t.Errorf("acme/web = %+v, want known but absent", web)
	}

	// Words match in the path, the description and the topics, in any case.
	for query, want := range map[string]string{
		"api":           "acme/app acme/ops/api-gate",
		"API gateway":   "acme/ops/api-gate",
		"typescript":    "acme/web",
		"ops":           "acme/ops/api-gate acme/ops/deploy",
		"nothing-there": "",
	} {
		if got := names(ask(map[string]any{"query": query})); got != want {
			t.Errorf("query %q = %q, want %q", query, got, want)
		}
	}
	if got := names(ask(map[string]any{"checked_out": true})); got != "acme/app acme/lib" {
		t.Errorf("checked_out = %q", got)
	}
	if two := ask(map[string]any{"limit": 2}); len(two.Projects) != 2 || two.Total != 5 || !two.Truncated {
		t.Errorf("limit 2 = %+v", two)
	}
	// Nothing in this tool runs gitty when the index is there.
	if len(f.calls()) != 0 {
		t.Errorf("gitty was run: %v", f.calls())
	}

	t.Run("a changed index is read again", func(t *testing.T) {
		idx := sampleIndex()
		idx.Projects["acme/new"] = &contract.Project{ID: 9, Description: "Just created"}
		time.Sleep(10 * time.Millisecond)
		f.writeIndex(idx)
		if got := names(ask(map[string]any{"query": "just created"})); got != "acme/new" {
			t.Errorf("after the index changed: %q", got)
		}
	})

	t.Run("no index yet: gitty builds one", func(t *testing.T) {
		if err := os.Remove(filepath.Join(f.ws, ".gitty", "index.json")); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(sampleIndex())
		f.lay("index.index", string(data))
		if got := ask(nil); got.Total != 5 {
			t.Errorf("after building: %+v", got)
		}
		if f.lastCall() != "index" {
			t.Errorf("gitty was run as %q", f.lastCall())
		}
	})

	t.Run("an index that cannot be built or read is an error", func(t *testing.T) {
		os.Remove(filepath.Join(f.ws, ".gitty", "index.json"))
		os.Remove(filepath.Join(f.stub, "index.index"))
		f.lay("index.exit", "2")
		f.lay("index.stderr", "Error: a token is required; use --anon\n")
		c.mustFail("gitty_projects", nil, "a token is required")

		idx := sampleIndex()
		idx.Version = 42
		f.writeIndex(idx)
		c.mustFail("gitty_projects", nil, "version 42")
	})
}

// --- gitty_search, gitty_read: real checkouts ---

// git runs git in dir for building a fixture.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// checkoutFixture creates a committed repository at <ws>/<dir>.
func (f *fixture) checkoutFixture(dir string, files map[string]string) string {
	f.t.Helper()
	full := filepath.Join(f.ws, filepath.FromSlash(dir))
	if err := os.MkdirAll(full, 0o755); err != nil {
		f.t.Fatal(err)
	}
	git(f.t, full, "init", "-q", "-b", "main", ".")
	for name, content := range files {
		p := filepath.Join(full, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	git(f.t, full, "add", "-A")
	git(f.t, full, "commit", "-q", "-m", "fixture")
	return full
}

func twoCheckouts(t *testing.T) (*fixture, string, string) {
	f := newFixture(t)
	app := f.checkoutFixture("acme/app", map[string]string{
		"main.go":       "package main\n\nfunc main() {\n\tlib.NewUser(\"a.b\")\n}\n",
		"docs/notes.md": "Calls lib.NewUser on start.\nAlso a.b literally.\n",
		"go.mod":        "module gitlab.example.com/acme/app\n",
		"empty.txt":     "",
	})
	lib := f.checkoutFixture("acme/lib", map[string]string{
		"user.go":  "package lib\n\n// NewUser makes a user.\nfunc NewUser(name string) *User { return nil }\n",
		"many.txt": strings.Repeat("needle\n", 50),
		"long.txt": "start " + strings.Repeat("x", 1000) + " needle-at-the-end\n",
	})
	f.writeIndex(contract.Index{
		Version: contract.IndexVersion,
		Projects: map[string]*contract.Project{
			"acme/app": {ID: 1, Local: &contract.Local{Dir: "acme/app"}},
			"acme/lib": {ID: 2, Local: &contract.Local{Dir: "acme/lib"}},
			"acme/web": {ID: 3},
		},
	})
	return f, app, lib
}

func TestSearchTool(t *testing.T) {
	f, app, lib := twoCheckouts(t)
	// Present on disk, never added: not the project's.
	if err := os.WriteFile(filepath.Join(app, "untracked.txt"), []byte("lib.NewUser in a file git does not track\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Tracked, but not text.
	if err := os.WriteFile(filepath.Join(lib, "blob.bin"), []byte("NewUser\x00\x01\x02"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, lib, "add", "blob.bin")
	git(t, lib, "commit", "-q", "-m", "binary")
	c := f.connect()

	type answer struct {
		Matches   []searchMatch `json:"matches"`
		Searched  int           `json:"searched"`
		Truncated bool          `json:"truncated"`
	}
	search := func(args map[string]any) answer {
		t.Helper()
		r := c.call("gitty_search", args)
		if r.IsError {
			t.Fatalf("gitty_search(%v): %s", args, r.text())
		}
		var a answer
		if err := json.Unmarshal(r.StructuredContent, &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	where := func(a answer) string {
		var out []string
		for _, m := range a.Matches {
			out = append(out, fmt.Sprintf("%s:%s:%d", m.Project, m.File, m.Line))
		}
		return strings.Join(out, " ")
	}

	// Across every checkout, in order: who defines it and who calls it.
	got := search(map[string]any{"pattern": "NewUser"})
	if where(got) != "acme/app:docs/notes.md:1 acme/app:main.go:4 acme/lib:user.go:3 acme/lib:user.go:4" || got.Searched != 2 || got.Truncated {
		t.Errorf("NewUser: %s (searched %d, truncated %v)", where(got), got.Searched, got.Truncated)
	}
	if m := got.Matches[1]; m.Text != "\tlib.NewUser(\"a.b\")" {
		t.Errorf("match text = %q", m.Text)
	}

	for name, tt := range map[string]struct {
		args map[string]any
		want string
	}{
		"one project":             {map[string]any{"pattern": "NewUser", "projects": []string{"acme/lib"}}, "acme/lib:user.go:3 acme/lib:user.go:4"},
		"a regular expression":    {map[string]any{"pattern": `func (main|NewUser)\(`}, "acme/app:main.go:3 acme/lib:user.go:4"},
		"a literal with a dot":    {map[string]any{"pattern": "a.b", "fixed": true}, "acme/app:docs/notes.md:2 acme/app:main.go:4"},
		"regardless of case":      {map[string]any{"pattern": "newuser makes", "ignore_case": true}, "acme/lib:user.go:3"},
		"only some files":         {map[string]any{"pattern": "NewUser", "glob": "*.md"}, "acme/app:docs/notes.md:1"},
		"a pattern with a dash":   {map[string]any{"pattern": "-at-the-end"}, "acme/lib:long.txt:1"},
		"nothing found":           {map[string]any{"pattern": "no-such-thing-anywhere"}, ""},
		"a project by other case": {map[string]any{"pattern": "module", "projects": []string{"/Acme/App/"}}, "acme/app:go.mod:1"},
	} {
		if got := where(search(tt.args)); got != tt.want {
			t.Errorf("%s: %q, want %q", name, got, tt.want)
		}
	}

	// Bounded: by the caller, and in how much of a line comes back.
	capped := search(map[string]any{"pattern": "needle", "max": 7})
	if len(capped.Matches) != 7 || !capped.Truncated {
		t.Errorf("max 7: %d matches, truncated %v", len(capped.Matches), capped.Truncated)
	}
	if def := search(map[string]any{"pattern": "needle"}); len(def.Matches) != 51 || def.Truncated {
		t.Errorf("default limit: %d matches, truncated %v", len(def.Matches), def.Truncated)
	}
	long := search(map[string]any{"pattern": "^start"})
	if n := len([]rune(long.Matches[0].Text)); n != maxMatchRunes+1 {
		t.Errorf("a long line came back as %d runes, want it clipped to %d", n, maxMatchRunes)
	}

	c.mustFail("gitty_search", map[string]any{"pattern": ""}, "pattern is required")
	c.mustFail("gitty_search", map[string]any{"pattern": "x", "projects": []string{"acme/web"}}, "not checked out")
	c.mustFail("gitty_search", map[string]any{"pattern": "x", "projects": []string{"acme/nope"}}, "no project")
	c.mustFail("gitty_search", map[string]any{"pattern": "(unclosed"}, "acme/app")
	if len(f.calls()) != 0 {
		t.Errorf("searching should not run gitty: %v", f.calls())
	}
}

func TestReadTool(t *testing.T) {
	f, app, lib := twoCheckouts(t)
	outside := filepath.Join(f.ws, "secret.txt")
	if err := os.WriteFile(outside, []byte("outside every checkout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, ".env"), []byte("TOKEN=not-tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "blob.bin"), []byte("a\x00b"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, lib, "add", "blob.bin")
	symlinks := runtime.GOOS != "windows"
	if symlinks {
		// Tracked links: one that stays inside the checkout, one that leaves it.
		if err := os.Symlink("user.go", filepath.Join(lib, "alias.go")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(lib, "escape.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(f.ws, "acme", "app"), filepath.Join(lib, "sibling")); err != nil {
			t.Fatal(err)
		}
		git(t, lib, "add", "alias.go", "escape.txt", "sibling")
	}
	git(t, lib, "commit", "-q", "-m", "more")
	c := f.connect()

	read := func(args map[string]any) string {
		t.Helper()
		r := c.call("gitty_read", args)
		if r.IsError {
			t.Fatalf("gitty_read(%v): %s", args, r.text())
		}
		return r.text()
	}

	if got := read(map[string]any{"project": "acme/lib", "path": "user.go"}); got !=
		"acme/lib:user.go (4 lines)\npackage lib\n\n// NewUser makes a user.\nfunc NewUser(name string) *User { return nil }\n" {
		t.Errorf("whole file:\n%s", got)
	}
	if got := read(map[string]any{"project": "acme/lib", "path": "user.go", "start": 3, "end": 3}); got !=
		"acme/lib:user.go lines 3-3 of 4\n// NewUser makes a user.\n" {
		t.Errorf("one line:\n%s", got)
	}
	if got := read(map[string]any{"project": "acme/lib", "path": "many.txt", "start": 49, "end": 999}); !strings.HasPrefix(got, "acme/lib:many.txt lines 49-50 of 50\n") {
		t.Errorf("a range past the end:\n%s", got)
	}
	if got := read(map[string]any{"project": "ACME/App", "path": "./docs//notes.md"}); !strings.HasPrefix(got, "acme/app:docs/notes.md (2 lines)\n") {
		t.Errorf("a path that needs tidying:\n%s", got)
	}
	if got := read(map[string]any{"project": "acme/app", "path": "empty.txt"}); got != "acme/app:empty.txt is empty\n" {
		t.Errorf("an empty file:\n%s", got)
	}
	if symlinks {
		if got := read(map[string]any{"project": "acme/lib", "path": "alias.go"}); !strings.Contains(got, "func NewUser") {
			t.Errorf("a link that stays inside the checkout:\n%s", got)
		}
	}

	// What may not be read. Every refusal names the reason, and none of them
	// returns a byte of the file asked for.
	refusals := []struct {
		what string
		args map[string]any
		want string
	}{
		{"a parent directory", map[string]any{"project": "acme/lib", "path": "../app/main.go"}, `".."`},
		{"a parent directory, deeper", map[string]any{"project": "acme/lib", "path": "docs/../../../secret.txt"}, `".."`},
		{"a parent directory, with backslashes", map[string]any{"project": "acme/lib", "path": `..\app\main.go`}, `".."`},
		{"an absolute path", map[string]any{"project": "acme/lib", "path": outside}, "relative"},
		{"the repository's own files", map[string]any{"project": "acme/lib", "path": ".git/config"}, ".git"},
		{"the repository's own files, other case", map[string]any{"project": "acme/lib", "path": ".GIT/HEAD"}, ".git"},
		{"a file git does not track", map[string]any{"project": "acme/app", "path": ".env"}, "not a file git tracks"},
		{"a file that is not there", map[string]any{"project": "acme/app", "path": "nope.txt"}, "not a file git tracks"},
		{"a directory", map[string]any{"project": "acme/app", "path": "docs"}, "not a file git tracks"},
		{"the root", map[string]any{"project": "acme/app", "path": "."}, "a file is required"},
		{"a pattern instead of a path", map[string]any{"project": "acme/app", "path": "*.go"}, "not a file git tracks"},
		{"a binary file", map[string]any{"project": "acme/lib", "path": "blob.bin"}, "not a text file"},
		{"a line past the end", map[string]any{"project": "acme/lib", "path": "user.go", "start": 99}, "past its end"},
		{"a project that is not checked out", map[string]any{"project": "acme/web", "path": "x"}, "not checked out"},
		{"a project that does not exist", map[string]any{"project": "acme/nope", "path": "x"}, "no project"},
		{"no project", map[string]any{"path": "x"}, "project is required"},
		{"no path", map[string]any{"project": "acme/lib"}, "path relative"},
	}
	if symlinks {
		refusals = append(refusals,
			struct {
				what string
				args map[string]any
				want string
			}{"a tracked link that leaves the checkout", map[string]any{"project": "acme/lib", "path": "escape.txt"}, "outside the project"},
			struct {
				what string
				args map[string]any
				want string
			}{"a file reached through a link to another checkout", map[string]any{"project": "acme/lib", "path": "sibling/main.go"}, "not a file git tracks"},
		)
	}
	for _, tt := range refusals {
		r := c.call("gitty_read", tt.args)
		if !r.IsError || !strings.Contains(r.text(), tt.want) {
			t.Errorf("%s: isError=%v, %q; want a refusal mentioning %q", tt.what, r.IsError, r.text(), tt.want)
		}
		for _, leaked := range []string{"outside every checkout", "not-tracked", "package main", "[core]"} {
			if strings.Contains(r.text(), leaked) {
				t.Errorf("%s: the refusal leaked file content: %q", tt.what, r.text())
			}
		}
	}
}

// --- calls in flight ---

// A call the client gives up on is stopped: gitty is interrupted, nothing is
// answered for it, and the server goes on serving.
func TestCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("interrupting a child process is not available")
	}
	f := newFixture(t)
	f.lay("context.block", "")
	c := f.connect()

	c.send(`{"jsonrpc":"2.0","id":500,"method":"tools/call","params":{"name":"gitty_context"}}`)
	waitFor(t, filepath.Join(f.stub, "context.started"))

	// While the call runs, the server still answers.
	if resp := c.request("ping", nil); resp.Error != nil {
		t.Fatalf("ping during a call: %+v", resp.Error)
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":500,"reason":"the user moved on"}}`)
	waitFor(t, filepath.Join(f.stub, "context.interrupted"))

	// The next thing on the wire is the next answer, not one for call 500.
	if resp := c.request("ping", nil); resp.Error != nil {
		t.Errorf("ping after a cancelled call: %+v", resp.Error)
	}
}

// When the input ends, calls already under way are finished and answered.
func TestEndOfInputFinishesCalls(t *testing.T) {
	f := newFixture(t)
	f.layJSON("context", sampleBriefing())
	c := f.connect()

	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"gitty_context","arguments":{"fetch":false}}}`)
	c.in.Close()
	resp := c.read()
	if string(resp.ID) != "1" || resp.Error != nil || !strings.Contains(string(resp.Result), "acme/lib") {
		t.Errorf("the call in flight when the input ended: id %s, %s", resp.ID, resp.Result)
	}
	select {
	case err := <-c.done:
		if err != nil {
			t.Errorf("serve returned %v", err)
		}
		c.done <- nil // for the cleanup
	case <-time.After(10 * time.Second):
		t.Error("the server did not stop after its input ended")
	}
}

func waitFor(t *testing.T, file string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(file); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", filepath.Base(file))
}

// --- the command line ---

func TestCommandLine(t *testing.T) {
	invoke := func(env map[string]string, args ...string) (string, string, int) {
		t.Helper()
		for k, v := range env {
			t.Setenv(k, v)
		}
		var stdout, stderr bytes.Buffer
		code := run(args, strings.NewReader(""), &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	none := map[string]string{envBin: "", envWorkspace: "", stubEnv: ""}

	if out, _, code := invoke(none, "--version"); code != 0 || strings.TrimSpace(out) != version {
		t.Errorf("--version: %q, exit %d", out, code)
	}

	out, _, code := invoke(none, "--describe")
	var doc struct {
		Name     string   `json:"name"`
		Protocol []string `json:"protocol"`
		Command  []string `json:"command"`
		Tools    []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 || doc.Name != "gitty-mcp" ||
		len(doc.Tools) != 6 || doc.Tools[0].Name != "gitty_context" || strings.Join(doc.Command, " ") != "gitty mcp" {
		t.Errorf("--describe (exit %d, %v): %s", code, err, out)
	}
	if out, _, _ := invoke(none, "--describe", "--no-fetch"); strings.Contains(out, "gitty_fetch") {
		t.Error("--describe --no-fetch lists gitty_fetch")
	}

	// Without a workspace it does not start, and says how to give it one.
	out, errOut, code := invoke(map[string]string{envBin: "/usr/bin/true", envWorkspace: ""})
	if code != 2 || out != "" || !strings.Contains(errOut, "gitty -C") {
		t.Errorf("no workspace: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	_, errOut, code = invoke(map[string]string{envBin: "/usr/bin/true", envWorkspace: t.TempDir()})
	if code != 2 || !strings.Contains(errOut, "not a gitty workspace") {
		t.Errorf("a directory that is no workspace: exit %d, stderr %q", code, errOut)
	}
	for _, args := range [][]string{{"--nope"}, {"extra"}, {"--jobs=99"}} {
		if out, _, code := invoke(none, args...); code != 2 || out != "" {
			t.Errorf("%v: exit %d, stdout %q; want 2 and nothing on stdout", args, code, out)
		}
	}

	// With one, it serves until its input ends, and writes nothing to stdout
	// that is not protocol.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".gitty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".gitty", "config"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envBin, "/usr/bin/true")
	t.Setenv(envWorkspace, ws)
	var stdout, stderr bytes.Buffer
	code = run([]string{"--no-fetch"}, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), &stdout, &stderr)
	if code != 0 || stdout.String() != `{"jsonrpc":"2.0","id":1,"result":{}}`+"\n" {
		t.Errorf("serving: exit %d, stdout %q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "serving workspace") {
		t.Errorf("stderr should say what is being served: %q", stderr.String())
	}
}
