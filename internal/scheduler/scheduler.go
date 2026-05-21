// Copyright 2026 Zibby Lab. Apache-2.0.

// Package scheduler is the cron-driven dispatcher.
//
// On boot:
//   - hydrate Task rows from State (so MCP-side edits survive restart)
//   - merge with config's `schedules` (config wins if both define a task)
//   - register a robfig/cron entry per enabled Task that calls Runner.Run
//
// The scheduler is intentionally minimal: every tick spawns a goroutine that
// calls Runner.Run, which itself handles single-flight + persistence. The
// scheduler doesn't track in-flight state.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/robfig/cron/v3"

	"github.com/ZibbyHQ/agent-ops/internal/config"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/task"
)

// Scheduler wraps robfig/cron with daemon-specific glue.
type Scheduler struct {
	cron   *cron.Cron
	runner *task.Runner
	store  *state.Store
	log    *slog.Logger

	mu       sync.Mutex
	entries  map[string]cron.EntryID // task name → entry id
}

// New returns a Scheduler. The caller must call Start before Add* takes effect.
func New(runner *task.Runner, store *state.Store, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		cron: cron.New(cron.WithParser(cron.NewParser(
			cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
		))),
		runner:  runner,
		store:   store,
		log:     logger,
		entries: map[string]cron.EntryID{},
	}
}

// Start begins firing scheduled ticks.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop blocks until all running ticks have finished. Use during shutdown.
func (s *Scheduler) Stop(ctx context.Context) error {
	stopCtx := s.cron.Stop()
	select {
	case <-stopCtx.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Hydrate loads tasks from the State store + merges with config schedules,
// then registers each enabled task. Config wins on conflict because the
// operator's source-controlled config is more authoritative than DB state
// after a manual MCP edit (which the user can re-apply).
func (s *Scheduler) Hydrate(ctx context.Context, cfg *config.Config) error {
	stored, err := s.store.ListTasks(ctx)
	if err != nil {
		return fmt.Errorf("scheduler.Hydrate: list: %w", err)
	}
	byName := make(map[string]state.Task, len(stored))
	for _, t := range stored {
		byName[t.Name] = t
	}
	// Config-defined tasks overwrite any prior persisted version.
	for _, sched := range cfg.Schedules {
		enabled := sched.Enabled == nil || *sched.Enabled
		t := state.Task{
			Name:    sched.Name,
			Cron:    sched.Cron,
			Prompt:  sched.Prompt,
			Tools:   sched.Tools,
			Enabled: enabled,
		}
		if err := s.store.UpsertTask(ctx, t); err != nil {
			return fmt.Errorf("scheduler.Hydrate: upsert %q: %w", sched.Name, err)
		}
		byName[sched.Name] = t
	}

	for _, t := range byName {
		if !t.Enabled {
			continue
		}
		if err := s.register(t); err != nil {
			return err
		}
	}
	return nil
}

// register adds (or replaces) a cron entry for the given Task.
func (s *Scheduler) register(t state.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.entries[t.Name]; ok {
		s.cron.Remove(old)
		delete(s.entries, t.Name)
	}
	job := s.makeJob(t)
	id, err := s.cron.AddFunc(t.Cron, job)
	if err != nil {
		return fmt.Errorf("scheduler.register: %q (cron=%q): %w", t.Name, t.Cron, err)
	}
	s.entries[t.Name] = id
	s.log.Info("scheduler: task registered", "name", t.Name, "cron", t.Cron)
	return nil
}

func (s *Scheduler) makeJob(t state.Task) func() {
	return func() {
		// Re-read the task fresh in case an MCP update happened between
		// registration and tick — the cron entry points at this closure
		// but the prompt/tools may have moved.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cur, err := s.store.GetTask(ctx, t.Name)
		if err != nil {
			s.log.Error("scheduler: get task", "name", t.Name, "error", err)
			return
		}
		if !cur.Enabled {
			s.log.Info("scheduler: task disabled, skipping tick", "name", cur.Name)
			return
		}

		_, _, runErr := s.runner.Run(ctx, task.Spec{
			Name:    cur.Name,
			Trigger: task.TriggerSchedule,
			Prompt:  cur.Prompt,
			Tools:   cur.Tools,
		})
		if runErr != nil {
			s.log.Error("scheduler: run failed", "name", cur.Name, "error", runErr)
		}
	}
}

// SetTask atomically upserts + re-registers (or removes) a task. Called by the
// MCP layer when a user updates a schedule.
func (s *Scheduler) SetTask(ctx context.Context, t state.Task) error {
	if t.Name == "" {
		return errors.New("scheduler.SetTask: Name required")
	}
	if t.Enabled && t.Cron == "" {
		return errors.New("scheduler.SetTask: enabled task requires Cron")
	}
	if err := s.store.UpsertTask(ctx, t); err != nil {
		return err
	}
	if !t.Enabled {
		s.mu.Lock()
		if id, ok := s.entries[t.Name]; ok {
			s.cron.Remove(id)
			delete(s.entries, t.Name)
		}
		s.mu.Unlock()
		return nil
	}
	return s.register(t)
}

// RunNow triggers an immediate ad-hoc run of the named task. Returns the run
// id once the runner has persisted it. Synchronous — blocks until completion.
func (s *Scheduler) RunNow(ctx context.Context, taskName, overridePrompt string) (state.TaskRun, error) {
	t, err := s.store.GetTask(ctx, taskName)
	if err != nil {
		return state.TaskRun{}, fmt.Errorf("scheduler.RunNow: get task %q: %w", taskName, err)
	}
	prompt := t.Prompt
	if overridePrompt != "" {
		prompt = overridePrompt
	}
	run, _, err := s.runner.Run(ctx, task.Spec{
		Name:    t.Name,
		Trigger: task.TriggerManual,
		Prompt:  prompt,
		Tools:   t.Tools,
	})
	return run, err
}

// Entries returns the currently-registered task names (for debugging / MCP).
func (s *Scheduler) Entries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for name := range s.entries {
		out = append(out, name)
	}
	return out
}
