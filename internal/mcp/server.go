// Package mcp implements a Model Context Protocol server over stdio, so
// coding agents can drive Stratus (health, reset, snapshots, chaos and
// diagnostics) as tools.
//
// It speaks newline-delimited JSON-RPC 2.0 and implements the subset of MCP
// a tools-only server needs: initialize, ping, tools/list and tools/call.
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

// SupportedVersions lists protocol versions this server speaks, newest first.
var SupportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Tool is one callable tool.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object for the arguments.
	InputSchema map[string]any
	// Handler runs the tool. Its result is sent to the model as JSON text.
	// An error becomes a tool result with isError set, which the model sees.
	Handler func(ctx context.Context, args json.RawMessage) (any, error)
}

// Server is an MCP server with a fixed set of tools.
type Server struct {
	Name         string
	Version      string
	Instructions string
	tools        []Tool
	byName       map[string]Tool
}

// NewServer builds a server and rejects duplicate tool names.
func NewServer(name, version, instructions string, tools ...Tool) (*Server, error) {
	byName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		if _, dup := byName[t.Name]; dup {
			return nil, fmt.Errorf("duplicate tool %q", t.Name)
		}
		byName[t.Name] = t
	}
	return &Server{Name: name, Version: version, Instructions: instructions, tools: tools, byName: byName}, nil
}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

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

const maxMessageSize = 8 << 20

// Serve reads requests from r and writes responses to w until r is
// exhausted or ctx is cancelled. Requests are handled one at a time.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxMessageSize)
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	send := func(resp response) error {
		mu.Lock()
		defer mu.Unlock()
		return enc.Encode(resp)
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		resp, ok := s.handle(ctx, line)
		if !ok {
			continue // notification: no response
		}
		if err := send(resp); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read request: %w", err)
	}
	return nil
}

// handle processes one message. ok is false for notifications.
func (s *Server) handle(ctx context.Context, line []byte) (resp response, ok bool) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(json.RawMessage("null"), codeParseError, "parse error: "+err.Error()), true
	}
	isNotification := len(req.ID) == 0
	if req.JSONRPC != "2.0" || req.Method == "" {
		if isNotification {
			return response{}, false
		}
		return errorResponse(req.ID, codeInvalidRequest, "invalid JSON-RPC 2.0 request"), true
	}
	if isNotification {
		return response{}, false
	}

	switch req.Method {
	case "initialize":
		return result(req.ID, s.initialize(req.Params)), true
	case "ping":
		return result(req.ID, map[string]any{}), true
	case "tools/list":
		return result(req.ID, s.listTools()), true
	case "tools/call":
		res, rpcErr := s.callTool(ctx, req.Params)
		if rpcErr != nil {
			return response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}, true
		}
		return result(req.ID, res), true
	default:
		return errorResponse(req.ID, codeMethodNotFound, "method not found: "+req.Method), true
	}
}

func result(id json.RawMessage, v any) response {
	return response{JSONRPC: "2.0", ID: id, Result: v}
}

func errorResponse(id json.RawMessage, code int, msg string) response {
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := SupportedVersions[0]
	for _, v := range SupportedVersions {
		if v == p.ProtocolVersion {
			version = v
		}
	}
	out := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
	}
	if s.Instructions != "" {
		out["instructions"] = s.Instructions
	}
	return out
}

func (s *Server) listTools() map[string]any {
	tools := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
		})
	}
	return map[string]any{"tools": tools}
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (*toolResult, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call needs a tool name"}
	}
	tool, ok := s.byName[p.Name]
	if !ok {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}

	out, err := tool.Handler(ctx, args)
	if err != nil {
		return &toolResult{Content: []textContent{{Type: "text", Text: err.Error()}}, IsError: true}, nil
	}
	text, err := render(out)
	if err != nil {
		return &toolResult{Content: []textContent{{Type: "text", Text: "encode result: " + err.Error()}}, IsError: true}, nil
	}
	return &toolResult{Content: []textContent{{Type: "text", Text: text}}}, nil
}

func render(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case json.RawMessage:
		var buf any
		if err := json.Unmarshal(x, &buf); err != nil {
			return string(x), nil
		}
		v = buf
	}
	b, err := json.MarshalIndent(v, "", "  ")
	return string(b), err
}
