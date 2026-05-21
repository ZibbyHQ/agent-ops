// Copyright 2026 Zibby Lab. Apache-2.0.

// Package bootstrap performs first-run initialization for the daemon:
// generates/loads the MCP bearer token, persists it under <state>/mcp.token,
// and (when a bootstrap schedule is configured) runs it exactly once.
//
// "First run" is detected by the presence of <state>/bootstrap.done; this is
// a single file rather than a state.events query so a future Raft replay
// doesn't trigger a fresh bootstrap on every follower.
package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ZibbyHQ/agent-ops/internal/config"
	"github.com/ZibbyHQ/agent-ops/internal/scheduler"
	"github.com/ZibbyHQ/agent-ops/internal/state"
	"github.com/ZibbyHQ/agent-ops/internal/task"
)

// EnsureToken returns the daemon's MCP bearer token. Priority:
//  1. AGENT_OPS_TOKEN env var (the Zibby control plane sets this when
//     provisioning a sidecar — token is already known upstream)
//  2. <stateDir>/mcp.token file (preserved across restarts)
//  3. A fresh 32-byte random token, persisted to (2)
func EnsureToken(stateDir, envName string) (string, error) {
	if envName != "" {
		if v := os.Getenv(envName); v != "" {
			return v, nil
		}
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("bootstrap.EnsureToken: state dir: %w", err)
	}
	path := filepath.Join(stateDir, "mcp.token")
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) > 0 {
		return string(raw), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("bootstrap.EnsureToken: read: %w", err)
	}
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", fmt.Errorf("bootstrap.EnsureToken: persist: %w", err)
	}
	return tok, nil
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "ao_" + hex.EncodeToString(buf), nil
}

// MaybeRunFirstRun fires the configured Bootstrap schedule exactly once if
// <stateDir>/bootstrap.done is absent. Idempotent — subsequent restarts skip.
func MaybeRunFirstRun(
	ctx context.Context,
	cfg *config.Config,
	sched *scheduler.Scheduler,
	store *state.Store,
) error {
	if cfg.Bootstrap == nil {
		return nil
	}
	marker := filepath.Join(cfg.StateDir, "bootstrap.done")
	if _, err := os.Stat(marker); err == nil {
		return nil // already done
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("bootstrap.MaybeRunFirstRun: stat marker: %w", err)
	}

	// Persist the bootstrap as a normal task so MCP can re-run it ad-hoc.
	t := state.Task{
		Name:    cfg.Bootstrap.Name,
		Cron:    "@yearly", // never auto-fires; bootstrap is manual-only thereafter
		Prompt:  cfg.Bootstrap.Prompt,
		Tools:   cfg.Bootstrap.Tools,
		Enabled: false,
	}
	if err := store.UpsertTask(ctx, t); err != nil {
		return err
	}

	// Trigger one synchronous run. Pass through the scheduler so the same
	// path is used as RunNow + tests.
	_, err := sched.RunNow(ctx, t.Name, t.Prompt)
	if err != nil {
		// Don't write the marker on failure — operator may want to retry
		// once they fix what was broken.
		return fmt.Errorf("bootstrap: first-run task failed: %w", err)
	}
	return os.WriteFile(marker, []byte("ok"), 0o600)
	// the marker has no business value beyond "we've been here" — its
	// presence alone keeps subsequent restarts idempotent.
}

// We use task.TriggerBootstrap directly nowhere in this file (RunNow tags it
// as TriggerManual), but the constant is re-exported here so future cluster
// code can label cluster-induced bootstraps separately.
var _ = task.TriggerBootstrap
