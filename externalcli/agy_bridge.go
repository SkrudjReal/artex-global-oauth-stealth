package externalcli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
)

type mcpCall struct {
	ID, Name  string
	Arguments json.RawMessage
	Result    chan llm.ContentBlock
	Done      <-chan struct{}
}
type agyBridge struct {
	URL    string
	Calls  chan *mcpCall
	server *http.Server
	cancel context.CancelFunc
	ctx    context.Context
	tools  []llm.ToolSchema
}

func newAGYBridge(parent context.Context, tools []llm.ToolSchema) (*agyBridge, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	b := &agyBridge{URL: "http://" + listener.Addr().String() + "/" + hex.EncodeToString(secret[:]), Calls: make(chan *mcpCall, 1), ctx: ctx, cancel: cancel, tools: tools}
	b.server = &http.Server{Handler: http.HandlerFunc(b.serve), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 8192}
	go func() { _ = b.server.Serve(listener) }()
	return b, nil
}
func (b *agyBridge) close() { b.cancel(); _ = b.server.Close() }
func (b *agyBridge) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != strings.TrimPrefix(b.URL, "http://"+r.Host) || r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", 403)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var q struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q) != nil || q.Version != "2.0" {
		http.Error(w, "invalid MCP request", 400)
		return
	}
	if len(q.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	var result any
	var rpcErr any
	switch q.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]string{"name": "artex", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		tools := []any{}
		for _, tool := range b.tools {
			tools = append(tools, map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": tool.InputSchema})
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		var args struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		valid := json.Unmarshal(q.Params, &args) == nil && json.Valid(args.Arguments)
		found := false
		for _, tool := range b.tools {
			if args.Name == tool.Name {
				found = true
			}
		}
		if !valid || !found {
			rpcErr = map[string]any{"code": -32602, "message": "Tool not available in this ARTEX session"}
			break
		}
		var id [16]byte
		_, err := rand.Read(id[:])
		if err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		call := &mcpCall{ID: "mcp-" + hex.EncodeToString(id[:]), Name: args.Name, Arguments: args.Arguments, Result: make(chan llm.ContentBlock, 1), Done: r.Context().Done()}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		select {
		case b.Calls <- call:
		case <-ctx.Done():
			return
		case <-b.ctx.Done():
			return
		}
		select {
		case block := <-call.Result:
			content := []any{}
			for _, part := range block.Content {
				if part.Type != llm.BlockText {
					rpcErr = map[string]any{"code": -32603, "message": "Unsupported ARTEX result content"}
					break
				}
				content = append(content, map[string]string{"type": "text", "text": part.Text})
			}
			result = map[string]any{"content": content, "isError": block.IsError}
		case <-ctx.Done():
			return
		case <-b.ctx.Done():
			return
		}
	default:
		rpcErr = map[string]any{"code": -32601, "message": "Method not supported"}
	}
	response := map[string]any{"jsonrpc": "2.0", "id": q.ID}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
func (c *mcpCall) answer(ctx context.Context, result llm.ContentBlock) error {
	select {
	case <-c.Done:
		return errors.New("AGY disconnected before receiving the ARTEX result")
	default:
	}
	select {
	case c.Result <- result:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.Done:
		return errors.New("AGY tool connection closed")
	}
}
