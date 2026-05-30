// Copyright 2026 Zibby Lab. Apache-2.0.

// Package service installs, controls, and inspects the agent-ops daemon as
// a long-running system service on Linux (systemd) and macOS (launchd).
//
// We rolled this by hand rather than pulling in github.com/kardianos/service
// for three reasons:
//
//  1. The cross-cutting surface we actually need is small — render a
//     unit/plist template, write it to a known path, and shell out to
//     systemctl / launchctl for start/stop/status. ~250 lines vs an extra
//     transitive dependency tree.
//  2. We need to keep ExecStart pinned to the EXISTING `agent-opsd --config
//     /etc/agent-ops/config.yaml` invocation so Fargate task defs already
//     in production don't break. kardianos generates its own ExecStart from
//     os.Args, which would require a wrapper anyway.
//  3. Auditability — the unit file the user gets on disk is exactly what
//     they read in the source template. No magic.
//
// Windows support is deliberately omitted (phase 3). On any non-linux,
// non-darwin platform NewManager returns an ErrUnsupported.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// ErrUnsupported is returned by NewManager on platforms other than linux+systemd
// and darwin+launchd.
var ErrUnsupported = errors.New("service: unsupported platform (linux/darwin only in v0.2)")

// Spec is the data shipped into the unit / plist template.
type Spec struct {
	// ExecPath is the absolute path to the daemon binary. systemd ExecStart
	// and launchd ProgramArguments use this directly. The CLI populates it
	// with the agent-opsd alongside the running agent-ops binary, falling
	// back to PATH lookup.
	ExecPath string

	// ConfigPath is the absolute path to config.yaml.
	ConfigPath string

	// User / Group control "User=" in systemd. Empty defaults to "root"
	// (script installers that need apt-get/mount). The plan recommends
	// "agent-ops:agent-ops" for non-script-install setups.
	User  string
	Group string

	// StateDir is /var/lib/agent-ops on Linux, ~/Library/Application
	// Support/agent-ops on Mac.
	StateDir string

	// LogPath is /var/log/agent-ops.log (Linux) or
	// ~/Library/Logs/agent-ops.log (Mac).
	LogPath string
}

// Status is what `agent-ops status` (and the Manager.Status method) report.
type Status struct {
	// Installed = unit file / plist exists on disk
	Installed bool
	// Active = "is the daemon process actually running right now"
	Active bool
	// Raw is the verbatim output of the platform's status command for the
	// "agent-ops status" CLI to surface when --verbose is on.
	Raw string
}

// Manager is the platform-agnostic surface. The concrete implementation is
// chosen at NewManager() time based on runtime.GOOS.
type Manager interface {
	// Render returns the unit / plist contents that Install would write,
	// without touching the filesystem. Used by `init --dry-run` and by the
	// service_test.go round-trip tests.
	Render(spec Spec) (string, error)
	// UnitPath returns the filesystem destination the rendered unit will
	// be written to on Install (e.g. /etc/systemd/system/agent-ops.service).
	UnitPath() string
	// Install writes the unit file + daemon-reloads. Caller-supplied
	// Spec.ExecPath / ConfigPath must already exist.
	Install(ctx context.Context, spec Spec) error
	// Uninstall stops + removes the unit. Does NOT delete config or state.
	Uninstall(ctx context.Context) error
	// Start / Stop / Restart wrap the platform commands.
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	// Status reports current state.
	Status(ctx context.Context) (Status, error)
	// Logs returns the path to follow (Linux: empty → journalctl handles it
	// via a dedicated method; Mac: the launchd StandardOutPath). The CLI
	// uses this when no `--file` flag is given.
	LogPath() string
	// FollowLogsCmd returns the argv the CLI should exec to tail logs. Linux
	// uses `journalctl -u agent-ops -f`; Mac uses `tail -f <LogPath>`.
	FollowLogsCmd(sinceArg string) []string
}

// NewManager picks an implementation based on GOOS.
func NewManager() (Manager, error) {
	switch runtime.GOOS {
	case "linux":
		return &systemdManager{
			unitPath:   "/etc/systemd/system/agent-ops.service",
			binName:    "systemctl",
			unitName:   "agent-ops",
			defaultLog: "/var/log/agent-ops.log",
		}, nil
	case "darwin":
		// User-scope agent on Mac — installs to ~/Library/LaunchAgents.
		// System-scope (LaunchDaemons) requires sudo + a different
		// path; we treat that as an advanced opt-in left for a later
		// `agent-ops install --system` flag.
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("service: home dir: %w", err)
		}
		return &launchdManager{
			label:      "dev.zibby.agent-ops",
			plistPath:  filepath.Join(home, "Library", "LaunchAgents", "dev.zibby.agent-ops.plist"),
			defaultLog: filepath.Join(home, "Library", "Logs", "agent-ops.log"),
		}, nil
	default:
		return nil, ErrUnsupported
	}
}

// ─── systemd (Linux) ───────────────────────────────────────────────────────

type systemdManager struct {
	unitPath   string
	binName    string
	unitName   string
	defaultLog string
}

func (m *systemdManager) UnitPath() string { return m.unitPath }
func (m *systemdManager) LogPath() string  { return m.defaultLog }

func (m *systemdManager) Render(spec Spec) (string, error) {
	if spec.LogPath == "" {
		spec.LogPath = m.defaultLog
	}
	if spec.User == "" {
		spec.User = "root"
	}
	if spec.Group == "" {
		spec.Group = spec.User
	}
	return renderTemplate("agent-ops.service", systemdUnit, spec)
}

func (m *systemdManager) Install(ctx context.Context, spec Spec) error {
	body, err := m.Render(spec)
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.unitPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("service.Install: write unit: %w", err)
	}
	return runCmd(ctx, m.binName, "daemon-reload")
}

func (m *systemdManager) Uninstall(ctx context.Context) error {
	_ = runCmd(ctx, m.binName, "disable", "--now", m.unitName)
	if err := os.Remove(m.unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service.Uninstall: remove unit: %w", err)
	}
	return runCmd(ctx, m.binName, "daemon-reload")
}

func (m *systemdManager) Start(ctx context.Context) error {
	return runCmd(ctx, m.binName, "enable", "--now", m.unitName)
}
func (m *systemdManager) Stop(ctx context.Context) error {
	return runCmd(ctx, m.binName, "stop", m.unitName)
}
func (m *systemdManager) Restart(ctx context.Context) error {
	return runCmd(ctx, m.binName, "restart", m.unitName)
}

func (m *systemdManager) Status(ctx context.Context) (Status, error) {
	st := Status{}
	if _, err := os.Stat(m.unitPath); err == nil {
		st.Installed = true
	}
	out, _ := captureCmd(ctx, m.binName, "is-active", m.unitName)
	st.Active = strings.TrimSpace(out) == "active"
	verbose, _ := captureCmd(ctx, m.binName, "status", m.unitName, "--no-pager")
	st.Raw = verbose
	return st, nil
}

func (m *systemdManager) FollowLogsCmd(since string) []string {
	args := []string{"-u", m.unitName, "-f"}
	if since != "" {
		args = append(args, "--since", since)
	}
	return append([]string{"journalctl"}, args...)
}

// ─── launchd (macOS) ───────────────────────────────────────────────────────

type launchdManager struct {
	label      string
	plistPath  string
	defaultLog string
}

func (m *launchdManager) UnitPath() string { return m.plistPath }
func (m *launchdManager) LogPath() string  { return m.defaultLog }

func (m *launchdManager) Render(spec Spec) (string, error) {
	if spec.LogPath == "" {
		spec.LogPath = m.defaultLog
	}
	if spec.StateDir == "" {
		spec.StateDir = "/tmp"
	}
	return renderTemplate("agent-ops.plist", launchdPlist, spec)
}

func (m *launchdManager) Install(ctx context.Context, spec Spec) error {
	body, err := m.Render(spec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.plistPath), 0o755); err != nil {
		return fmt.Errorf("service.Install: mkdir LaunchAgents: %w", err)
	}
	if err := os.WriteFile(m.plistPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("service.Install: write plist: %w", err)
	}
	// `launchctl load` is the supported path for user-scope LaunchAgents
	// across macOS versions including the post-Catalina bootstrap-domain
	// rework. We tolerate "already loaded" so re-install is idempotent.
	_ = runCmd(ctx, "launchctl", "unload", m.plistPath)
	return runCmd(ctx, "launchctl", "load", m.plistPath)
}

func (m *launchdManager) Uninstall(ctx context.Context) error {
	_ = runCmd(ctx, "launchctl", "unload", m.plistPath)
	if err := os.Remove(m.plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service.Uninstall: remove plist: %w", err)
	}
	return nil
}

func (m *launchdManager) Start(ctx context.Context) error {
	// launchctl start (re-)kicks an already-loaded agent.
	return runCmd(ctx, "launchctl", "start", m.label)
}
func (m *launchdManager) Stop(ctx context.Context) error {
	return runCmd(ctx, "launchctl", "stop", m.label)
}
func (m *launchdManager) Restart(ctx context.Context) error {
	_ = m.Stop(ctx)
	return m.Start(ctx)
}

func (m *launchdManager) Status(ctx context.Context) (Status, error) {
	st := Status{}
	if _, err := os.Stat(m.plistPath); err == nil {
		st.Installed = true
	}
	// `launchctl list dev.zibby.agent-ops` prints "PID Status Label" — we
	// scrape the second column. Errors here (= not loaded) just mean
	// !Active.
	out, _ := captureCmd(ctx, "launchctl", "list", m.label)
	st.Raw = out
	// A loaded-but-stopped agent shows PID="-"; running shows a numeric PID.
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 1 && fields[0] != "-" && fields[0] != "PID" {
			if _, err := fmt.Sscanf(fields[0], "%d", new(int)); err == nil {
				st.Active = true
				break
			}
		}
	}
	return st, nil
}

func (m *launchdManager) FollowLogsCmd(_ string) []string {
	// launchd has no `since` equivalent that's portable across macOS
	// versions; we ignore the flag and just tail the log file.
	return []string{"tail", "-f", m.defaultLog}
}

// ─── helpers ──────────────────────────────────────────────────────────────

func renderTemplate(name, body string, spec Spec) (string, error) {
	t, err := template.New(name).Parse(body)
	if err != nil {
		return "", fmt.Errorf("service: parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, spec); err != nil {
		return "", fmt.Errorf("service: render %s: %w", name, err)
	}
	return buf.String(), nil
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("service: %s %s: %w (output: %s)",
			name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func captureCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
