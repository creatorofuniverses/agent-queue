// Package mcp exposes the daemon API as newline-delimited JSON-RPC over stdio.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/creatorofuniverses/agent-queue/internal/config"
	"io"
	"sync"
)

type Caller func(context.Context, string, any, any) error
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
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

func Serve(ctx context.Context, in io.Reader, out io.Writer, call Caller, agents []config.AgentConfig, env func(string) string, version string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Closing a closable input interrupts a blocked Scan when the server is cancelled.
	defer func() {
		if ctx.Err() != nil {
			if c, ok := in.(io.Closer); ok {
				_ = c.Close()
			}
		}
	}()
	type line struct {
		b   []byte
		err error
	}
	lines := make(chan line)
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 4096), 1<<20)
		for sc.Scan() {
			b := append([]byte(nil), sc.Bytes()...)
			select {
			case lines <- line{b: b}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case lines <- line{err: sc.Err()}:
		case <-ctx.Done():
		}
	}()
	enc := json.NewEncoder(out)
	var mu sync.Mutex
	write := func(r response) error { mu.Lock(); defer mu.Unlock(); return enc.Encode(r) }
	for {
		var l line
		select {
		case <-ctx.Done():
			return ctx.Err()
		case l = <-lines:
		}
		if l.b == nil {
			return l.err
		}
		var req request
		r := response{JSONRPC: "2.0", ID: json.RawMessage("null")}
		if !json.Valid(l.b) {
			r.Error = &rpcError{-32700, "parse error"}
			if err := write(r); err != nil {
				return err
			}
			continue
		}
		if err := json.Unmarshal(l.b, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
			r.Error = &rpcError{-32600, "invalid request"}
			if err := write(r); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 {
			continue
		} // notifications never receive responses
		r.ID = req.ID
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				r.Error = &rpcError{-32602, "invalid initialize params"}
				break
			}
			switch p.ProtocolVersion {
			case "2025-06-18", "2025-03-26", "2024-11-05":
			default:
				p.ProtocolVersion = "2025-06-18"
			}
			r.Result = map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "aq", "version": version}}
		case "ping":
			r.Result = map[string]any{}
		case "tools/list":
			r.Result = map[string]any{"tools": toolDefinitions()}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				r.Error = &rpcError{-32602, "invalid tools/call params"}
				break
			}
			r.Result = callTool(ctx, call, agents, env, p.Name, p.Arguments)
		default:
			r.Error = &rpcError{-32601, fmt.Sprintf("method not found: %s", req.Method)}
		}
		if err := write(r); err != nil {
			return err
		}
	}
}
