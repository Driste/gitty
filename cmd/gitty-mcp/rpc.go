package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The Model Context Protocol over stdio: JSON-RPC 2.0, one message per line.
//
// Only what a tool server needs is implemented — the handshake, ping, the
// tool list and tool calls, and cancellation — by hand, on the standard
// library. stdout carries protocol messages and nothing else; everything a
// person might want to read goes to stderr.

// protocolVersions are the revisions of the protocol this server speaks,
// newest first. A client that asks for one of them gets it; a client that
// asks for anything else is offered the newest, and decides for itself
// whether it can live with that.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// maxMessageBytes bounds one incoming message. Nothing this server is asked
// comes near it; a line beyond it is a broken or hostile peer.
const maxMessageBytes = 4 << 20

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

func rpcErrorf(code int, format string, a ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, a...)}
}

var nullID = json.RawMessage("null")

// conn is one client connection: the message loop, the writer both it and
// the tool calls share, and the calls in flight.
type conn struct {
	srv *server
	out io.Writer

	writeMu sync.Mutex

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
	wg       sync.WaitGroup
}

// serve reads messages until the input ends or the context is cancelled.
//
// Tool calls run concurrently with the loop, so a long one — a clone — does
// not keep the server from answering a ping or hearing that the call was
// cancelled. When the input ends, the calls already under way are finished
// and answered before serve returns: a client that pipes its requests in and
// closes the pipe gets every answer, and one that wants the server gone at
// once signals it, which cancels them through the context.
func (s *server) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := &conn{srv: s, out: out, inflight: map[string]context.CancelFunc{}}
	defer c.wg.Wait()
	defer cancel()

	// Reading blocks, and a blocked read cannot be cancelled: do it on its
	// own goroutine and let the loop choose between a line and the context.
	type line struct {
		data []byte
		err  error
	}
	lines := make(chan line)
	go func() {
		r := bufio.NewReaderSize(in, 64<<10)
		for {
			data, err := readLine(r)
			select {
			case lines <- line{data, err}:
			case <-ctx.Done():
				return
			}
			// A message that was too long has been skipped to its end:
			// the stream is still good, and reading goes on.
			if err != nil && !errors.Is(err, errMessageTooLong) {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case l := <-lines:
			if len(bytes.TrimSpace(l.data)) > 0 {
				c.dispatch(ctx, l.data)
			}
			switch {
			case errors.Is(l.err, io.EOF):
				c.wg.Wait()
				return nil
			case errors.Is(l.err, errMessageTooLong):
				c.write(rpcResponse{JSONRPC: "2.0", ID: nullID, Error: rpcErrorf(codeParseError, "message longer than %d bytes", maxMessageBytes)})
			case l.err != nil:
				return l.err
			}
		}
	}
}

var errMessageTooLong = errors.New("message too long")

// readLine reads one line of any length up to maxMessageBytes. A longer one
// is consumed to its end and reported, so the stream stays in step.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			buf = append(buf, chunk...)
			if len(buf) > maxMessageBytes {
				buf, tooLong = nil, true
			}
		}
		switch {
		case err == nil:
			if tooLong {
				return nil, errMessageTooLong
			}
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			if tooLong {
				return nil, errMessageTooLong
			}
			return buf, err
		}
	}
}

// dispatch handles one line: a message, or a batch of them.
func (c *conn) dispatch(ctx context.Context, data []byte) {
	trimmed := bytes.TrimSpace(data)
	if trimmed[0] != '[' {
		if resp, ok := c.message(ctx, trimmed, true); ok {
			c.write(resp)
		}
		return
	}
	var batch []json.RawMessage
	if err := json.Unmarshal(trimmed, &batch); err != nil {
		c.write(rpcResponse{JSONRPC: "2.0", ID: nullID, Error: rpcErrorf(codeParseError, "parse error: %v", err)})
		return
	}
	if len(batch) == 0 {
		c.write(rpcResponse{JSONRPC: "2.0", ID: nullID, Error: rpcErrorf(codeInvalidRequest, "empty batch")})
		return
	}
	// A batch is answered as a batch, so its members are handled in place.
	var responses []rpcResponse
	for _, raw := range batch {
		if resp, ok := c.message(ctx, raw, false); ok {
			responses = append(responses, resp)
		}
	}
	if len(responses) > 0 {
		c.write(responses)
	}
}

// message handles one JSON-RPC message and returns the response to send now,
// if there is one. With async set, a tool call is started in the background
// and answers for itself when it is done.
func (c *conn) message(ctx context.Context, raw []byte, async bool) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return rpcResponse{JSONRPC: "2.0", ID: nullID, Error: rpcErrorf(codeParseError, "parse error: %v", err)}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: nullID, Error: rpcErrorf(codeInvalidRequest, "invalid request: %v", err)}, true
	}
	hasID := len(req.ID) > 0 && !bytes.Equal(req.ID, nullID)
	id := req.ID
	if !hasID {
		id = nullID
	}
	if req.Method == "" {
		// A response to a request this server never made, or junk.
		var probe struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(raw, &probe) == nil && (probe.Result != nil || probe.Error != nil) {
			return rpcResponse{}, false
		}
		return rpcResponse{JSONRPC: "2.0", ID: id, Error: rpcErrorf(codeInvalidRequest, "invalid request: no method")}, true
	}
	if req.JSONRPC != "2.0" {
		if !hasID {
			return rpcResponse{}, false
		}
		return rpcResponse{JSONRPC: "2.0", ID: id, Error: rpcErrorf(codeInvalidRequest, `invalid request: jsonrpc must be "2.0"`)}, true
	}

	// Notifications are never answered.
	if !hasID {
		c.notification(req)
		return rpcResponse{}, false
	}

	if req.Method == "tools/call" && async {
		callCtx, cancel := context.WithCancel(ctx)
		key := string(req.ID)
		c.mu.Lock()
		c.inflight[key] = cancel
		c.mu.Unlock()
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			resp := c.respond(callCtx, req)
			c.mu.Lock()
			delete(c.inflight, key)
			c.mu.Unlock()
			cancelled := callCtx.Err() != nil
			cancel()
			// A cancelled call is not answered: the client has stopped
			// listening for it.
			if !cancelled {
				c.write(resp)
			}
		}()
		return rpcResponse{}, false
	}
	return c.respond(ctx, req), true
}

func (c *conn) respond(ctx context.Context, req rpcRequest) rpcResponse {
	result, err := c.srv.handle(ctx, req.Method, req.Params)
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if err != nil {
		var rerr *rpcError
		if !errors.As(err, &rerr) {
			rerr = rpcErrorf(codeInternalError, "%v", err)
		}
		resp.Error = rerr
		return resp
	}
	resp.Result = result
	return resp
}

// notification acts on the one notification that matters to a tool server:
// the client giving up on a call.
func (c *conn) notification(req rpcRequest) {
	if req.Method != "notifications/cancelled" {
		return
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(req.Params, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	c.mu.Lock()
	cancel := c.inflight[string(p.RequestID)]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// write sends one message: a single line, however long.
func (c *conn) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		data, _ = json.Marshal(rpcResponse{JSONRPC: "2.0", ID: nullID, Error: rpcErrorf(codeInternalError, "encoding a response failed: %v", err)})
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.out.Write(append(data, '\n'))
}

// handle answers one request.
func (s *server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, rpcErrorf(codeInvalidParams, "invalid initialize parameters: %v", err)
			}
		}
		return map[string]any{
			"protocolVersion": negotiateVersion(p.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": serverName, "version": version},
			"instructions":    s.instructions(),
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.toolList()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
			return nil, rpcErrorf(codeInvalidParams, "invalid tools/call parameters: a tool name is required")
		}
		t := s.tool(p.Name)
		if t == nil {
			return nil, rpcErrorf(codeInvalidParams, "Unknown tool: %s", p.Name)
		}
		return s.call(ctx, t, p.Arguments), nil
	}
	return nil, rpcErrorf(codeMethodNotFound, "Method not found: %s", method)
}

// negotiateVersion picks the protocol revision to answer an initialize with.
func negotiateVersion(requested string) string {
	for _, v := range protocolVersions {
		if v == requested {
			return v
		}
	}
	return protocolVersions[0]
}
