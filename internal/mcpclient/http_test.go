// Copyright 2026 Zibby Lab. Apache-2.0.

package mcpclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeMCPServer is a tiny in-memory MCP server good enough for client
// round-trip tests. It does NOT implement the spec exhaustively — just
// initialize / tools/list / tools/call / a synthetic error path.
type fakeMCPServer struct {
	requireBearer string                    // if non-empty, demand exact match
	tools         []ToolDef                 // returned by tools/list
	callHandler   func(name string, args json.RawMessage) (CallResult, error)
	gotAuth       string                    // captured for assertions
	requests      []rpcRequest              // captured for assertions
}

func (f *fakeMCPServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var req rpcRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json: "+err.Error(), 400)
			return
		}
		f.requests = append(f.requests, req)

		if f.requireBearer != "" && r.Header.Get("Authorization") != "Bearer "+f.requireBearer {
			writeRPCError(w, req.ID, -32001, "unauthorized")
			return
		}

		switch req.Method {
		case "initialize":
			writeRPCResult(w, req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "0.0.0"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPCResult(w, req.ID, map[string]any{"tools": f.tools})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if f.callHandler == nil {
				writeRPCError(w, req.ID, -32601, "no call handler")
				return
			}
			res, err := f.callHandler(p.Name, p.Arguments)
			if err != nil {
				writeRPCError(w, req.ID, -32000, err.Error())
				return
			}
			writeRPCResult(w, req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": res.Text}},
				"isError": res.IsError,
			})
		default:
			writeRPCError(w, req.ID, -32601, "no such method "+req.Method)
		}
	})
}

func writeRPCResult(w http.ResponseWriter, id int, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
}

func writeRPCError(w http.ResponseWriter, id int, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg},
	})
}

// TestHTTPClient_HappyPath dials a fake MCP server, lists tools, calls
// one, and asserts the response flows through.
func TestHTTPClient_HappyPath(t *testing.T) {
	fake := &fakeMCPServer{
		requireBearer: "secret-token",
		tools: []ToolDef{
			{Name: "trigger_workflow", Description: "do a thing",
				InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		callHandler: func(name string, args json.RawMessage) (CallResult, error) {
			return CallResult{Text: "ok for " + name + " args=" + string(args)}, nil
		},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c, err := New(Config{
		Name: "zibby", Transport: TransportHTTP, URL: srv.URL, AuthToken: "secret-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("listtools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "trigger_workflow" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	res, err := c.CallTool(context.Background(), "trigger_workflow", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("calltool: %v", err)
	}
	if !strings.Contains(res.Text, "ok for trigger_workflow") {
		t.Errorf("unexpected text: %q", res.Text)
	}
	if fake.gotAuth != "Bearer secret-token" {
		t.Errorf("bearer not forwarded; got %q", fake.gotAuth)
	}
}

// TestHTTPClient_RejectsBadBearer pins the auth pass-through behavior.
func TestHTTPClient_RejectsBadBearer(t *testing.T) {
	fake := &fakeMCPServer{requireBearer: "right"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c, _ := New(Config{Name: "x", Transport: TransportHTTP, URL: srv.URL, AuthToken: "wrong"})
	_ = c.Start(context.Background())
	defer c.Close()
	if err := c.Initialize(context.Background()); err == nil {
		t.Fatal("expected unauthorized error")
	}
}

// TestHTTPClient_HTTPErrorPropagates ensures non-200 responses surface
// with the body included, so debug logs are actionable.
func TestHTTPClient_HTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the database is on fire", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := New(Config{Name: "x", Transport: TransportHTTP, URL: srv.URL})
	_ = c.Start(context.Background())
	defer c.Close()
	err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "http 500") || !strings.Contains(err.Error(), "database is on fire") {
		t.Errorf("error doesn't mention http status or body: %v", err)
	}
}

// TestHTTPClient_CallToolIsError_Surfaces ensures a tool returning
// isError:true is parsed and surfaced via CallResult.IsError.
func TestHTTPClient_CallToolIsError_Surfaces(t *testing.T) {
	fake := &fakeMCPServer{
		callHandler: func(name string, _ json.RawMessage) (CallResult, error) {
			return CallResult{Text: "boom", IsError: true}, nil
		},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c, _ := New(Config{Name: "x", Transport: TransportHTTP, URL: srv.URL})
	_ = c.Start(context.Background())
	defer c.Close()
	_ = c.Initialize(context.Background())
	res, err := c.CallTool(context.Background(), "anything", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Text != "boom" {
		t.Errorf("unexpected: %+v", res)
	}
}

// TestNew_RejectsBadConfig pins the validation messages so the CLI/MCP
// surfaces clear errors when an operator types the wrong transport.
func TestNew_RejectsBadConfig(t *testing.T) {
	if _, err := New(Config{Name: "x", Transport: "smtp"}); err == nil {
		t.Fatal("expected unknown-transport error")
	}
	if _, err := New(Config{Name: "x", Transport: TransportHTTP}); err == nil {
		t.Fatal("expected missing-url error")
	}
	if _, err := New(Config{Name: "x", Transport: TransportStdio}); err == nil {
		t.Fatal("expected missing-command error")
	}
}
