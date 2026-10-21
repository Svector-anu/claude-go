// Package mcp is a minimal Model Context Protocol server over stdio: newline
// delimited JSON-RPC 2.0, tools only. It exists so claude-go can offer tools to
// Claude Code without a runtime or SDK dependency.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Protocol versions this server can speak, newest first.
var supportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const (
	maxMessageBytes = 4 << 20
	maxResultBytes  = 100 << 10

	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// Result is what a tool returns. Failures the model can act on (bad input, a
// refused secret) are results with IsError set, not protocol errors.
type Result struct {
	Text    string
	IsError bool
}

// Tool is one callable tool.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object describing the arguments.
	InputSchema map[string]any
	// Annotations carry hints such as readOnlyHint and destructiveHint.
	Annotations map[string]any
	Handler     func(ctx context.Context, args json.RawMessage) Result
}

// Server serves tools to one client.
type Server struct {
	Name         string
	Version      string
	Instructions string
	Tools        []Tool

	mu  sync.Mutex
	out io.Writer
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) write(resp response) {
	resp.JSONRPC = "2.0"
	if resp.ID == nil {
		resp.ID = json.RawMessage("null")
	}
	data, err := json.Marshal(resp)
	if err != nil {
		data = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"could not encode the response"}}`)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(append(data, '\n'))
}

func (s *Server) fail(id json.RawMessage, code int, msg string) {
	s.write(response{ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// Serve reads requests from r and writes responses to w until r ends or ctx is
// cancelled. Requests are handled one at a time, in order. Reading happens in a
// goroutine so cancellation is honoured even while no input is arriving; that
// goroutine ends with the process or when r closes.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.out = w
	type message struct {
		line    []byte
		tooLong bool
		err     error
	}
	lines := make(chan message)
	go func() {
		br := bufio.NewReaderSize(r, 64<<10)
		for {
			line, tooLong, err := readLine(br)
			select {
			case lines <- message{line, tooLong, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-lines:
			if m.err != nil {
				if errors.Is(m.err, io.EOF) {
					return nil
				}
				return fmt.Errorf("read request: %w", m.err)
			}
			switch {
			case m.tooLong:
				s.fail(nil, codeInvalidRequest, "message too large")
			case len(trimSpace(m.line)) > 0:
				s.handle(ctx, m.line)
			}
		}
	}
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r' || b[len(b)-1] == '\n') {
		b = b[:len(b)-1]
	}
	return b
}

// readLine returns the next line. A line over the limit is consumed in full and
// reported as tooLong, so one oversized message cannot desynchronise the stream.
func readLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		frag, isPrefix, err := br.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) && len(buf) > 0 {
				return buf, tooLong, nil
			}
			return nil, false, err
		}
		if !tooLong {
			if len(buf)+len(frag) > maxMessageBytes {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, frag...)
			}
		}
		if !isPrefix {
			return buf, tooLong, nil
		}
	}
}

func (s *Server) handle(ctx context.Context, raw []byte) {
	if t := trimSpace(raw); len(t) > 0 && t[0] == '[' {
		s.fail(nil, codeInvalidRequest, "batch requests are not supported")
		return
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		s.fail(nil, codeParse, "parse error")
		return
	}
	isNotification := req.ID == nil
	if req.JSONRPC != "2.0" || req.Method == "" {
		if !isNotification {
			s.fail(req.ID, codeInvalidRequest, "not a JSON-RPC 2.0 request")
		}
		return
	}
	if isNotification {
		return // notifications (initialized, cancelled, …) need no answer
	}

	switch req.Method {
	case "initialize":
		s.initialize(req)
	case "ping":
		s.write(response{ID: req.ID, Result: struct{}{}})
	case "tools/list":
		s.write(response{ID: req.ID, Result: map[string]any{"tools": s.toolList()}})
	case "tools/call":
		s.call(ctx, req)
	default:
		s.fail(req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *Server) initialize(req request) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &p) // a client that omits it simply gets our newest
	version := supportedVersions[0]
	for _, v := range supportedVersions {
		if v == p.ProtocolVersion {
			version = v
		}
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
	}
	if s.Instructions != "" {
		result["instructions"] = s.Instructions
	}
	s.write(response{ID: req.ID, Result: result})
}

func (s *Server) toolList() []map[string]any {
	out := make([]map[string]any, 0, len(s.Tools))
	for _, t := range s.Tools {
		entry := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
		if len(t.Annotations) > 0 {
			entry["annotations"] = t.Annotations
		}
		out = append(out, entry)
	}
	return out
}

func (s *Server) call(ctx context.Context, req request) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		s.fail(req.ID, codeInvalidParams, "tools/call needs a tool name")
		return
	}
	var tool *Tool
	for i := range s.Tools {
		if s.Tools[i].Name == p.Name {
			tool = &s.Tools[i]
		}
	}
	if tool == nil {
		s.fail(req.ID, codeInvalidParams, "unknown tool: "+p.Name)
		return
	}
	if len(p.Arguments) == 0 {
		p.Arguments = json.RawMessage("{}")
	}
	res := runTool(ctx, tool, p.Arguments)
	if len(res.Text) > maxResultBytes {
		res.Text = res.Text[:maxResultBytes] + "\n[output truncated]"
	}
	s.write(response{ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": res.Text}},
		"isError": res.IsError,
	}})
}

// runTool shields the server from a panicking handler.
func runTool(ctx context.Context, t *Tool, args json.RawMessage) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = Result{Text: fmt.Sprintf("the %s tool failed unexpectedly", t.Name), IsError: true}
		}
	}()
	return t.Handler(ctx, args)
}
