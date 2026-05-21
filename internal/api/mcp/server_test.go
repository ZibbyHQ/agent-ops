package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZibbyHQ/agent-ops/internal/driver"
	"github.com/ZibbyHQ/agent-ops/internal/scheduler"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/task"
	"github.com/ZibbyHQ/agent-ops/internal/tool"
)

type fakeDriver struct{}

func (fakeDriver) Name() string { return "fake" }
func (fakeDriver) Run(_ context.Context, req driver.Request) (driver.Result, error) {
	return driver.Result{FinalMessage: "ran: " + req.UserPrompt}, nil
}

func setup(t *testing.T) (*httptest.Server, *state.Store, *scheduler.Scheduler) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	tools := tool.NewRegistry()
	_ = tools.Register(tool.NewShellTool())

	runner := task.NewRunner(fakeDriver{}, tools, st)
	sched := scheduler.New(runner, st, slog.Default())
	sched.Start()
	t.Cleanup(func() { _ = sched.Stop(context.Background()) })

	srv := New(Config{
		Scheduler: sched,
		Store:     st,
		Tools:     tools,
		Token:     "test-token",
	})

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	return httpSrv, st, sched
}

func rpcCall(t *testing.T, base, method string, params any) []byte {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	req, _ := http.NewRequest("POST", base+"/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rpc %s: status %d", method, resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	return out
}

func decodeResult(t *testing.T, raw []byte, into any) {
	t.Helper()
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v; raw=%s", err, raw)
	}
	if len(resp.Error) > 0 {
		t.Fatalf("rpc error: %s", resp.Error)
	}
	if err := json.Unmarshal(resp.Result, into); err != nil {
		t.Fatalf("decode result: %v; result=%s", err, resp.Result)
	}
}

func TestInitialize(t *testing.T) {
	srv, _, _ := setup(t)
	var got struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	decodeResult(t, rpcCall(t, srv.URL, "initialize", map[string]any{}), &got)
	if got.ProtocolVersion == "" {
		t.Fatal("no protocolVersion")
	}
	if got.ServerInfo.Name != "agent-ops" {
		t.Fatalf("server name = %q", got.ServerInfo.Name)
	}
	if got.Capabilities.Tools == nil {
		t.Fatal("no tools capability advertised")
	}
}

func TestToolsList_ContainsAllBuiltinsAndHostTools(t *testing.T) {
	srv, _, _ := setup(t)
	var got struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/list", map[string]any{}), &got)

	names := map[string]struct{}{}
	for _, t := range got.Tools {
		names[t.Name] = struct{}{}
	}
	for _, want := range []string{
		"agent_status", "agent_run_now", "agent_history", "agent_logs",
		"agent_list_tasks", "agent_get_task", "agent_set_task",
		"host_shell",
	} {
		if _, ok := names[want]; !ok {
			t.Errorf("tool %q missing from tools/list (got %v)", want, names)
		}
	}
}

func TestAuth_RejectsMissingToken(t *testing.T) {
	srv, _, _ := setup(t)
	resp, err := http.Post(srv.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unauthorized") {
		t.Fatalf("expected unauthorized, got %s", body)
	}
}

func TestAuth_RejectsWrongToken(t *testing.T) {
	srv, _, _ := setup(t)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Authorization", "Bearer not-the-real-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unauthorized") {
		t.Fatalf("expected unauthorized, got %s", body)
	}
}

func TestSetTask_RunNow_HistoryEndToEnd(t *testing.T) {
	srv, _, _ := setup(t)

	// 1. Create a task
	var setRes struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name": "agent_set_task",
		"arguments": map[string]any{
			"name":   "smoke",
			"cron":   "0 9 * * 1",
			"prompt": "say hello",
		},
	}), &setRes)
	if setRes.IsError {
		t.Fatalf("set_task error: %s", setRes.Content[0].Text)
	}

	// 2. Run it
	var runRes struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_run_now",
		"arguments": map[string]any{"task_name": "smoke"},
	}), &runRes)
	if runRes.IsError {
		t.Fatalf("run_now error: %s", runRes.Content[0].Text)
	}
	if !strings.Contains(runRes.Content[0].Text, "completed") {
		t.Fatalf("expected status=completed in run_now output, got %s", runRes.Content[0].Text)
	}

	// 3. History should now include it
	time.Sleep(50 * time.Millisecond) // give the runner a beat
	var histRes struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_history",
		"arguments": map[string]any{"task_name": "smoke", "limit": 10},
	}), &histRes)
	if !strings.Contains(histRes.Content[0].Text, "smoke") {
		t.Fatalf("history missing the smoke run: %s", histRes.Content[0].Text)
	}
}

func TestHostToolPassthrough(t *testing.T) {
	srv, _, _ := setup(t)
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "host_shell",
		"arguments": map[string]any{"command": "echo hello-from-host"},
	}), &res)
	if res.IsError {
		t.Fatalf("host_shell error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "hello-from-host") {
		t.Fatalf("expected echo output, got %s", res.Content[0].Text)
	}
}

func TestMissionFlow_SetGetRemember(t *testing.T) {
	srv, store, _ := setup(t)

	// 1. set a mission statement
	var setRes struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_set_mission",
		"arguments": map[string]any{"statement": "I steward the OpenDesign instance."},
	}), &setRes)
	if setRes.IsError {
		t.Fatalf("set_mission error: %s", setRes.Content[0].Text)
	}

	// 2. add a fact
	var rememberRes struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name": "agent_remember_fact",
		"arguments": map[string]any{
			"fact":   "Postgres 16 listening on :5432, password in /etc/myapp/db.conf",
			"source": "bootstrap",
		},
	}), &rememberRes)
	if rememberRes.IsError {
		t.Fatalf("remember_fact error: %s", rememberRes.Content[0].Text)
	}

	// 3. read it back
	var getRes struct {
		Content []struct{ Text string }
		IsError bool
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/call", map[string]any{
		"name":      "agent_get_mission",
		"arguments": map[string]any{},
	}), &getRes)
	text := getRes.Content[0].Text
	if !strings.Contains(text, "I steward the OpenDesign instance.") {
		t.Fatalf("statement missing from get_mission output:\n%s", text)
	}
	if !strings.Contains(text, "Postgres 16") {
		t.Fatalf("fact missing from get_mission output:\n%s", text)
	}

	// 4. underlying store reflects the same — belt + suspenders so a future
	// MCP regression that silently fails to persist would be caught.
	m, err := store.GetMission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Statement == "" || len(m.Facts) == 0 {
		t.Fatalf("store didn't persist mission: %+v", m)
	}
}

func TestToolsList_IncludesMissionTools(t *testing.T) {
	srv, _, _ := setup(t)
	var got struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	decodeResult(t, rpcCall(t, srv.URL, "tools/list", map[string]any{}), &got)
	names := map[string]struct{}{}
	for _, t := range got.Tools {
		names[t.Name] = struct{}{}
	}
	for _, want := range []string{"agent_get_mission", "agent_set_mission", "agent_remember_fact"} {
		if _, ok := names[want]; !ok {
			t.Errorf("mission tool %q missing from tools/list", want)
		}
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv, _, _ := setup(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}
}
