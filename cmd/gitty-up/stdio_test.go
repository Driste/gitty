package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// talk feeds lines to a stdio server and returns what it wrote back, a
// message per line.
func talk(t *testing.T, answer func(context.Context, []byte) []byte, input string) []*rpcReply {
	t.Helper()
	var out strings.Builder
	if err := serveStdio(context.Background(), strings.NewReader(input), &out, answer); err != nil {
		t.Fatalf("serveStdio: %v", err)
	}
	var replies []*rpcReply
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		r := &rpcReply{raw: line}
		if err := json.Unmarshal([]byte(line), r); err != nil {
			t.Fatalf("a line of output is not a JSON message: %v\n%s", err, line)
		}
		replies = append(replies, r)
	}
	return replies
}

// idOf is the id a reply carries, as written.
func idOf(r *rpcReply) string {
	var m struct {
		ID json.RawMessage `json:"id"`
	}
	json.Unmarshal([]byte(r.raw), &m)
	return string(m.ID)
}

const (
	stdioInit = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	stdioDone = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
)

func TestStdioSpeaksBothRevisions(t *testing.T) {
	h := stocked(t, true)
	answer := h.answerLine(&caller{Name: "ana"})

	t.Run("the handshake revisions", func(t *testing.T) {
		got := talk(t, answer, strings.Join([]string{
			stdioInit,
			stdioDone,
			"",
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
			`{"jsonrpc":"2.0","id":"three","method":"tools/call","params":{"name":"gitty_tree","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":4,"method":"ping"}`,
		}, "\n")+"\n")
		if len(got) != 4 {
			t.Fatalf("want four answers (the notification and the blank line get none), got %d", len(got))
		}
		if v, _ := got[0].Result["protocolVersion"].(string); v != "2025-06-18" {
			t.Errorf("initialize settled on %q", v)
		}
		if listed, _ := got[1].Result["tools"].([]any); len(listed) != len(tools) {
			t.Errorf("tools/list: %s", got[1].raw)
		}
		wantText(t, "gitty_tree over stdio", got[2].text(), "9 projects in 4 groups", "platform/")
		if idOf(got[2]) != `"three"` || idOf(got[3]) != "4" {
			t.Errorf("ids were not kept: %s, %s", idOf(got[2]), idOf(got[3]))
		}
	})

	t.Run("the stateless revision, which has no headers here", func(t *testing.T) {
		got := talk(t, answer, strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + modernMeta + `}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"gitty_search","arguments":{"query":"build image"},` + modernMeta + `}}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gitty_tree","_meta":{"io.modelcontextprotocol/protocolVersion":"2031-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		}, "\n")) // and no newline after the last: the client just closed
		if len(got) != 3 {
			t.Fatalf("want three answers, got %d", len(got))
		}
		if got[0].Result["resultType"] != "complete" {
			t.Errorf("server/discover: %s", got[0].raw)
		}
		wantText(t, "gitty_search over stdio", got[1].text(), "acme/platform/ci/build")
		if got[2].Error == nil || got[2].Error.Code != codeBadVersion {
			t.Errorf("a revision this server does not speak: %s", got[2].raw)
		}
	})

	t.Run("what is not a message", func(t *testing.T) {
		long := `{"jsonrpc":"2.0","id":9,"method":"ping","params":{"pad":"` + strings.Repeat("x", maxStdioLine) + `"}}`
		got := talk(t, answer, strings.Join([]string{
			`not json`,
			long,
			`[` + stdioInit + `]`,
			`{"jsonrpc":"2.0","id":5,"method":"ping"}`,
		}, "\r\n")+"\r\n")
		if len(got) != 4 {
			t.Fatalf("want four answers, got %d", len(got))
		}
		for i, code := range []int{codeParse, codeInvalidRequest, codeInvalidRequest} {
			if got[i].Error == nil || got[i].Error.Code != code {
				t.Errorf("answer %d: want error %d, got %s", i, code, got[i].raw)
			}
		}
		if got[3].Error != nil || idOf(got[3]) != "5" {
			t.Errorf("the message after the bad ones was not answered: %s", got[3].raw)
		}
	})
}

// A hub of one's own signs notes with the name of whoever started it, and
// asks the instance nothing about them: what it lists is what they see.
func TestStdioOwnHubSignsNotes(t *testing.T) {
	h := stocked(t, true)
	answer := h.answerLine(&caller{Name: "ana", ID: 100})
	whoBefore := h.fake.hitsFor("/api/v4/user")

	got := talk(t, answer, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"gitty_note","arguments":{"path":"acme/shop/api","kind":"caveat","text":"The staging database is shared with web."}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"gitty_outline","arguments":{"path":"acme/shop/api"}}}`,
	}, "\n")+"\n")
	if len(got) != 2 || got[0].isError() {
		t.Fatalf("recording a note: %+v", got)
	}
	wantText(t, "the outline", got[1].text(), "The staging database is shared with web.", "@ana")
	if n := h.fake.hitsFor("/api/v4/user") - whoBefore; n != 0 {
		t.Errorf("the instance was asked who the caller is %d times", n)
	}
}

// --- the bridge ---

func bridgeTo(t *testing.T, hubURL, token string) (*bridge, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	b, err := newBridge(hubURL, token, func(format string, a ...any) {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		fmt.Fprintf(&logs.b, format+"\n", a...)
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, logs
}

func TestBridgeCarriesBothRevisions(t *testing.T) {
	h := stocked(t, false)
	b, _ := bridgeTo(t, h.web.URL+"/", "tok-ana")

	t.Run("the stateless revision gets the headers HTTP wants", func(t *testing.T) {
		got := talk(t, b.answer, strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + modernMeta + `}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + modernMeta + `}}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gitty_outline","arguments":{"path":"acme/platform/ci/build"},` + modernMeta + `}}`,
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"gitty_trée","arguments":{},` + modernMeta + `}}`,
		}, "\n")+"\n")
		if len(got) != 4 {
			t.Fatalf("want four answers, got %d", len(got))
		}
		for i := 0; i < 3; i++ {
			if got[i].Error != nil || got[i].Result["resultType"] != "complete" {
				t.Errorf("answer %d: %s", i, got[i].raw)
			}
		}
		direct := h.call(t, "tok-ana", "gitty_outline", `{"path":"acme/platform/ci/build"}`).text()
		if got[2].text() != direct || direct == "" {
			t.Errorf("the bridge changed an answer:\n%s\n--- direct:\n%s", got[2].text(), direct)
		}
		// A name that a header cannot carry as it is still reaches the hub
		// as the name it is: the hub says it knows no such tool, not that
		// the header and the body disagree.
		if got[3].Error == nil || !strings.Contains(got[3].Error.Message, "Unknown tool: gitty_trée") {
			t.Errorf("a tool name outside ASCII: %s", got[3].raw)
		}
	})

	t.Run("the handshake revisions", func(t *testing.T) {
		got := talk(t, b.answer, strings.Join([]string{
			stdioInit,
			stdioDone,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gitty_note","arguments":{"path":"acme/platform/ci/build","kind":"usage","text":"Pin the version."}}}`,
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"gitty_outline","arguments":{"path":"acme/platform/ci/build"}}}`,
		}, "\n")+"\n")
		if len(got) != 4 {
			t.Fatalf("want four answers (the notification gets none), got %d", len(got))
		}
		if v, _ := got[0].Result["protocolVersion"].(string); v != "2025-06-18" {
			t.Errorf("initialize settled on %q: %s", v, got[0].raw)
		}
		if got[2].isError() {
			t.Errorf("the note was refused: %s", got[2].text())
		}
		// The note is signed by whoever the token belongs to, with their
		// standing on the project: the hub asked the instance, as it would
		// have for a client that came to it directly.
		wantText(t, "the outline", got[3].text(), "Pin the version.", "@ana", "maintainer")
	})
}

func TestBridgeSaysWhatWentWrong(t *testing.T) {
	h := stocked(t, false)
	ask := `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"gitty_tree","arguments":{}}}`

	t.Run("a token the instance does not accept", func(t *testing.T) {
		b, logs := bridgeTo(t, h.web.URL, "tok-nobody")
		got := talk(t, b.answer, ask+"\n"+stdioDone+"\n")
		if len(got) != 1 || got[0].Error == nil || idOf(got[0]) != "41" {
			t.Fatalf("want one error, for request 41: %+v", got)
		}
		if !strings.Contains(got[0].Error.Message, "does not accept that token") {
			t.Errorf("the agent is told: %s", got[0].Error.Message)
		}
		if !strings.Contains(logs.String(), "GITLAB_TOKEN") {
			t.Errorf("the log does not say which token: %s", logs.String())
		}
	})

	t.Run("no token at all", func(t *testing.T) {
		b, _ := bridgeTo(t, h.web.URL, "")
		got := talk(t, b.answer, ask+"\n")
		if len(got) != 1 || got[0].Error == nil || idOf(got[0]) != "41" {
			t.Fatalf("want one error, for request 41: %+v", got)
		}
	})

	t.Run("something that is not a hub", func(t *testing.T) {
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<html>no</html>")
		}))
		defer other.Close()
		b, _ := bridgeTo(t, other.URL, "tok-ana")
		got := talk(t, b.answer, ask+"\n")
		if len(got) != 1 || got[0].Error == nil || idOf(got[0]) != "41" ||
			!strings.Contains(got[0].Error.Message, "answered 404, which is not what a gitty-up says") {
			t.Fatalf("want one error naming the 404: %+v", got)
		}
	})

	t.Run("a hub that is not there", func(t *testing.T) {
		gone := httptest.NewServer(http.NotFoundHandler())
		addr := gone.URL
		gone.Close()
		b, _ := bridgeTo(t, addr, "tok-ana")
		got := talk(t, b.answer, ask+"\n"+stdioDone+"\n")
		if len(got) != 1 || got[0].Error == nil || idOf(got[0]) != "41" ||
			!strings.Contains(got[0].Error.Message, "could not reach the hub at "+addr) {
			t.Fatalf("want one error saying the hub could not be reached: %+v", got)
		}
	})

	t.Run("the token never leaves in words", func(t *testing.T) {
		b, logs := bridgeTo(t, h.web.URL, "tok-nobody")
		b.check(context.Background())
		got := talk(t, b.answer, ask+"\n")
		if all := logs.String() + got[0].raw; strings.Contains(all, "tok-nobody") {
			t.Errorf("the token is in what was said:\n%s", all)
		}
		if !strings.Contains(logs.String(), "did not accept your GitLab token") {
			t.Errorf("the check did not warn: %s", logs.String())
		}
	})
}

func TestBridgeChecksTheHub(t *testing.T) {
	h := stocked(t, false)
	b, logs := bridgeTo(t, h.web.URL, "tok-ana")
	b.check(context.Background())
	if !strings.Contains(logs.String(), "bridging to") {
		t.Errorf("a hub that is there and takes the token: %s", logs.String())
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"something-else"}`)
	}))
	defer other.Close()
	b, logs = bridgeTo(t, other.URL, "tok-ana")
	b.check(context.Background())
	if !strings.Contains(logs.String(), "does not look like a gitty-up") {
		t.Errorf("something else at the address: %s", logs.String())
	}
}

func TestHeaderValue(t *testing.T) {
	for in, want := range map[string]string{
		"gitty_tree":      "gitty_tree",
		"gitty_trée":      "=?base64?Z2l0dHlfdHLDqWU=?=",
		" padded ":        "=?base64?IHBhZGRlZCA=?=",
		"":                "=?base64??=",
		"=?base64?eA==?=": "=?base64?PT9iYXNlNjQ/ZUE9PT89?=",
	} {
		if got := headerValue(in); got != want {
			t.Errorf("headerValue(%q) = %q, want %q", in, got, want)
		}
		if back := decodeHeaderValue(headerValue(in)); back != in {
			t.Errorf("%q does not survive the header: %q", in, back)
		}
	}
}

// --- the command ---

// stdioSession is a client of `gitty up --stdio`, run in this process.
type stdioSession struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Reader
	logs *syncBuffer
	done chan int
	next int
}

type logWriter struct{ b *syncBuffer }

func (w logWriter) Write(p []byte) (int, error) {
	w.b.mu.Lock()
	defer w.b.mu.Unlock()
	return w.b.b.Write(p)
}

func startStdio(t *testing.T, args ...string) *stdioSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &stdioSession{t: t, in: inW, out: bufio.NewReaderSize(outR, 1<<20), logs: &syncBuffer{}, done: make(chan int, 1)}
	go func() {
		code := run(context.Background(), append([]string{"--stdio"}, args...), inR, outW, logWriter{s.logs})
		outW.Close()
		s.done <- code
	}()
	t.Cleanup(func() { s.end() })
	return s
}

// end closes the client's side, which is how a session is ended, and
// returns the server's exit code.
func (s *stdioSession) end() int {
	s.in.Close()
	select {
	case code := <-s.done:
		s.done <- code
		return code
	case <-time.After(10 * time.Second):
		s.t.Errorf("the server did not stop when its input closed\n%s", s.logs.String())
		return -1
	}
}

func (s *stdioSession) tool(name, args string) *rpcReply {
	s.t.Helper()
	s.next++
	line := `{"jsonrpc":"2.0","id":` + itoa(s.next) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}` + "\n"
	if _, err := io.WriteString(s.in, line); err != nil {
		s.t.Fatalf("writing to the server: %v\n%s", err, s.logs.String())
	}
	raw, err := s.out.ReadString('\n')
	if err != nil {
		s.t.Fatalf("reading from the server: %v\n%s", err, s.logs.String())
	}
	r := &rpcReply{raw: raw}
	if err := json.Unmarshal([]byte(raw), r); err != nil || idOf(r) != itoa(s.next) {
		s.t.Fatalf("not an answer to request %d: %s", s.next, raw)
	}
	return r
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// waitFor asks until the answer holds want.
func (s *stdioSession) waitFor(name, args, want string) string {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		text := s.tool(name, args).text()
		if strings.Contains(text, want) {
			return text
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("%s never said %q:\n%s\n--- log:\n%s", name, want, text, s.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isolateEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"GITLAB_TOKEN", envInstance, envGroup, envHub, "GITTY_BIN", "GITTY_WORKSPACE", "GITTY_UP_DATA", "GITTY_UP_LOG"} {
		t.Setenv(name, "")
	}
}

func TestCommandStdioOwnHub(t *testing.T) {
	isolateEnv(t)
	f := newFakeGitLab(t)
	f.seed()
	dir := t.TempDir()

	s := startStdio(t, "--url="+f.srv.URL, "--data="+dir, "--token=tok-ana")
	// The agent is answered from the first moment, and with the listing as
	// soon as there is one.
	s.waitFor("gitty_tree", `{}`, "9 projects in 4 groups")
	if r := s.tool("gitty_note", `{"path":"acme/shop/web","kind":"caveat","text":"Builds need Node 22."}`); r.isError() {
		t.Fatalf("recording a note: %s", r.text())
	}
	wantText(t, "the outline", s.tool("gitty_outline", `{"path":"acme/shop/web"}`).text(), "Builds need Node 22.", "@ana")
	if code := s.end(); code != 0 {
		t.Errorf("exit %d when the client closed\n%s", code, s.logs.String())
	}
	logs := s.logs.String()
	wantText(t, "the log", logs, "serving over stdio from a hub of your own", "notes are signed @ana", "listed ", "no checkout is read")
	refuseText(t, "the log", logs, "tok-ana")

	// The next session — another agent, another day — finds what the last
	// one left, before it has listed anything itself.
	f.setDown(true)
	defer func(d time.Duration) { identifyWait = d }(identifyWait)
	identifyWait = 300 * time.Millisecond
	again := startStdio(t, "--url="+f.srv.URL, "--data="+dir, "--token=tok-ana")
	wantText(t, "the outline, later", again.tool("gitty_outline", `{"path":"acme/shop/web"}`).text(), "Builds need Node 22.")
	// The instance could not be asked whose token this is: the session is
	// served all the same, and its notes are signed plainly.
	if r := again.tool("gitty_note", `{"path":"acme/shop/api","kind":"caveat","text":"Offline note."}`); r.isError() {
		t.Fatalf("recording a note with the instance down: %s", r.text())
	}
	wantText(t, "the outline, offline", again.tool("gitty_outline", `{"path":"acme/shop/api"}`).text(), "Offline note.", "@me")
}

func TestCommandStdioOwnHubRefusesABadToken(t *testing.T) {
	isolateEnv(t)
	f := newFakeGitLab(t)
	f.seed()
	s := startStdio(t, "--url="+f.srv.URL, "--data="+t.TempDir(), "--token=tok-nobody")
	// It stops by itself: nobody has to close anything.
	select {
	case code := <-s.done:
		s.done <- code
		if code != 2 {
			t.Errorf("exit %d, want 2\n%s", code, s.logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the server kept running\n%s", s.logs.String())
	}
	wantText(t, "the log", s.logs.String(), "does not accept the token")
	refuseText(t, "the log", s.logs.String(), "tok-nobody")
}

func TestCommandStdioBridge(t *testing.T) {
	isolateEnv(t)
	h := stocked(t, false)

	// The hub's address comes from the workspace, by way of gitty.
	t.Setenv(envHub, h.web.URL)
	t.Setenv("GITLAB_TOKEN", "tok-ben")
	s := startStdio(t)
	wantText(t, "the tree, from the team's hub", s.tool("gitty_tree", `{}`).text(), "9 projects in 4 groups")
	if r := s.tool("gitty_note", `{"path":"acme/shop/web","kind":"usage","text":"Run make dev."}`); r.isError() {
		t.Fatalf("recording a note: %s", r.text())
	}
	// Both ends say what passed: the session for whoever is watching it,
	// and the hub with the name of who asked.
	wantText(t, "the session's log", s.logs.String(), "call gitty_tree {}: ", `call gitty_note {"path":"acme/shop/web","kind":"usage","text":"Run make dev."}: `)
	wantText(t, "the hub's log", h.logs.String(), "call @ben gitty_tree {}: ", "call @ben gitty_note ")
	// The note is in the team's hub, signed by the token's owner.
	wantText(t, "the hub's own outline", h.call(t, "tok-ana", "gitty_outline", `{"path":"acme/shop/web"}`).text(), "Run make dev.", "@ben")
	if code := s.end(); code != 0 {
		t.Errorf("exit %d when the client closed\n%s", code, s.logs.String())
	}
	refuseText(t, "the log", s.logs.String(), "tok-ben")

	// --own keeps to itself where a team's hub is named.
	f := newFakeGitLab(t)
	f.seed()
	own := startStdio(t, "--own", "--url="+f.srv.URL, "--data="+t.TempDir(), "--anon")
	own.waitFor("gitty_tree", `{}`, "9 projects in 4 groups")
	refuseText(t, "an own hub's outline", own.tool("gitty_outline", `{"path":"acme/shop/web"}`).text(), "Run make dev.")
}
