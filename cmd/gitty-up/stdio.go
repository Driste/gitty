package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Serving over stdio: `gitty up --stdio`.
//
// This is how an agent's host is told about gitty in one line — a command it
// starts itself and talks to over the command's stdin and stdout, with
// nothing to deploy and no address to configure. What the command serves
// from depends on where it is started:
//
//   - In a workspace whose config names a hub, it is a bridge to that hub:
//     each message is passed on with the user's own token, and the answer
//     passed back. The agent gets the team's shared context, and the host
//     needs no URL, no header and no sign-in set up.
//
//   - Anywhere else, it is a hub of its own, for this one person: it lists
//     the instance, has gitty tell it what the workspace's checkouts say,
//     and keeps notes in the user's data directory, where every later
//     session finds them.
//
// Either way stdout carries the protocol and nothing else.

// maxStdioLine bounds one message: a tool call with a note in it.
const maxStdioLine = 4 << 20

// serveStdio reads one JSON-RPC message a line and writes back what answer
// returns for each, a line each, until the input ends. Messages are handled
// one at a time, in order: every answer here is a matter of milliseconds,
// and one at a time is what keeps a line from ever being interleaved.
func serveStdio(ctx context.Context, in io.Reader, out io.Writer, answer func(ctx context.Context, line []byte) []byte) error {
	type read struct {
		line []byte
		err  error
	}
	lines := make(chan read)
	go func() {
		defer close(lines)
		r := bufio.NewReaderSize(in, 64<<10)
		for {
			line, err := readLine(r)
			if len(line) > 0 || err != nil {
				select {
				case lines <- read{line, err}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil && !errors.Is(err, errLineTooLong) {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case rd, open := <-lines:
			if !open {
				return nil
			}
			var reply []byte
			switch {
			case errors.Is(rd.err, errLineTooLong):
				reply = mustJSON(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: codeInvalidRequest, Message: "that message is larger than this server reads"}})
			case len(bytes.TrimSpace(rd.line)) > 0:
				reply = answer(ctx, rd.line)
			}
			if reply != nil {
				if _, err := out.Write(append(bytes.TrimRight(reply, "\n"), '\n')); err != nil {
					return fmt.Errorf("writing to the client: %w", err)
				}
			}
			if rd.err != nil && !errors.Is(rd.err, errLineTooLong) {
				if errors.Is(rd.err, io.EOF) {
					return nil // the client closed its end: the way a session is ended
				}
				return fmt.Errorf("reading from the client: %w", rd.err)
			}
		}
	}
}

var errLineTooLong = errors.New("line too long")

// readLine reads up to the next newline. A line over the limit is read to
// its end and reported, so that the one after it starts where it should.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			line = append(line, chunk...)
			if len(line) > maxStdioLine {
				tooLong, line = true, nil
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case tooLong && err == nil:
			return nil, errLineTooLong
		default:
			return line, err
		}
	}
}

// mustJSON renders a message as one line.
func mustJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// answerLine is answer for a transport of lines: the same protocol, with no
// headers beside the message.
func (h *hub) answerLine(c *caller) func(ctx context.Context, line []byte) []byte {
	return func(ctx context.Context, line []byte) []byte {
		_, resp := h.answer(ctx, c, line, nil)
		if resp == nil {
			return nil
		}
		return mustJSON(resp)
	}
}

// --- a hub of one's own ---

// shareWorkspace has gitty tell this hub what the workspace's checkouts
// say. The hub cannot read a repository and does not try: it opens a door
// on this machine only, at an address nobody else can guess, and runs
// `gitty share` at it — the same command, with the same care about what
// counts as a repository's own account, that feeds a team's hub.
//
// The door takes what gitty sends and nothing else, and is shut when gitty
// is done. Its address reaches gitty in the environment, not on the command
// line, where other users of the machine could read it.
func (h *hub) shareWorkspace(ctx context.Context, gittyBin, workspace, token string, anon bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.logf("could not open a local door for gitty share: %v", err)
		return
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		ln.Close()
		return
	}
	prefix := "/" + hex.EncodeToString(secret)
	api := h.routes(nil)
	mux := http.NewServeMux()
	mux.Handle(prefix+"/v1/", http.StripPrefix(prefix, api))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	args := []string{"share"}
	if anon {
		args = append(args, "--anon")
	}
	cmd := exec.CommandContext(ctx, gittyBin, args...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), envHub+"=http://"+ln.Addr().String()+prefix)
	if token != "" {
		cmd.Env = append(cmd.Env, "GITLAB_TOKEN="+token)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	summary := lastLine(stdout.String())
	switch {
	case ctx.Err() != nil:
	case err != nil && summary == "":
		h.logf("could not read the workspace's checkouts (gitty share: %v): %s", err, lastLine(stderr.String()))
	default:
		h.logf("read the workspace's checkouts: %s", strings.TrimPrefix(summary, "summary "))
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// --- a bridge to the team's hub ---

// bridge passes messages between a client on stdio and a hub over HTTP.
type bridge struct {
	hub   string // the hub's address, no trailing slash
	token string
	http  *http.Client
	logf  func(string, ...any)

	mu     sync.Mutex
	legacy string // the protocol version a handshake settled on, if there was one
}

// newBridge prepares a bridge. The token is the user's own, so it is held to
// the same rule gitty holds it to: over https, or to this machine.
func newBridge(hubURL, token string, logf func(string, ...any)) (*bridge, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(hubURL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not the address of a hub: expected something like https://gitty.example.com", hubURL)
	}
	if token != "" && u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("the hub at %s is plain http: your GitLab token would cross the network unprotected. Use an https address for it", u)
	}
	return &bridge{hub: u.String(), token: token, http: &http.Client{Timeout: 120 * time.Second}, logf: logf}, nil
}

// headerValue encodes a value for a header the way the protocol asks when
// plain ASCII cannot carry it.
func headerValue(v string) string {
	plain := v != "" && v == strings.TrimSpace(v) && !(strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?="))
	for i := 0; plain && i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			plain = false
		}
	}
	if plain {
		return v
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?="
}

// answer passes one message to the hub and returns its answer, as a line.
func (b *bridge) answer(ctx context.Context, line []byte) []byte {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string                     `json:"name"`
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	json.Unmarshal(line, &msg)
	wantsAnswer := len(msg.ID) > 0 && string(msg.ID) != "null" && msg.Method != ""
	fail := func(format string, a ...any) []byte {
		text := fmt.Sprintf(format, a...)
		b.logf("%s", text)
		if !wantsAnswer {
			return nil
		}
		return mustJSON(rpcResponse{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{Code: codeInternal, Message: text}})
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.hub+"/mcp", bytes.NewReader(line))
	if err != nil {
		return fail("gitty up: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "gitty-up/"+version+" (stdio bridge)")
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	// Over HTTP a request of the current revision says in headers what it
	// says in its body, and the hub holds it to that.
	var modern string
	if raw, ok := msg.Params.Meta[metaVersion]; ok {
		json.Unmarshal(raw, &modern)
	}
	b.mu.Lock()
	legacy := b.legacy
	b.mu.Unlock()
	switch {
	case modern != "":
		req.Header.Set("MCP-Protocol-Version", modern)
		req.Header.Set("Mcp-Method", msg.Method)
		if msg.Method == "tools/call" {
			req.Header.Set("Mcp-Name", headerValue(msg.Params.Name))
		}
	case legacy != "" && msg.Method != "initialize":
		req.Header.Set("MCP-Protocol-Version", legacy)
	}

	resp, err := b.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fail("gitty up could not reach the hub at %s: %v", b.hub, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fail("gitty up could not read the answer of the hub at %s: %v", b.hub, err)
	}
	if !wantsAnswer {
		return nil
	}

	// What the hub says is passed on as it is. Only an answer that cannot
	// be matched to its question — the hub did not get as far as reading
	// it — is given the id it belongs to, and a status that is not an
	// answer at all is put into words.
	var answer map[string]json.RawMessage
	if json.Unmarshal(body, &answer) != nil || (answer["result"] == nil && answer["error"] == nil) {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return fail("the hub at %s did not accept your GitLab token: set GITLAB_TOKEN, in the environment this server is started in, to a token of the instance", b.hub)
		default:
			return fail("the hub at %s answered %d, which is not what a gitty-up says: is that its address?", b.hub, resp.StatusCode)
		}
	}
	if id, ok := answer["id"]; !ok || string(id) == "null" {
		answer["id"] = msg.ID
		answer["jsonrpc"] = json.RawMessage(`"2.0"`)
		if resp.StatusCode == http.StatusUnauthorized {
			b.logf("the hub at %s did not accept the token in GITLAB_TOKEN", b.hub)
		}
	}
	if msg.Method == "initialize" {
		var result struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(answer["result"], &result) == nil && result.ProtocolVersion != "" {
			b.mu.Lock()
			b.legacy = result.ProtocolVersion
			b.mu.Unlock()
		}
	}
	return mustJSON(answer)
}

// check asks the hub who it is, so that a wrong address or a token it will
// not take is said at the start, in the server's log, and not only when
// the agent first asks something.
func (b *bridge) check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.hub+"/v1/info", nil)
	if err != nil {
		return
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		b.logf("warning: could not reach the hub at %s: %v", b.hub, err)
		return
	}
	defer resp.Body.Close()
	var info struct {
		Name     string `json:"name"`
		Version  string `json:"version"`
		Instance string `json:"instance"`
		Projects int    `json:"projects"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		b.logf("warning: the hub at %s did not accept your GitLab token (GITLAB_TOKEN); every request will be refused until it does", b.hub)
	case resp.StatusCode != http.StatusOK || json.Unmarshal(body, &info) != nil || info.Name != serverName:
		b.logf("warning: %s answered %d and does not look like a gitty-up", b.hub, resp.StatusCode)
	default:
		b.logf("bridging to %s %s at %s: %d projects of %s", info.Name, info.Version, b.hub, info.Projects, info.Instance)
	}
}

// stdioEnv is what gitty tells a plugin about where it was started.
type stdioEnv struct {
	gittyBin  string
	workspace string
}

func readStdioEnv() stdioEnv {
	return stdioEnv{gittyBin: os.Getenv("GITTY_BIN"), workspace: os.Getenv("GITTY_WORKSPACE")}
}
