// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimal valid baked config so the CLI add/list/remove path can parse +
// rewrite. Mirrors integrate_test.baseConfig but duplicated here so the
// CLI tests don't depend on internal package internals.
const cfgFixture = `state_dir: /tmp/ao
agent:
  provider: claude
  model: claude-sonnet-4-6
  api_key_env: ANTHROPIC_API_KEY
schedules:
  - name: hourly_health_check
    cron: "@hourly"
    prompt: check
    tools: [shell]
mcp:
  listen_addr: ":7842"
  token_env: AGENT_OPS_TOKEN
`

func newCLIWithTempPaths(t *testing.T) (rootArgs []string, cfgPath, envPath string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.yaml")
	envPath = filepath.Join(dir, "agent-ops.env")
	if err := os.WriteFile(cfgPath, []byte(cfgFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{"--config", cfgPath}, cfgPath, envPath
}

// TestIntegrate_AddListRemove_RoundTrip exercises the full happy-path:
// add → list (sees the new integration) → remove → list (empty). Pins
// the JSON output shape that the MCP-tool equivalent also emits.
func TestIntegrate_AddListRemove_RoundTrip(t *testing.T) {
	root := New("test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)

	prefix, cfgPath, envPath := newCLIWithTempPaths(t)

	// --- add
	out.Reset()
	root.SetArgs(append(prefix, "integrate", "add",
		"--name", "zibby",
		"--transport", "http",
		"--url", "https://api/mcp",
		"--auth-env", "ZBY",
		"--token", "secret",
		"--kv", "AGENT_OPS_NOTIFY_WORKFLOW_ID=wf_x",
		"--env-file", envPath,
	))
	if err := root.Execute(); err != nil {
		t.Fatalf("add: %v\noutput:\n%s", err, out.String())
	}
	var addRes struct {
		Name            string `json:"name"`
		RestartRequired bool   `json:"restart_required"`
	}
	if err := json.Unmarshal(out.Bytes(), &addRes); err != nil {
		t.Fatalf("add stdout not JSON: %v\n%s", err, out.String())
	}
	if addRes.Name != "zibby" || !addRes.RestartRequired {
		t.Errorf("unexpected add result: %+v", addRes)
	}

	// Sanity-check the env file landed at the override path with the
	// bearer + extra-env line.
	envBody, _ := os.ReadFile(envPath)
	if !strings.Contains(string(envBody), "ZBY=secret") ||
		!strings.Contains(string(envBody), "AGENT_OPS_NOTIFY_WORKFLOW_ID=wf_x") {
		t.Errorf("env file content unexpected: %q", envBody)
	}

	// --- list
	out.Reset()
	root.SetArgs(append(prefix, "integrate", "list", "--json"))
	if err := root.Execute(); err != nil {
		t.Fatalf("list: %v\n%s", err, out.String())
	}
	var listRes []struct {
		Name      string `json:"name"`
		Transport string `json:"transport"`
	}
	if err := json.Unmarshal(out.Bytes(), &listRes); err != nil {
		t.Fatalf("list stdout not JSON: %v\n%s", err, out.String())
	}
	if len(listRes) != 1 || listRes[0].Name != "zibby" {
		t.Errorf("unexpected list: %+v", listRes)
	}

	// --- remove
	out.Reset()
	root.SetArgs(append(prefix, "integrate", "remove",
		"--name", "zibby", "--env-file", envPath))
	if err := root.Execute(); err != nil {
		t.Fatalf("remove: %v\n%s", err, out.String())
	}

	// After removal, list returns []
	out.Reset()
	root.SetArgs(append(prefix, "integrate", "list", "--json"))
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	// json.Unmarshal of "null" or "[]" both result in nil/empty — accept either.
	var after []struct{}
	_ = json.Unmarshal(out.Bytes(), &after)
	if len(after) != 0 {
		t.Errorf("expected empty list after remove, got %d", len(after))
	}

	// Final check: config.yaml is still parseable & the integration is gone.
	body, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(body), "name: zibby") {
		t.Errorf("config still has zibby integration after remove:\n%s", body)
	}
}

// TestIntegrate_Add_RejectsMissingFlags pins the flag-validation surface
// — `--name` is the only "required" flag in cobra terms; transport
// validation lives in integrate.Add.
func TestIntegrate_Add_RejectsMissingFlags(t *testing.T) {
	root := New("test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"integrate", "add"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected required-flag error")
	}
}

// TestIntegrate_Add_HelpListsAllFlags is a defensive check — refactors
// that drop a flag should surface in --help output.
func TestIntegrate_Add_HelpListsAllFlags(t *testing.T) {
	root := New("test")
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"integrate", "add", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--name", "--transport", "--url", "--auth-env", "--token", "--kv", "--stdio-env"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--help missing flag %q. Full output:\n%s", want, out.String())
		}
	}
}

// TestParseKVPairs covers the helper directly — easier to assert error
// shape than via the cobra round-trip.
func TestParseKVPairs(t *testing.T) {
	m, err := parseKVPairs([]string{"A=1", "B=two words=here"})
	if err != nil {
		t.Fatal(err)
	}
	if m["A"] != "1" || m["B"] != "two words=here" {
		t.Errorf("parse: %+v", m)
	}
	if _, err := parseKVPairs([]string{"NOEQUALS"}); err == nil {
		t.Error("expected error for malformed entry")
	}
}
