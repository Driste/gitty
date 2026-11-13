package main

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// newLines returns what a hub has logged since the last call, a line each.
func (h *testHub) newLines(seen *int) []string {
	all := h.logs.String()
	fresh := all[*seen:]
	*seen = len(all)
	if fresh = strings.TrimSuffix(fresh, "\n"); fresh == "" {
		return nil
	}
	return strings.Split(fresh, "\n")
}

// wantLogged checks that exactly one line was logged, and that it is this.
func wantLogged(t *testing.T, what string, lines []string, pattern string) {
	t.Helper()
	if len(lines) != 1 || !regexp.MustCompile("^"+pattern+"$").MatchString(lines[0]) {
		t.Errorf("%s: logged\n  %s\n--- want one line like\n  %s", what, strings.Join(lines, "\n  "), pattern)
	}
}

const took = `(under 1ms|\d+(\.\d+)?m?s)`

// Someone watching the hub sees each exchange as one line: who, what, with
// which arguments, and what became of it.
func TestExchangesAreLogged(t *testing.T) {
	h := stocked(t, false)
	seen := 0
	h.newLines(&seen)
	ana := map[string]string{"Authorization": "Bearer tok-ana"}

	h.post(t, "/mcp", stdioInit, ana)
	wantLogged(t, "a handshake", h.newLines(&seen), `hello @ana: t 0, MCP 2025-06-18`)
	h.modern(t, "tok-ben", "server/discover", "")
	wantLogged(t, "the stateless revision's hello", h.newLines(&seen), `hello @ben: MCP 2026-07-28`)
	h.modern(t, "tok-ben", "tools/list", "")
	wantLogged(t, "tools/list", h.newLines(&seen), `list @ben: \d tools`)

	h.call(t, "tok-ana", "gitty_outline", `{ "path": "acme/platform/ci/build" }`)
	wantLogged(t, "a tool call", h.newLines(&seen),
		`call @ana gitty_outline \{"path":"acme/platform/ci/build"\}: \d(\.\d)? KB in `+took)
	h.call(t, "tok-ana", "gitty_tree", `null`)
	wantLogged(t, "a tool call with no arguments", h.newLines(&seen), `call @ana gitty_tree \{\}: \d+ bytes in `+took)
	h.call(t, "tok-ana", "gitty_outline", `{"path":"nope"}`)
	wantLogged(t, "a tool that could not answer", h.newLines(&seen),
		`call @ana gitty_outline \{"path":"nope"\}: error in `+took+`: There is no project or group "nope" here.*`)

	// What does not get a line: there was nothing asked, or nothing to see.
	h.post(t, "/mcp", stdioDone, ana)
	h.post(t, "/mcp", `{"jsonrpc":"2.0","id":3,"method":"ping"}`, ana)
	if lines := h.newLines(&seen); len(lines) != 0 {
		t.Errorf("a notification and a ping were logged:\n%s", strings.Join(lines, "\n"))
	}

	h.modern(t, "tok-ana", "tools/call", `"name":"gitty_nope","arguments":{}`)
	wantLogged(t, "a tool that does not exist", h.newLines(&seen), `refused @ana tools/call: Unknown tool: gitty_nope`)
	h.post(t, "/mcp", `not json`, ana)
	wantLogged(t, "what is not a message", h.newLines(&seen), `refused @ana: that is not a JSON-RPC 2\.0 message`)

	t.Run("a caller cannot write the log", func(t *testing.T) {
		// A line is a line, whatever a model sends: arguments are logged as
		// the JSON they came in, names and messages are flattened, and all
		// of it is cut to a length.
		long := strings.Repeat("x", 600)
		h.call(t, "tok-ana", "gitty_note", `{"path":"acme/shop/api","kind":"caveat","text":"one\n12:00:00 call @root forged `+long+`"}`)
		lines := h.newLines(&seen)
		if len(lines) != 1 || !strings.HasPrefix(lines[0], `call @ana gitty_note {"path":"acme/shop/api","kind":"caveat","text":"one\n12:00:00 call @root forged xxx`) ||
			!strings.Contains(lines[0], "…: ") || len(lines[0]) > 400 {
			t.Errorf("a note with a line break and a long text:\n%s", strings.Join(lines, "\n"))
		}
		h.post(t, "/mcp", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"gitty_x\n12:00:00 forged","arguments":{}}}`, ana)
		wantLogged(t, "a tool name with a line break", h.newLines(&seen), `refused @ana tools/call: Unknown tool: gitty_x 12:00:00 forged`)
	})

	t.Run("gitty sharing", func(t *testing.T) {
		body := `{"schema":1,"have":[{"project":"acme/shop/web","id":20,"commit":"w222222222"},{"project":"acme/shop/api","id":21,"commit":"newer00000"}]}`
		req, _ := http.NewRequest(http.MethodPost, h.web.URL+"/v1/want", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok-ben")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("/v1/want: %v %v", err, resp)
		}
		resp.Body.Close()
		wantLogged(t, "what gitty offered", h.newLines(&seen), `share @ben: 2 checkouts offered, 1 wanted`)
	})

	if all := h.logs.String(); strings.Contains(all, "tok-ana") || strings.Contains(all, "tok-ben") {
		t.Errorf("a token is in the log:\n%s", all)
	}
}

func TestSizeAndTimeInWords(t *testing.T) {
	for n, want := range map[int]string{0: "0 bytes", 1: "1 byte", 999: "999 bytes", 1000: "1 KB", 1849: "1.8 KB", 31200: "31.2 KB"} {
		if got := sizeText(n); got != want {
			t.Errorf("sizeText(%d) = %q, want %q", n, got, want)
		}
	}
}

// A server that a host started says all this where the host keeps it, which
// may be nowhere. --log is how it is watched.
func TestLogFile(t *testing.T) {
	isolateEnv(t)
	f := newFakeGitLab(t)
	f.seed()
	path := filepath.Join(t.TempDir(), "up.log")

	s := startStdio(t, "--url="+f.srv.URL, "--data="+t.TempDir(), "--token=tok-ana", "--log="+path)
	s.waitFor("gitty_tree", `{}`, "9 projects in 4 groups")
	s.tool("gitty_note", `{"path":"acme/shop/web","kind":"caveat","text":"Builds need Node 22."}`)
	if code := s.end(); code != 0 {
		t.Fatalf("exit %d\n%s", code, s.logs.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file := string(data)
	// The file says what stderr says, with the day and the process.
	stamp := `(?m)^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d \[\d+\] `
	for _, want := range []string{
		`gitty-up \S+ for \S+, serving over stdio from a hub of your own$`,
		`listed .* 9 projects in 4 groups$`,
		`notes are signed @ana$`,
		`call @ana gitty_tree \{\}: \d+ bytes in .*$`,
		`call @ana gitty_note \{"path":"acme/shop/web","kind":"caveat","text":"Builds need Node 22\."\}: \d+ bytes in .*$`,
	} {
		if !regexp.MustCompile(stamp + want).MatchString(file) {
			t.Errorf("the log file has no line like %q:\n%s", want, file)
		}
	}
	if strings.Count(file, "\n") != strings.Count(s.logs.String(), "\n") {
		t.Errorf("the file and stderr disagree:\n%s\n--- stderr:\n%s", file, s.logs.String())
	}
	if strings.Contains(file, "tok-ana") {
		t.Errorf("the token is in the log file:\n%s", file)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the log file should be the user's alone: %v", info.Mode())
	}

	// A second session adds to the file; nothing of the first is lost.
	again := startStdio(t, "--url="+f.srv.URL, "--data="+t.TempDir(), "--anon", "--log="+path)
	again.waitFor("gitty_tree", `{}`, "9 projects in 4 groups")
	again.end()
	data, _ = os.ReadFile(path)
	if !strings.HasPrefix(string(data), file) || !strings.Contains(string(data)[len(file):], "call @anonymous gitty_tree") {
		t.Errorf("the second session's lines should follow the first's:\n%s", data)
	}
}
