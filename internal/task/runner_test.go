package task

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZibbyHQ/agent-ops/internal/driver"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/tool"
)

// fakeDriver runs a programmable callback per invocation.
type fakeDriver struct {
	name string
	run  func(ctx context.Context, req driver.Request) (driver.Result, error)
}

func (d *fakeDriver) Name() string { return d.name }
func (d *fakeDriver) Run(ctx context.Context, req driver.Request) (driver.Result, error) {
	return d.run(ctx, req)
}

func openState(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRunner_HappyPath(t *testing.T) {
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(ctx context.Context, req driver.Request) (driver.Result, error) {
			// Validate that runner threaded args through.
			if req.UserPrompt == "" {
				t.Error("UserPrompt empty")
			}
			if req.MaxToolCalls == 0 {
				t.Error("MaxToolCalls 0")
			}
			if req.SystemPrompt == "" {
				t.Error("SystemPrompt empty")
			}
			// Emit a couple of log lines via the sink to test persistence.
			if req.LogSink != nil {
				_ = req.LogSink.Log(ctx, "info", "first")
				_ = req.LogSink.Log(ctx, "info", "second")
			}
			return driver.Result{FinalMessage: "all done", ToolCalls: 2}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)

	run, res, err := r.Run(context.Background(), Spec{
		Name:    "task-a",
		Trigger: TriggerManual,
		Prompt:  "do thing",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != state.StatusCompleted {
		t.Fatalf("status = %q, want completed", run.Status)
	}
	if !strings.Contains(run.Summary, "all done") {
		t.Fatalf("summary = %q", run.Summary)
	}
	if run.ToolCalls != 2 {
		t.Fatalf("ToolCalls = %d", run.ToolCalls)
	}
	if res.FinalMessage != "all done" {
		t.Fatalf("driver result not passed through: %+v", res)
	}
	logs, _ := st.LogsForRun(context.Background(), run.ID, 10)
	if len(logs) != 2 {
		t.Fatalf("expected 2 log rows, got %d", len(logs))
	}
}

func TestRunner_DriverError_MarkedFailed(t *testing.T) {
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(context.Context, driver.Request) (driver.Result, error) {
			return driver.Result{}, errors.New("boom")
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)

	run, _, err := r.Run(context.Background(), Spec{
		Name:    "task-fail",
		Trigger: TriggerManual,
		Prompt:  "doomed",
	})
	if err != nil {
		t.Fatalf("Run unexpectedly errored: %v", err)
	}
	if run.Status != state.StatusFailed {
		t.Fatalf("status = %q, want failed", run.Status)
	}
	if !strings.Contains(run.Error, "boom") {
		t.Fatalf("error not surfaced: %q", run.Error)
	}
}

func TestRunner_DriverInternalError_MarkedFailed(t *testing.T) {
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(context.Context, driver.Request) (driver.Result, error) {
			return driver.Result{Error: "max_tool_calls reached"}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)
	run, _, _ := r.Run(context.Background(), Spec{
		Name: "task-overrun", Trigger: TriggerManual, Prompt: "hi",
	})
	if run.Status != state.StatusFailed {
		t.Fatalf("status = %q, want failed", run.Status)
	}
}

func TestRunner_Timeout_MarkedTimeout(t *testing.T) {
	st := openState(t)
	d := &fakeDriver{
		name: "fake",
		run: func(ctx context.Context, req driver.Request) (driver.Result, error) {
			<-ctx.Done() // wait for runner's timeout
			return driver.Result{}, ctx.Err()
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)
	r.TaskTimeout = 50 * time.Millisecond
	run, _, _ := r.Run(context.Background(), Spec{
		Name: "task-slow", Trigger: TriggerManual, Prompt: "wait",
	})
	if run.Status != state.StatusTimeout {
		t.Fatalf("status = %q, want timeout", run.Status)
	}
}

func TestRunner_RefusesConcurrentSameTask(t *testing.T) {
	st := openState(t)
	started := make(chan struct{})
	finish := make(chan struct{})
	d := &fakeDriver{
		name: "fake",
		run: func(ctx context.Context, req driver.Request) (driver.Result, error) {
			close(started)
			<-finish
			return driver.Result{FinalMessage: "ok"}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = r.Run(context.Background(), Spec{Name: "same", Trigger: TriggerManual, Prompt: "p"})
	}()
	<-started

	_, _, err := r.Run(context.Background(), Spec{Name: "same", Trigger: TriggerManual, Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("expected already-running error, got: %v", err)
	}

	close(finish)
	wg.Wait()
}

func TestRunner_MissionPrependedToSystemPrompt(t *testing.T) {
	ctx := context.Background()
	st := openState(t)

	// Seed a mission so the runner has something to inject.
	if err := st.SetStatement(ctx, "I steward the OpenDesign instance. Always dry-run upgrades."); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddFact(ctx, "bootstrap", "OpenDesign installed via apt at /usr/local/bin/opendesign"); err != nil {
		t.Fatal(err)
	}

	var capturedSystem string
	d := &fakeDriver{
		name: "fake",
		run: func(_ context.Context, req driver.Request) (driver.Result, error) {
			capturedSystem = req.SystemPrompt
			return driver.Result{FinalMessage: "ok"}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)
	_, _, err := r.Run(ctx, Spec{Name: "weekly", Trigger: TriggerManual, Prompt: "check upstream"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capturedSystem, "I steward the OpenDesign instance") {
		t.Fatalf("system prompt missing mission statement:\n%s", capturedSystem)
	}
	if !strings.Contains(capturedSystem, "OpenDesign installed via apt") {
		t.Fatalf("system prompt missing facts:\n%s", capturedSystem)
	}
	if !strings.Contains(capturedSystem, "== MISSION ==") {
		t.Fatalf("system prompt missing MISSION header:\n%s", capturedSystem)
	}
	if !strings.Contains(capturedSystem, "== BASE GUIDANCE ==") {
		t.Fatalf("system prompt missing BASE GUIDANCE separator:\n%s", capturedSystem)
	}
}

func TestRunner_NoMission_UsesBaseSystemPromptOnly(t *testing.T) {
	ctx := context.Background()
	st := openState(t)

	var capturedSystem string
	d := &fakeDriver{
		name: "fake",
		run: func(_ context.Context, req driver.Request) (driver.Result, error) {
			capturedSystem = req.SystemPrompt
			return driver.Result{FinalMessage: "ok"}, nil
		},
	}
	r := NewRunner(d, tool.NewRegistry(), st)
	_, _, err := r.Run(ctx, Spec{Name: "t", Trigger: TriggerManual, Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(capturedSystem, "== MISSION ==") {
		t.Fatalf("MISSION header should not appear when no mission is set:\n%s", capturedSystem)
	}
	if !strings.Contains(capturedSystem, defaultSystemPrompt) {
		t.Fatal("base system prompt should still be present when no mission set")
	}
}

func TestRunner_RejectsEmptyName(t *testing.T) {
	st := openState(t)
	r := NewRunner(&fakeDriver{name: "fake", run: func(context.Context, driver.Request) (driver.Result, error) {
		return driver.Result{}, nil
	}}, tool.NewRegistry(), st)
	if _, _, err := r.Run(context.Background(), Spec{Name: "", Prompt: "x"}); err == nil {
		t.Fatal("expected error for empty name")
	}
}
