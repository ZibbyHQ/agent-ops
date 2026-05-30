// Copyright 2026 Zibby Lab. Apache-2.0.

package service

import (
	"strings"
	"testing"
)

// TestSystemdRender_PinsExecStart locks down the systemd ExecStart line.
// Existing Fargate task defs reference `agent-opsd --config /path` literally
// — any future refactor of the template that changes that string breaks
// deployed sidecars on next restart. This test is the canary.
func TestSystemdRender_PinsExecStart(t *testing.T) {
	m := &systemdManager{
		unitPath:   "/tmp/agent-ops.service",
		binName:    "systemctl",
		unitName:   "agent-ops",
		defaultLog: "/var/log/agent-ops.log",
	}
	body, err := m.Render(Spec{
		ExecPath:   "/usr/local/bin/agent-opsd",
		ConfigPath: "/etc/agent-ops/config.yaml",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(body, "ExecStart=/usr/local/bin/agent-opsd --config /etc/agent-ops/config.yaml") {
		t.Fatalf("ExecStart line missing or rewritten — Fargate sidecars depend on this exact form.\nGot:\n%s", body)
	}
	if !strings.Contains(body, "[Install]") {
		t.Errorf("[Install] section missing")
	}
	if !strings.Contains(body, "User=root") {
		t.Errorf("User= should default to root when Spec.User is empty; got:\n%s", body)
	}
}

// TestSystemdRender_HonorsUserOverride checks that Spec.User flows into the
// rendered unit (so a future --user agent-ops install flag flips it cleanly).
func TestSystemdRender_HonorsUserOverride(t *testing.T) {
	m := &systemdManager{defaultLog: "/var/log/agent-ops.log"}
	body, err := m.Render(Spec{
		ExecPath:   "/usr/local/bin/agent-opsd",
		ConfigPath: "/etc/agent-ops/config.yaml",
		User:       "agent-ops",
		Group:      "agent-ops",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(body, "User=agent-ops") {
		t.Errorf("User=agent-ops not in rendered unit:\n%s", body)
	}
	if !strings.Contains(body, "Group=agent-ops") {
		t.Errorf("Group=agent-ops not in rendered unit:\n%s", body)
	}
}

// TestLaunchdRender_PinsLabel locks down the Mac plist Label + arguments.
// The Label is the handle launchctl uses for start/stop, so the test makes
// sure a future refactor doesn't desync it from launchdManager.label.
func TestLaunchdRender_PinsLabel(t *testing.T) {
	m := &launchdManager{
		label:      "dev.zibby.agent-ops",
		plistPath:  "/tmp/test.plist",
		defaultLog: "/tmp/agent-ops.log",
	}
	body, err := m.Render(Spec{
		ExecPath:   "/usr/local/bin/agent-opsd",
		ConfigPath: "/etc/agent-ops/config.yaml",
		StateDir:   "/tmp/state",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"<string>dev.zibby.agent-ops</string>",
		"<string>/usr/local/bin/agent-opsd</string>",
		"<string>--config</string>",
		"<string>/etc/agent-ops/config.yaml</string>",
		"<string>/tmp/state</string>",
		"<string>/tmp/agent-ops.log</string>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered plist missing %q:\n%s", want, body)
		}
	}
}

// TestRender_EscapesShellChars defends against a path-with-special-chars
// breaking the unit / plist. systemd reads ExecStart as a token list (no
// shell interpolation), so the only real risk is a literal newline that
// terminates the directive — text/template emits the value verbatim and the
// presence of a newline in body would split the line. We probe with two
// representative payloads.
func TestRender_EscapesShellChars(t *testing.T) {
	m := &systemdManager{defaultLog: "/var/log/agent-ops.log"}
	tricky := "/opt/agent-ops$with spaces/agent-opsd"
	body, err := m.Render(Spec{
		ExecPath:   tricky,
		ConfigPath: "/etc/agent-ops/config.yaml",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(body, "ExecStart="+tricky+" --config /etc/agent-ops/config.yaml") {
		t.Fatalf("path with spaces did not survive template render:\n%s", body)
	}
}
