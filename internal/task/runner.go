// Copyright 2026 Zibby Lab. Apache-2.0.

// Package task is the bridge between a Schedule (or ad-hoc invocation) and
// the LLM Driver. It owns:
//   - assembling the system+user prompt
//   - cutting a TaskRun row in state
//   - calling the Driver
//   - persisting the final status + logs
//
// Runner.Run is goroutine-safe; concurrent runs of different tasks may proceed
// in parallel, but per-task serialization (one run at a time per Task) is
// enforced by the scheduler, not here.
package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ZibbyHQ/agent-ops/internal/driver"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/tool"
)

// Runner ties together driver, tool registry, and state store.
type Runner struct {
	Driver       driver.Driver
	Tools        *tool.Registry
	State        *state.Store
	MaxToolCalls int
	TaskTimeout  time.Duration
	SystemPrompt string

	// In-flight protects against duplicate concurrent runs of the same task
	// (e.g. a slow run still going when the next cron tick fires). v0.1
	// policy: skip the new tick + log "skipped".
	mu       sync.Mutex
	inflight map[string]struct{}
}

// NewRunner returns a Runner with sensible defaults.
func NewRunner(d driver.Driver, tools *tool.Registry, st *state.Store) *Runner {
	return &Runner{
		Driver:       d,
		Tools:        tools,
		State:        st,
		MaxToolCalls: 25,
		TaskTimeout:  10 * time.Minute,
		SystemPrompt: defaultSystemPrompt,
		inflight:     map[string]struct{}{},
	}
}

const defaultSystemPrompt = `You are agent-ops, an autonomous DevOps operator running on a single
host. You receive instructions and have a small set of tools (shell, etc.)
to satisfy them. Be concise. Prefer read-only inspection before mutating.
When a tool fails, recover or report — do not retry blindly. End the run
when the user instruction is satisfied OR when you genuinely cannot make
further progress; explain either outcome in plain English.`

// Trigger is the entry point — schedule | manual | bootstrap. Always returns
// the run id (even on failure) so callers can subscribe to logs.
type Trigger string

const (
	TriggerSchedule  Trigger = "schedule"
	TriggerManual    Trigger = "manual"
	TriggerBootstrap Trigger = "bootstrap"
)

// Spec describes one task invocation.
type Spec struct {
	Name    string   // Task name; "ad-hoc" if not from a Schedule
	Trigger Trigger  // why we're running
	Prompt  string   // user prompt
	Tools   []string // tool allowlist; nil/empty → all
}

// Run executes one task. Returns the persisted TaskRun (terminal state) and
// the driver's Result for callers that want the assistant's final message
// inline (e.g. MCP synchronous run_now).
func (r *Runner) Run(ctx context.Context, spec Spec) (state.TaskRun, driver.Result, error) {
	if spec.Name == "" {
		return state.TaskRun{}, driver.Result{}, errors.New("task.Run: spec.Name required")
	}
	if spec.Prompt == "" {
		return state.TaskRun{}, driver.Result{}, errors.New("task.Run: spec.Prompt required")
	}

	// Single-flight per task: skip if a run is already in-flight.
	if !r.markInflight(spec.Name) {
		return state.TaskRun{}, driver.Result{}, fmt.Errorf("task.Run: %q is already running, skipping", spec.Name)
	}
	defer r.releaseInflight(spec.Name)

	runID, err := generateRunID()
	if err != nil {
		return state.TaskRun{}, driver.Result{}, err
	}
	run := state.TaskRun{
		ID:        runID,
		TaskName:  spec.Name,
		Trigger:   string(spec.Trigger),
		StartedAt: time.Now().UTC(),
	}
	if err := r.State.StartRun(ctx, run); err != nil {
		return state.TaskRun{}, driver.Result{}, fmt.Errorf("task.Run: start: %w", err)
	}

	cctx, cancel := context.WithTimeout(ctx, r.TaskTimeout)
	defer cancel()

	sink := &runLogSink{state: r.State, runID: runID}

	dReq := driver.Request{
		SystemPrompt: r.SystemPrompt,
		UserPrompt:   spec.Prompt,
		Tools:        r.Tools.Subset(spec.Tools),
		MaxToolCalls: r.MaxToolCalls,
		LogSink:      sink,
	}

	dRes, dErr := r.Driver.Run(cctx, dReq)

	finalStatus := state.StatusCompleted
	summary := truncate(dRes.FinalMessage, 2000)
	errMsg := ""
	// Order matters: context-derived states beat driver errors, because a
	// driver call that returned `ctx.Err()` is really a timeout/cancel, not
	// a bug. Cancellation likewise outranks "ran-but-failed".
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		finalStatus = state.StatusTimeout
		errMsg = "task exceeded configured timeout"
	case ctx.Err() == context.Canceled || cctx.Err() == context.Canceled:
		finalStatus = state.StatusCanceled
		errMsg = "task canceled"
	case dErr != nil:
		finalStatus = state.StatusFailed
		errMsg = dErr.Error()
	case dRes.Error != "":
		// Driver itself reported a partial-failure (e.g. max_tool_calls).
		finalStatus = state.StatusFailed
		errMsg = dRes.Error
	}

	// Finish in a fresh context — the parent may be canceled, but we still
	// want to persist the terminal status.
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	if err := r.State.FinishRun(finishCtx, runID, finalStatus, summary, errMsg, dRes.ToolCalls); err != nil {
		return run, dRes, fmt.Errorf("task.Run: finish: %w", err)
	}

	out, _ := r.State.GetRun(finishCtx, runID)
	return out, dRes, nil
}

func (r *Runner) markInflight(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.inflight[name]; busy {
		return false
	}
	r.inflight[name] = struct{}{}
	return true
}

func (r *Runner) releaseInflight(name string) {
	r.mu.Lock()
	delete(r.inflight, name)
	r.mu.Unlock()
}

// runLogSink writes one log row per agent-loop event for later replay.
type runLogSink struct {
	state *state.Store
	runID string

	mu  sync.Mutex
	seq int
}

func (s *runLogSink) Log(ctx context.Context, level, message string) error {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	return s.state.AppendRunLog(ctx, s.runID, seq, level, message)
}

func generateRunID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "run-" + hex.EncodeToString(buf), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
