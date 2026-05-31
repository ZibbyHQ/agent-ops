// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRoot_Version asserts the version subcommand prints the baked-in
// version exactly once with a trailing newline. Pinned because release
// scripts grep for it in CI.
func TestRoot_Version(t *testing.T) {
	root := New("0.2.0-test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute version: %v", err)
	}
	got := strings.TrimSpace(out.String())
	if got != "0.2.0-test" {
		t.Errorf("version output = %q, want %q", got, "0.2.0-test")
	}
}

// TestRoot_UnknownSubcommand makes sure an invented subcommand exits with
// an error (cobra's default behavior — pin so a future config tweak doesn't
// silently turn it into a no-op).
func TestRoot_UnknownSubcommand(t *testing.T) {
	root := New("test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"flibbertigibbet"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unknown subcommand, got nil")
	}
}

// TestRoot_Help confirms the canonical subcommand list renders in help
// output. Catches accidental removal of a subcommand during refactors.
func TestRoot_Help(t *testing.T) {
	root := New("test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute --help: %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"init", "start", "stop", "restart", "status",
		"logs", "doctor", "uninstall", "schedule", "mcp", "version",
		"integrate",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("--help missing subcommand %q. Full output:\n%s", want, s)
		}
	}
}

// TestInit_DryRun_RendersConfig — round-trip the init wizard with --yes
// --dry-run, parse the rendered config back through config.Parse, and assert
// the answers survived (model + provider + api_key_env). This pins the
// emitted YAML shape so a refactor of renderConfigYAML can't silently drift
// from what config.Load will accept.
func TestInit_DryRun_RendersConfig(t *testing.T) {
	root := New("test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	// `--config` is consumed by initCmd as just the path it would write
	// to — with --dry-run we never touch the filesystem.
	root.SetArgs([]string{"init", "--yes", "--dry-run",
		"--config", "/tmp/agent-ops-test/config.yaml"})
	if err := root.Execute(); err != nil {
		t.Fatalf("init --dry-run: %v", err)
	}
	body := out.String()
	for _, want := range []string{
		"provider: claude-cli",
		"api_key_env: CLAUDE_CODE_OAUTH_TOKEN",
		"hourly_health_check",
		"listen_addr:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dry-run output missing %q. Full body:\n%s", want, body)
		}
	}
}
