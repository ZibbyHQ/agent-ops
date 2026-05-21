// Copyright 2026 Zibby Lab. Apache-2.0.

package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ShellTool runs a shell command. Outputs (stdout, stderr, exit code) are
// returned in a single text blob the LLM can read.
//
// Safety model in v0.1: there is no allowlist; the daemon runs as whatever
// user it's deployed as, so the operator constrains capability at the OS
// level (drop caps, read-only rootfs, etc). v0.2 will add an explicit
// command allowlist + denylist in config.
type ShellTool struct {
	// MaxOutputBytes truncates the captured output to keep LLM context bounded.
	MaxOutputBytes int

	// DefaultTimeout is the cap when the caller doesn't supply timeout_seconds.
	DefaultTimeout time.Duration
}

// NewShellTool returns a ShellTool with sensible defaults.
func NewShellTool() *ShellTool {
	return &ShellTool{
		MaxOutputBytes: 64 * 1024,
		DefaultTimeout: 60 * time.Second,
	}
}

func (t *ShellTool) Name() string { return "shell" }

func (t *ShellTool) Description() string {
	return "Run a shell command on the host. Returns combined stdout+stderr and exit code. " +
		"Use this for OS-level introspection, package management, log inspection, " +
		"docker/kubectl calls, anything reachable from a POSIX shell."
}

const shellSchemaJSON = `{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "Shell command to execute. Will be run via 'sh -c'."
    },
    "cwd": {
      "type": "string",
      "description": "Working directory. Defaults to the daemon's CWD."
    },
    "timeout_seconds": {
      "type": "integer",
      "minimum": 1,
      "maximum": 1800,
      "description": "Per-command timeout in seconds. Defaults to 60."
    }
  },
  "required": ["command"]
}`

func (t *ShellTool) Schema() json.RawMessage {
	return json.RawMessage(shellSchemaJSON)
}

type shellArgs struct {
	Command        string `json:"command"`
	Cwd            string `json:"cwd"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func (t *ShellTool) Invoke(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a shellArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return Result{}, fmt.Errorf("shell: parse args: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return Result{}, errors.New("shell: command is required")
	}
	to := t.DefaultTimeout
	if a.TimeoutSeconds > 0 {
		to = time.Duration(a.TimeoutSeconds) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", a.Command)
	if a.Cwd != "" {
		cmd.Dir = a.Cwd
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		// exec.ExitError carries the exit code; non-exit errors mean we
		// couldn't even start the process (timeout, missing sh, etc).
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else if cctx.Err() == context.DeadlineExceeded {
			exitCode = 124 // conventional "timed out" exit
		} else {
			return Result{}, fmt.Errorf("shell: start: %w", err)
		}
	}

	body := out.Bytes()
	truncated := false
	if t.MaxOutputBytes > 0 && len(body) > t.MaxOutputBytes {
		body = append(body[:t.MaxOutputBytes], []byte("\n[... truncated ...]")...)
		truncated = true
	}

	return Result{
		Output: fmt.Sprintf("exit=%d\n%s", exitCode, string(body)),
		Truncated: truncated,
	}, nil
}
