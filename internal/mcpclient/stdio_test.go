// Copyright 2026 Zibby Lab. Apache-2.0.

package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the fake MCP subprocess. When the test binary is
// re-invoked with MCPCLIENT_FAKE_STDIO=1 in the env, it sheds its test
// identity, runs the stdio MCP server loop, and exits — that subprocess
// is what stdio_test.go spawns to drive the real stdioClient through a
// real fork/exec path.
//
// This trick avoids depending on an external binary in the test (would
// have to ship one in testdata + build it per-OS) and works on every
// platform Go supports.
func TestMain(m *testing.M) {
	if os.Getenv("MCPCLIENT_FAKE_STDIO") == "1" {
		runFakeStdioServer()
		return
	}
	os.Exit(m.Run())
}

// runFakeStdioServer is the body of the subprocess. It implements
// initialize / tools/list / tools/call and exits on EOF.
func runFakeStdioServer() {
	mode := os.Getenv("MCPCLIENT_FAKE_MODE") // "ok" | "crash-after-init"
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 64*1024), 8<<20)
	initCount := 0
	for scan.Scan() {
		line := scan.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			initCount++
			writeFakeStdio(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "fake-stdio"},
			})
			if mode == "crash-after-init" && initCount == 1 {
				// Bail after answering the first initialize so the
				// supervisor exercises its restart path.
				os.Exit(7)
			}
		case "notifications/initialized":
			// no-op (notification)
		case "tools/list":
			writeFakeStdio(req.ID, map[string]any{
				"tools": []ToolDef{{
					Name: "echo", Description: "echo back args as text",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				}},
			})
		case "tools/call":
			var p struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			writeFakeStdio(req.ID, map[string]any{
				"content": []map[string]any{{
					"type": "text",
					"text": fmt.Sprintf("called=%s args=%s", p.Name, string(p.Args)),
				}},
				"isError": false,
			})
		default:
			writeFakeStdio(req.ID, nil)
		}
		os.Stdout.Sync()
	}
}

func writeFakeStdio(id int, result any) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
	_, _ = io.WriteString(os.Stdout, string(body)+"\n")
}

// spawnFakeStdio re-execs this test binary in subprocess mode. Returns
// the Config + cleanup that the caller passes to New + Start.
func spawnFakeStdioConfig(t *testing.T, mode string) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: confirm the test binary exists + is runnable.
	if _, err := exec.LookPath(exe); err != nil {
		t.Fatal(err)
	}
	return Config{
		Name:      "fake",
		Transport: TransportStdio,
		Command:   exe,
		// `-test.run` filter limits the re-exec to a no-op test so the
		// runner doesn't try to re-run all the tests inside the child.
		Args: []string{"-test.run", "^TestStdioFakeSubprocess$"},
		Env: map[string]string{
			"MCPCLIENT_FAKE_STDIO": "1",
			"MCPCLIENT_FAKE_MODE":  mode,
		},
	}
}

// TestStdioFakeSubprocess is a no-op test that exists so the child binary
// has a -test.run target that matches. The child never actually runs the
// test body — TestMain returns before that.
func TestStdioFakeSubprocess(t *testing.T) {}

func TestStdioClient_HappyPath(t *testing.T) {
	cfg := spawnFakeStdioConfig(t, "ok")
	c, err := New(cfg)
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
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	res, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"hi":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "called=echo") || !strings.Contains(res.Text, `{"hi":1}`) {
		t.Errorf("unexpected text: %q", res.Text)
	}
}

func TestStdioClient_ContextCancelDuringCall(t *testing.T) {
	// The fake echo replies fast, so we cancel the context BEFORE the call
	// to assert ctx.Err() propagates straight back rather than us blocking
	// on a never-arriving response.
	cfg := spawnFakeStdioConfig(t, "ok")
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CallTool(ctx, "echo", nil); err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestStdioClient_Close_KillsSubprocess(t *testing.T) {
	cfg := spawnFakeStdioConfig(t, "ok")
	c, _ := New(cfg)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = c.Initialize(context.Background())
	// Give the reader goroutine a tick to settle.
	time.Sleep(50 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// Close MUST be idempotent.
	if err := c.Close(); err != nil {
		t.Fatalf("second close errored: %v", err)
	}
}
