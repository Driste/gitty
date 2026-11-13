package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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
	fail := func(status int, id json.RawMessage, code int, msg string, data any) (int, *rpcResponse) {
		return status, &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg, Data: data}}
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
		return http.StatusAccepted, nil
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
		return h.answerLegacy(ctx, c, &req)
	}
	return h.answerModern(ctx, c, &req, params.Meta, headers, headerVersion, bodyVersion)
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
