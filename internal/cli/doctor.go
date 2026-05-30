// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZibbyHQ/agent-ops/internal/config"
)

// newDoctorCmd runs a battery of "is this host ready?" checks. Each check
// outputs one line ("ok" / "warn" / "fail"); we exit non-zero only when at
// least one fail is found, so CI can wire this as a smoke test.
//
// Designed to be safe to run without sudo: every check is read-only or
// best-effort.
func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Self-check: config, provider binary, state dir, network.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			fail := runDoctor(cmd.Context(), cmd.OutOrStdout(), cfgPath)
			if fail > 0 {
				return fmt.Errorf("doctor: %d check(s) failed", fail)
			}
			return nil
		},
	}
}

// runDoctor writes one human-readable line per check + returns the number of
// failures. Exposed (lowercase) so future tests can drive it without going
// through cobra.
func runDoctor(_ context.Context, out io.Writer, cfgPath string) int {
	fails := 0

	// Config readable?
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(out, "[fail] config %s: %v\n", cfgPath, err)
		fails++
		// Still try the rest of the checks with a zero-value cfg so the
		// operator gets a full picture.
		cfg = &config.Config{}
	} else {
		fmt.Fprintf(out, "[ok]   config parsed: provider=%s model=%s\n",
			cfg.Agent.Provider, cfg.Agent.Model)
	}

	// API key env set?
	keyEnv := cfg.Agent.APIKeyEnv
	if keyEnv == "" {
		fmt.Fprintf(out, "[warn] agent.api_key_env is empty (claude-cli / ollama path)\n")
	} else if v := os.Getenv(keyEnv); v == "" {
		fmt.Fprintf(out, "[warn] env %s is unset — daemon will refuse to start until it's exported\n", keyEnv)
	} else {
		fmt.Fprintf(out, "[ok]   env %s is set (%d chars)\n", keyEnv, len(v))
	}

	// Provider CLI binary on PATH?
	switch cfg.Agent.Provider {
	case "claude-cli":
		checkBinary(out, "claude", &fails)
	case "codex":
		checkBinary(out, "codex", &fails)
	}

	// State dir writable?
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	if err := checkWritable(stateDir); err != nil {
		fmt.Fprintf(out, "[fail] state dir %s: %v\n", stateDir, err)
		fails++
	} else {
		fmt.Fprintf(out, "[ok]   state dir %s is writable\n", stateDir)
	}

	// MCP listen addr — does anything already own it?
	addr := cfg.MCP.ListenAddr
	if addr == "" {
		addr = ":7842"
	}
	if err := checkPortFree(addr); err != nil {
		fmt.Fprintf(out, "[warn] MCP addr %s appears busy: %v\n", addr, err)
	} else {
		fmt.Fprintf(out, "[ok]   MCP addr %s is free\n", addr)
	}

	// Network — light TCP dial to anthropic.com:443. 3s budget.
	if err := checkNetwork("api.anthropic.com:443"); err != nil {
		fmt.Fprintf(out, "[warn] anthropic.com unreachable: %v\n", err)
	} else {
		fmt.Fprintf(out, "[ok]   anthropic.com:443 reachable\n")
	}

	return fails
}

func checkBinary(out io.Writer, name string, fails *int) {
	if p, err := exec.LookPath(name); err == nil {
		fmt.Fprintf(out, "[ok]   %s on PATH (%s)\n", name, p)
		return
	}
	fmt.Fprintf(out, "[fail] %s NOT on PATH — install the provider CLI first\n", name)
	*fails++
}

func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return err
	}
	return os.Remove(probe)
}

func checkPortFree(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	_ = l.Close()
	return nil
}

func checkNetwork(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
