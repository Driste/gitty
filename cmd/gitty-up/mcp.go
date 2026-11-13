package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MCP over HTTP.
//
// One endpoint, POST only, nothing kept between requests. It speaks the
// protocol as revised on 2026-07-28 — every request carries its version and
// the client's capabilities, mirrored in headers that are checked against
// the body, and there is no handshake — and, on the same endpoint, the
// handshake revisions of 2025 for clients that have not moved: a request
// without the new metadata is taken as one of theirs.
//
// The server is stateless either way: it issues no session, and the answer
// to a request depends on nothing but the request and what the hub knows.

const protoModern = "2026-07-28"

// protoLegacy are the handshake revisions served, newest first.
var protoLegacy = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

var protoAll = append([]string{protoModern}, protoLegacy...)

func isLegacy(v string) bool {
	for _, l := range protoLegacy {
		if v == l {
			return true
		}
	}
	return false
}

// JSON-RPC and MCP error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeHeaderMismatch = -32020
	codeBadVersion     = -32022
)

const (
	metaVersion      = "io.modelcontextprotocol/protocolVersion"
	metaCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo   = "io.modelcontextprotocol/serverInfo"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// maxRequestBody bounds an MCP request: a tool call with a note in it.
const maxRequestBody = 1 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string, data any) {
	writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg, Data: data}})
}

// serverInfo is how the hub names itself in answers.
func serverInfo() map[string]any {
	return map[string]any{"name": serverName, "title": "gitty-up", "version": version}
}

// handleMCP is the MCP endpoint over HTTP.
func (h *hub) handleMCP(w http.ResponseWriter, r *http.Request) {
	// Who is asking comes first, whatever they ask. A client that is turned
	// away tries other ways in — an older transport opens with a GET — and
	// each of those must be told the same thing: not "wrong method" when the
	// trouble is the token.
	c, status, why := h.auth.identify(r.Context(), r)
	if c == nil {
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gitty-up"`)
		}
		writeRPCError(w, status, nil, codeInvalidRequest, why, nil)
		return
	}
	if r.Method != http.MethodPost {
		// No stream to open and no session to end, in either era.
		w.Header().Set("Allow", "POST")
		http.Error(w, "the MCP endpoint takes POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeRPCError(w, http.StatusRequestEntityTooLarge, nil, codeInvalidRequest, "the request is larger than this server reads", nil)
		return
	}
	status, resp := h.answer(r.Context(), c, body, r.Header)
	if resp == nil {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, resp)
}

// answer handles one JSON-RPC message from a known caller and returns what
// to say back, with the HTTP status it would travel under; nil means there
// is nothing to say (a notification). It is the whole of the protocol, and
// knows nothing of how the message arrived: headers is what came beside the
// body on a transport that has such a thing, and nil on one that does not
// (stdio), where everything a request has to say about itself is in it.
func (h *hub) answer(ctx context.Context, c *caller, body []byte, headers http.Header) (int, *rpcResponse) {
	started := time.Now()
	req, status, resp := h.respond(ctx, c, body, headers)
	who := ""
	if c != nil {
		who = c.Name
	}
	if line := exchangeLine(who, req, resp, time.Since(started)); line != "" {
		h.logf("%s", line)
	}
	return status, resp
}

// respond is answer before anything is said about it in the log. It also
// returns the request, as far as there was one to read.
func (h *hub) respond(ctx context.Context, c *caller, body []byte, headers http.Header) (*rpcRequest, int, *rpcResponse) {
	fail := func(status int, id json.RawMessage, code int, msg string, data any) (*rpcRequest, int, *rpcResponse) {
		return nil, status, &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg, Data: data}}
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		return fail(http.StatusBadRequest, nil, codeInvalidRequest, "send one JSON-RPC message at a time, not a batch", nil)
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" {
		return fail(http.StatusBadRequest, nil, codeParse, "that is not a JSON-RPC 2.0 message", nil)
	}
	if req.Method == "" || len(req.ID) == 0 || string(req.ID) == "null" {
		// A notification, or a response to a request this server never
		// makes: there is nothing to answer.
		return &req, http.StatusAccepted, nil
	}

	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if len(req.Params) > 0 {
		json.Unmarshal(req.Params, &params)
	}
	var bodyVersion string
	if raw, ok := params.Meta[metaVersion]; ok {
		json.Unmarshal(raw, &bodyVersion)
	}
	headerVersion := ""
	if headers != nil {
		headerVersion = headers.Get("MCP-Protocol-Version")
	}
	if bodyVersion == "" && (headerVersion == "" || isLegacy(headerVersion)) {
		status, resp := h.answerLegacy(ctx, c, &req)
		return &req, status, resp
	}
	status, resp := h.answerModern(ctx, c, &req, params.Meta, headers, headerVersion, bodyVersion)
	return &req, status, resp
}

// --- what the log says of an exchange ---

// exchangeLine is one line about one exchange, for whoever is watching the
// server: who asked, what, with which arguments, and what became of it. It
// is "" for what is not worth a line — a notification, a ping.
//
//	hello @ana: opencode 1.19.5, MCP 2025-06-18
//	list @ana: 5 tools
//	call @ana gitty_outline {"path":"acme/shop/web"}: 1.8 KB in 2ms
//	call @ana gitty_outline {"path":"nope"}: error in 1ms: There is no …
//	refused @ana tools/call: Unknown tool: gitty_nope
//
// who is "" where there is one caller and no need to name them. Everything
// a caller wrote is flattened to the line it is on and cut to a length: a
// log is read by people, and an argument is whatever a model chose to send.
func exchangeLine(who string, req *rpcRequest, resp *rpcResponse, took time.Duration) string {
	if who != "" {
		who = " @" + who
	}
	switch {
	case resp == nil:
		return ""
	case resp.Error != nil && req == nil:
		return "refused" + who + ": " + logText(resp.Error.Message, 200)
	case resp.Error != nil:
		return "refused" + who + " " + logText(req.Method, 60) + ": " + logText(resp.Error.Message, 200)
	case req == nil:
		return ""
	}
	result, _ := resp.Result.(map[string]any)
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		json.Unmarshal(req.Params, &p)
		client := strings.TrimSpace(p.ClientInfo.Name + " " + p.ClientInfo.Version)
		if client == "" {
			client = "a client that does not say what it is"
		}
		version, _ := result["protocolVersion"].(string)
		return "hello" + who + ": " + logText(client, 80) + ", MCP " + version
	case "server/discover":
		return "hello" + who + ": MCP " + protoModern
	case "tools/list":
		n := 0
		switch list := result["tools"].(type) {
		case []map[string]any:
			n = len(list)
		case []any:
			n = len(list)
		}
		return "list" + who + ": " + plural(n, "tool")
	case "tools/call":
		name, args, _ := callParams(req.Params)
		text, failed := resultText(result)
		line := "call" + who + " " + logText(name, 60) + " " + logArgs(args) + ": "
		if failed {
			return line + "error in " + tookText(took) + ": " + logText(text, 200)
		}
		return line + sizeText(len(text)) + " in " + tookText(took)
	}
	return ""
}

// resultText is what a tool result says, and whether it is an error. A
// result made here and one read back from another hub's answer are shaped
// alike and typed differently.
func resultText(result map[string]any) (text string, failed bool) {
	failed, _ = result["isError"].(bool)
	var first map[string]any
	switch content := result["content"].(type) {
	case []map[string]any:
		if len(content) > 0 {
			first = content[0]
		}
	case []any:
		if len(content) > 0 {
			first, _ = content[0].(map[string]any)
		}
	}
	text, _ = first["text"].(string)
	return text, failed
}

// logText makes something a caller wrote fit for a line of the log.
func logText(text string, max int) string {
	return clip(oneLine(text), max)
}

// logArgs is a tool call's arguments as one short line.
func logArgs(args json.RawMessage) string {
	var compact bytes.Buffer
	if json.Compact(&compact, args) != nil || compact.Len() == 0 || compact.String() == "null" {
		return "{}"
	}
	return logText(compact.String(), 200)
}

func tookText(d time.Duration) string {
	if d < time.Millisecond {
		return "under 1ms"
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(10 * time.Millisecond).String()
}

func sizeText(n int) string {
	if n < 1000 {
		return plural(n, "byte")
	}
	return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1000, 'f', 1, 64), ".0") + " KB"
}

// answerModern answers a request of the stateless revision, after holding
// it to what that revision requires of a request.
func (h *hub) answerModern(ctx context.Context, c *caller, req *rpcRequest, meta map[string]json.RawMessage,
	headers http.Header, headerVersion, bodyVersion string) (int, *rpcResponse) {
	bad := func(code int, msg string, data any) (int, *rpcResponse) {
		return http.StatusBadRequest, &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg, Data: data}}
	}
	if headers != nil {
		// What HTTP carries beside the body must agree with the body.
		switch {
		case headerVersion == "":
			return bad(codeHeaderMismatch, "Header mismatch: the MCP-Protocol-Version header is required", nil)
		case bodyVersion == "" && headerVersion != protoModern:
			return bad(codeBadVersion, "Unsupported protocol version", map[string]any{"supported": protoAll, "requested": headerVersion})
		case bodyVersion == "":
			return bad(codeInvalidParams, "the request is missing _meta[\""+metaVersion+"\"]", nil)
		case headerVersion != bodyVersion:
			return bad(codeHeaderMismatch, "Header mismatch: MCP-Protocol-Version header value '"+headerVersion+"' does not match body value '"+bodyVersion+"'", nil)
		}
	}
	if bodyVersion != protoModern {
		return bad(codeBadVersion, "Unsupported protocol version", map[string]any{"supported": protoAll, "requested": bodyVersion})
	}
	if _, ok := meta[metaCapabilities]; !ok {
		return bad(codeInvalidParams, "the request is missing _meta[\""+metaCapabilities+"\"]", nil)
	}
	if headers != nil {
		if got := headers.Get("Mcp-Method"); got != req.Method {
			return bad(codeHeaderMismatch, "Header mismatch: Mcp-Method header value '"+got+"' does not match body value '"+req.Method+"'", nil)
		}
	}

	// ok adds what every result of this revision carries.
	ok := func(result map[string]any) (int, *rpcResponse) {
		result["resultType"] = "complete"
		result["_meta"] = map[string]any{metaServerInfo: serverInfo()}
		return http.StatusOK, &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	switch req.Method {
	case "server/discover":
		return ok(map[string]any{
			"supportedVersions": protoAll,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      instructions,
			"ttlMs":             3_600_000,
			"cacheScope":        "public",
		})
	case "tools/list":
		// The same for every caller, and it changes only with a release.
		return ok(map[string]any{"tools": toolList(), "ttlMs": 3_600_000, "cacheScope": "public"})
	case "tools/call":
		name, args, err := callParams(req.Params)
		if err != nil {
			return bad(codeInvalidParams, err.Error(), nil)
		}
		if headers != nil {
			if got := decodeHeaderValue(headers.Get("Mcp-Name")); got != name {
				return bad(codeHeaderMismatch, "Header mismatch: Mcp-Name header value '"+got+"' does not match body value '"+name+"'", nil)
			}
		}
		t := toolNamed(name)
		if t == nil {
			return bad(codeInvalidParams, "Unknown tool: "+name, nil)
		}
		return ok(h.call(ctx, t, c, args))
	}
	return http.StatusNotFound, &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: codeMethodNotFound, Message: "Method not found: " + req.Method}}
}

// answerLegacy answers a client of the handshake revisions. The handshake
// is answered and then not needed: no session is issued, and every request
// after it is served on its own, like any other.
func (h *hub) answerLegacy(ctx context.Context, c *caller, req *rpcRequest) (int, *rpcResponse) {
	ok := func(result map[string]any) (int, *rpcResponse) {
		return http.StatusOK, &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	fail := func(code int, msg string) (int, *rpcResponse) {
		return http.StatusOK, &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}}
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		v := protoLegacy[0]
		if isLegacy(p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return ok(map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      serverInfo(),
			"instructions":    instructions,
		})
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		return ok(map[string]any{"tools": toolList()})
	case "tools/call":
		name, args, err := callParams(req.Params)
		if err != nil {
			return fail(codeInvalidParams, err.Error())
		}
		t := toolNamed(name)
		if t == nil {
			return fail(codeInvalidParams, "Unknown tool: "+name)
		}
		return ok(h.call(ctx, t, c, args))
	}
	return fail(codeMethodNotFound, "Method not found: "+req.Method)
}

func callParams(raw json.RawMessage) (name string, args json.RawMessage, err error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if e := json.Unmarshal(raw, &p); e != nil || p.Name == "" {
		return "", nil, errors.New("tools/call needs params.name")
	}
	return p.Name, p.Arguments, nil
}

// decodeHeaderValue undoes the encoding a client applies to a header value
// that plain ASCII cannot carry: =?base64?…?=
func decodeHeaderValue(v string) string {
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") && len(v) >= 11 {
		if raw, err := base64.StdEncoding.DecodeString(v[9 : len(v)-2]); err == nil {
			return string(raw)
		}
	}
	return v
}

// call runs a tool and puts what it says in a tool result. What goes wrong
// inside a tool is said in the result, where the model can read it and try
// again; only a request that is itself malformed is a protocol error.
func (h *hub) call(ctx context.Context, t *tool, c *caller, args json.RawMessage) map[string]any {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = json.RawMessage("{}")
	}
	h.st.seeOthers()
	out, err := t.run(ctx, h, c, args)
	if err != nil {
		var ask *errAsk
		var refused *errNote
		msg := err.Error()
		if !errors.As(err, &ask) && !errors.As(err, &refused) {
			h.logf("%s failed: %v", t.name, err)
			msg = "gitty-up could not answer: " + msg
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": out}}, "isError": false}
}
