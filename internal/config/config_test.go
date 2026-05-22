package config

import (
	"strings"
	"testing"
)

const validYAML = `
state_dir: /tmp/ao
agent:
  provider: claude
  model: claude-sonnet-4-6
  api_key_env: ANTHROPIC_API_KEY
schedules:
  - name: weekly_upgrade
    cron: "0 9 * * 1"
    prompt: "Check for upstream updates"
    tools: [shell, http]
mcp:
  listen_addr: ":7842"
  token_env: AGENT_OPS_TOKEN
`

func TestParse_HappyPath(t *testing.T) {
	c, err := Parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.StateDir != "/tmp/ao" {
		t.Errorf("StateDir = %q", c.StateDir)
	}
	if c.Agent.Provider != "claude" {
		t.Errorf("Agent.Provider = %q", c.Agent.Provider)
	}
	if c.Agent.MaxToolCallsPerTask != 25 {
		t.Errorf("default max_tool_calls_per_task not applied: %d", c.Agent.MaxToolCallsPerTask)
	}
	if len(c.Schedules) != 1 || c.Schedules[0].Name != "weekly_upgrade" {
		t.Errorf("schedule parse mismatch: %+v", c.Schedules)
	}
}

func TestParse_RejectsBadProvider(t *testing.T) {
	bad := strings.Replace(validYAML, "provider: claude", "provider: openai", 1)
	if _, err := Parse(strings.NewReader(bad)); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestParse_RejectsBadCron(t *testing.T) {
	bad := strings.Replace(validYAML, "0 9 * * 1", "not a cron", 1)
	if _, err := Parse(strings.NewReader(bad)); err == nil {
		t.Fatal("expected error for malformed cron")
	}
}

func TestParse_RejectsDuplicateScheduleNames(t *testing.T) {
	yaml := `
agent:
  provider: claude
  model: c
  api_key_env: K
schedules:
  - name: dup
    cron: "@hourly"
    prompt: "a"
  - name: dup
    cron: "@daily"
    prompt: "b"
`
	if _, err := Parse(strings.NewReader(yaml)); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestSchedulesEnabled_DefaultsToEnabled(t *testing.T) {
	c, _ := Parse(strings.NewReader(validYAML))
	if len(c.SchedulesEnabled()) != 1 {
		t.Fatalf("expected 1 enabled schedule, got %d", len(c.SchedulesEnabled()))
	}
}

func TestSchedulesEnabled_ExplicitFalse(t *testing.T) {
	yaml := `
agent:
  provider: claude
  model: c
  api_key_env: K
schedules:
  - name: on
    cron: "@hourly"
    prompt: "a"
  - name: off
    cron: "@hourly"
    prompt: "b"
    enabled: false
`
	c, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SchedulesEnabled()) != 1 {
		t.Fatalf("expected 1 enabled schedule, got %d", len(c.SchedulesEnabled()))
	}
}

func TestParse_BootstrapPromptEnvOverride(t *testing.T) {
	// AGENT_OPS_BOOTSTRAP_PROMPT is how the Zibby control plane injects a
	// per-instance goal into a Fargate task without baking a new config.yaml
	// per task. Two paths: synthesize a Bootstrap when config has none, and
	// override an existing Bootstrap.Prompt when it does.
	t.Setenv("AGENT_OPS_BOOTSTRAP_PROMPT", "install n8n on port 5678")

	t.Run("synthesizes Bootstrap when config has none", func(t *testing.T) {
		c, err := Parse(strings.NewReader(validYAML))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if c.Bootstrap == nil {
			t.Fatal("Bootstrap should be synthesized from env var")
		}
		if c.Bootstrap.Prompt != "install n8n on port 5678" {
			t.Errorf("Bootstrap.Prompt = %q", c.Bootstrap.Prompt)
		}
		if c.Bootstrap.Name != "bootstrap" {
			t.Errorf("Bootstrap.Name = %q, want default 'bootstrap'", c.Bootstrap.Name)
		}
	})

	t.Run("overrides existing Bootstrap.Prompt", func(t *testing.T) {
		yaml := validYAML + `
bootstrap:
  name: initial_setup
  prompt: "this should get overridden"
  tools: [shell]
`
		c, err := Parse(strings.NewReader(yaml))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if c.Bootstrap.Prompt != "install n8n on port 5678" {
			t.Errorf("env should win, got %q", c.Bootstrap.Prompt)
		}
		if c.Bootstrap.Name != "initial_setup" {
			t.Errorf("existing Name should be preserved, got %q", c.Bootstrap.Name)
		}
	})
}

func TestParse_AllowsReservedTopLevelKeys(t *testing.T) {
	// Future v0.x features should not break v0.1 config files.
	yaml := validYAML + `
cluster:
  raft_peers: ["a", "b"]
telemetry:
  otel_endpoint: "http://collector:4317"
connectors:
  zibby:
    url: "https://api.zibby.app"
`
	c, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("reserved keys should not fail: %v", err)
	}
	if len(c.Cluster) == 0 {
		t.Error("expected reserved Cluster section to parse")
	}
}
