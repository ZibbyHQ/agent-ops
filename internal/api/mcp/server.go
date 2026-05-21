// Copyright 2026 Zibby Lab. Apache-2.0.

// Package mcp implements the Model Context Protocol server that the daemon
// exposes to remote agents (the user's local Claude Code / Cursor / Codex /
// Gemini CLI).
//
// Transport: Streamable HTTP per MCP 1.x spec.
//   POST /mcp  → JSON-RPC request, JSON-RPC response (single round-trip)
//   GET  /mcp  → SSE stream for server→client notifications (not used in v0.1
//                but the endpoint is reserved so we don't break clients that
//                speculatively open it)
//
// Auth: Bearer token from the AGENT_OPS_TOKEN env var (or generated file).
//
// This server is intentionally hand-rolled — Anthropic's TypeScript MCP SDK
// has no Go counterpart yet, and the JSON-RPC wire format is small enough
// that depending on a half-baked third-party port would cost more than it
// saves.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ZibbyHQ/agent-ops/internal/scheduler"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/tool"
)

// Server is the MCP HTTP handler. Construct via New, mount on net/http.
type Server struct {
	scheduler *scheduler.Scheduler
	store     *state.Store
	tools     *tool.Registry
	token     string // bearer; empty → auth disabled (NOT recommended)
	log       *slog.Logger

	serverName    string
	serverVersion string
}

// Config bundles construction params.
type Config struct {
	Scheduler *scheduler.Scheduler
	Store     *state.Store
	Tools     *tool.Registry
	Token     string
	Logger    *slog.Logger

	ServerName    string
	ServerVersion string
}

// New builds an MCP Server.
func New(c Config) *Server {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	name := c.ServerName
	if name == "" {
		name = "agent-ops"
	}
	ver := c.ServerVersion
	if ver == "" {
		ver = "0.1.0"
	}
	return &Server{
		scheduler:     c.Scheduler,
		store:         c.Store,
		tools:         c.Tools,
		token:         c.Token,
		log:           logger,
		serverName:    name,
		serverVersion: ver,
	}
}

// Handler returns the http.Handler implementing the Streamable HTTP transport.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/healthz", s.handleHealth)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handlePOST(w, r)
	case http.MethodGet:
		s.handleGET(w, r)
	case http.MethodDelete:
		// MCP 1.x clients send DELETE to terminate a session; we don't keep
		// per-session state in v0.1 so respond 204 to satisfy the client.
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGET serves the SSE channel. v0.1 emits no server-initiated messages —
// we open the stream and keep it alive so MCP clients that probe this work.
func (s *Server) handleGET(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// Initial probe so clients know the channel is alive.
	_, _ = fmt.Fprint(w, ": ok\n\n")
	flusher.Flush()
	<-r.Context().Done()
}

func (s *Server) handlePOST(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		writeJSONRPCError(w, nil, -32001, "unauthorized")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	defer r.Body.Close()

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		writeJSONRPCError(w, req.ID, -32600, "invalid jsonrpc version")
		return
	}

	switch req.Method {
	case "initialize":
		s.respond(w, req.ID, s.initializeResult())
	case "initialized", "notifications/initialized":
		// Notification — no response.
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		s.respond(w, req.ID, s.toolsList())
	case "tools/call":
		s.handleToolsCall(w, r.Context(), req)
	case "ping":
		s.respond(w, req.ID, map[string]any{})
	default:
		writeJSONRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) authOK(r *http.Request) bool {
	if s.token == "" {
		// Disabled in dev — log loudly.
		s.log.Warn("mcp: no token configured, accepting unauthenticated request")
		return true
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	return strings.TrimPrefix(h, "Bearer ") == s.token
}

func (s *Server) initializeResult() map[string]any {
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    s.serverName,
			"version": s.serverVersion,
		},
	}
}

// ─── tools/list ─────────────────────────────────────────────────────────────

// toolsList enumerates the agent-ops control surface PLUS the daemon's
// underlying Tools (shell, http, …) so a remote agent can either trigger
// scheduled task runs or invoke tools directly.
func (s *Server) toolsList() map[string]any {
	out := []map[string]any{}

	for _, t := range builtinTools() {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": rawJSON(t.schema),
		})
	}

	// Expose each registered host tool, namespaced so we don't clash with
	// builtin agent_* tools. The local LLM driver sees these by their bare
	// names; remote callers see the prefixed name.
	for _, t := range s.tools.List() {
		out = append(out, map[string]any{
			"name":        "host_" + t.Name(),
			"description": t.Description(),
			"inputSchema": rawJSON(string(t.Schema())),
		})
	}

	return map[string]any{"tools": out}
}

// ─── tools/call ─────────────────────────────────────────────────────────────

func (s *Server) handleToolsCall(w http.ResponseWriter, ctx context.Context, req jsonRPCRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}

	// host_* → invoke a registered Tool directly.
	if strings.HasPrefix(params.Name, "host_") {
		bare := strings.TrimPrefix(params.Name, "host_")
		t, ok := s.tools.Get(bare)
		if !ok {
			writeJSONRPCError(w, req.ID, -32602, "no such host tool: "+bare)
			return
		}
		res, err := t.Invoke(ctx, params.Arguments)
		if err != nil {
			s.respond(w, req.ID, toolErrorResult(err.Error()))
			return
		}
		s.respond(w, req.ID, toolTextResult(res.Output))
		return
	}

	// builtin agent-ops tools
	switch params.Name {
	case "agent_status":
		s.respond(w, req.ID, s.toolStatus(ctx))
	case "agent_run_now":
		s.toolRunNow(w, ctx, req.ID, params.Arguments)
	case "agent_history":
		s.toolHistory(w, ctx, req.ID, params.Arguments)
	case "agent_logs":
		s.toolLogs(w, ctx, req.ID, params.Arguments)
	case "agent_list_tasks":
		s.toolListTasks(w, ctx, req.ID)
	case "agent_set_task":
		s.toolSetTask(w, ctx, req.ID, params.Arguments)
	case "agent_get_task":
		s.toolGetTask(w, ctx, req.ID, params.Arguments)
	case "agent_get_mission":
		s.toolGetMission(w, ctx, req.ID)
	case "agent_set_mission":
		s.toolSetMission(w, ctx, req.ID, params.Arguments)
	case "agent_remember_fact":
		s.toolRememberFact(w, ctx, req.ID, params.Arguments)
	default:
		writeJSONRPCError(w, req.ID, -32602, "no such tool: "+params.Name)
	}
}

// ─── builtin tool implementations ──────────────────────────────────────────

func (s *Server) toolStatus(ctx context.Context) map[string]any {
	runs, _ := s.store.ListRuns(ctx, "", 1)
	resp := map[string]any{
		"server":       s.serverName,
		"version":      s.serverVersion,
		"task_count":   len(s.scheduler.Entries()),
		"tool_count":   len(s.tools.List()),
	}
	if len(runs) > 0 {
		r := runs[0]
		resp["last_run"] = map[string]any{
			"id":         r.ID,
			"task_name":  r.TaskName,
			"trigger":    r.Trigger,
			"status":     r.Status,
			"started_at": r.StartedAt,
			"ended_at":   r.EndedAt,
			"summary":    r.Summary,
		}
	}
	return toolTextResult(mustEncodeJSON(resp))
}

func (s *Server) toolRunNow(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		TaskName       string `json:"task_name"`
		OverridePrompt string `json:"override_prompt"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if args.TaskName == "" {
		s.respond(w, id, toolErrorResult("task_name is required"))
		return
	}
	run, err := s.scheduler.RunNow(ctx, args.TaskName, args.OverridePrompt)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(run)))
}

func (s *Server) toolHistory(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		TaskName string `json:"task_name"`
		Limit    int    `json:"limit"`
	}
	_ = json.Unmarshal(raw, &args)
	runs, err := s.store.ListRuns(ctx, args.TaskName, args.Limit)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{
		"runs":  runs,
		"count": len(runs),
	})))
}

func (s *Server) toolLogs(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		RunID string `json:"run_id"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if args.RunID == "" {
		s.respond(w, id, toolErrorResult("run_id required"))
		return
	}
	logs, err := s.store.LogsForRun(ctx, args.RunID, args.Limit)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{
		"run_id": args.RunID,
		"logs":   logs,
	})))
}

func (s *Server) toolListTasks(w http.ResponseWriter, ctx context.Context, id any) {
	tasks, err := s.store.ListTasks(ctx)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{"tasks": tasks})))
}

func (s *Server) toolGetTask(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.Name == "" {
		writeJSONRPCError(w, id, -32602, "name required")
		return
	}
	t, err := s.store.GetTask(ctx, args.Name)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(t)))
}

// ─── Mission journal ───────────────────────────────────────────────────────

func (s *Server) toolGetMission(w http.ResponseWriter, ctx context.Context, id any) {
	m, err := s.store.GetMission(ctx)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(m)))
}

func (s *Server) toolSetMission(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		Statement string `json:"statement"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	// Empty statement is intentionally allowed — used to clear a mission.
	if err := s.store.SetStatement(ctx, args.Statement); err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult("ok"))
}

func (s *Server) toolRememberFact(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var args struct {
		Fact   string `json:"fact"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if args.Fact == "" {
		s.respond(w, id, toolErrorResult("fact is required"))
		return
	}
	if args.Source == "" {
		args.Source = "user" // MCP callers default to user-supplied
	}
	facts, err := s.store.AddFact(ctx, args.Source, args.Fact)
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{
		"added":       true,
		"total_facts": len(facts),
	})))
}

func (s *Server) toolSetTask(w http.ResponseWriter, ctx context.Context, id any, raw json.RawMessage) {
	var t state.Task
	if err := json.Unmarshal(raw, &t); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if t.Name == "" || t.Prompt == "" {
		s.respond(w, id, toolErrorResult("name and prompt required"))
		return
	}
	// Defaults: enabled=true unless caller explicitly sent enabled=false.
	if !t.Enabled && raw != nil {
		// Peek at the raw json to distinguish "absent" from "false".
		if !strings.Contains(string(raw), `"enabled"`) {
			t.Enabled = true
		}
	}
	if err := s.scheduler.SetTask(ctx, t); err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult("ok"))
}

// ─── JSON-RPC plumbing ─────────────────────────────────────────────────────

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Result  any            `json:"result,omitempty"`
	Error   *jsonRPCError  `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) respond(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeJSONRPCError(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: msg},
	})
}

// MCP "content" wrapper around a tool's result.
func toolTextResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	}
}

func toolErrorResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": true,
	}
}

func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }

func mustEncodeJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// builtin returns the static schema entries for the daemon's MCP tools.
// Centralized so tools/list and tests both reference one source of truth.
type builtin struct {
	name        string
	description string
	schema      string
}

func builtinTools() []builtin {
	return []builtin{
		{
			name:        "agent_status",
			description: "Show the agent-ops daemon's status: scheduled task count, host-tool count, last run summary.",
			schema:      `{"type":"object","properties":{}}`,
		},
		{
			name:        "agent_run_now",
			description: "Trigger an immediate run of a scheduled task. Optionally override the task's prompt for this run only.",
			schema:      `{"type":"object","properties":{"task_name":{"type":"string"},"override_prompt":{"type":"string"}},"required":["task_name"]}`,
		},
		{
			name:        "agent_history",
			description: "List recent task runs across all tasks (or filtered to one by task_name).",
			schema:      `{"type":"object","properties":{"task_name":{"type":"string"},"limit":{"type":"integer","default":20}}}`,
		},
		{
			name:        "agent_logs",
			description: "Fetch the per-line log of one task run (returned by agent_run_now or agent_history).",
			schema:      `{"type":"object","properties":{"run_id":{"type":"string"},"limit":{"type":"integer","default":500}},"required":["run_id"]}`,
		},
		{
			name:        "agent_list_tasks",
			description: "List every persisted task (schedule + prompt + tools + enabled flag).",
			schema:      `{"type":"object","properties":{}}`,
		},
		{
			name:        "agent_get_task",
			description: "Return the full config of one task by name.",
			schema:      `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`,
		},
		{
			name:        "agent_set_task",
			description: "Create or update a scheduled task. Supply name, cron (e.g. '0 9 * * 1'), prompt, optional tools allowlist, optional enabled flag.",
			schema:      `{"type":"object","properties":{"name":{"type":"string"},"cron":{"type":"string"},"prompt":{"type":"string"},"tools":{"type":"array","items":{"type":"string"}},"enabled":{"type":"boolean"}},"required":["name","cron","prompt"]}`,
		},
		// ── Mission journal ───────────────────────────────────────────────
		{
			name:        "agent_get_mission",
			description: "Return the instance's mission journal: the natural-language charter set by the user, plus the list of facts the agent has learned over time. This is what the agent reads on every task run to know who it is and what it's been doing.",
			schema:      `{"type":"object","properties":{}}`,
		},
		{
			name:        "agent_set_mission",
			description: "Replace the instance's mission statement (natural-language charter). Example: 'I steward the OpenDesign instance. Upgrades require dry-run; alert me at >80%% disk; never touch /etc/secrets.' Pass empty string to clear.",
			schema:      `{"type":"object","properties":{"statement":{"type":"string"}},"required":["statement"]}`,
		},
		{
			name:        "agent_remember_fact",
			description: "Append one fact to the mission journal. Use for important context the agent should carry across runs (versions installed, ports in use, decisions made). source defaults to 'user'.",
			schema:      `{"type":"object","properties":{"fact":{"type":"string"},"source":{"type":"string"}},"required":["fact"]}`,
		},
	}
}

// Token-acceptance helper so tests can override easily.
var _ = errors.New
