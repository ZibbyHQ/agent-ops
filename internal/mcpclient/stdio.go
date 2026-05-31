// Copyright 2026 Zibby Lab. Apache-2.0.

package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// stdioClient is the subprocess transport. JSON-RPC frames are
// newline-delimited on the child's stdin / stdout (per MCP stdio spec).
// We spin a single reader goroutine that demultiplexes responses to
// per-request channels keyed by id, and write requests serially under a
// mutex.
//
// Supervisor: Start launches the subprocess + reader. If the subprocess
// exits unexpectedly, the parent Manager (see manager.go) decides whether
// to call Start again — this client never auto-restarts itself, because
// the registered tool.RemoteToolAdapter would otherwise see stale tool
// metadata after a restart that re-listed.
type stdioClient struct {
	cfg  Config
	ids  idGen
	pend struct {
		sync.Mutex
		m map[int]chan rpcResponse
	}

	mu      sync.Mutex // guards cmd + writer + closed
	cmd     *exec.Cmd
	writer  io.WriteCloser
	closed  bool
	doneCh  chan struct{} // closed when the reader goroutine exits
	exitErr error         // captured exit status; protected by mu
}

func newStdioClient(cfg Config) *stdioClient {
	c := &stdioClient{cfg: cfg}
	c.pend.m = map[int]chan rpcResponse{}
	return c
}

func (c *stdioClient) Name() string { return c.cfg.Name }

func (c *stdioClient) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("mcpclient: stdio: client closed")
	}
	if c.cmd != nil && c.cmd.Process != nil {
		return nil // already running
	}

	cmd := exec.CommandContext(context.Background(), c.cfg.Command, c.cfg.Args...)
	// Inherit parent env then layer the cfg.Env overrides — lets the operator
	// pin e.g. NODE_OPTIONS without losing PATH.
	env := os.Environ()
	for k, v := range c.cfg.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcpclient: %s: stdin: %w", c.cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mcpclient: %s: stdout: %w", c.cfg.Name, err)
	}
	// Capture stderr to caller's stderr so the operator sees the subprocess's
	// diagnostics in the daemon log.
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcpclient: %s: start: %w", c.cfg.Name, err)
	}
	c.cmd = cmd
	c.writer = stdin
	c.doneCh = make(chan struct{})

	go c.readLoop(stdout)
	go func() {
		// Reap the process so it doesn't zombie. Capture exit status for the
		// Manager to inspect via Done().
		err := cmd.Wait()
		c.mu.Lock()
		c.exitErr = err
		c.mu.Unlock()
		close(c.doneCh)
	}()
	return nil
}

// Done returns a channel closed when the subprocess exits. The Manager uses
// this to detect crashes and re-Start with backoff.
func (c *stdioClient) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.doneCh
}

// ExitErr returns the captured exit status after Done() fires. Safe to call
// concurrently.
func (c *stdioClient) ExitErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitErr
}

func (c *stdioClient) Close() error {
	c.mu.Lock()
	c.closed = true
	cmd := c.cmd
	writer := c.writer
	done := c.doneCh
	c.mu.Unlock()

	if writer != nil {
		_ = writer.Close() // signals EOF to most well-behaved servers
	}
	if cmd != nil && cmd.Process != nil {
		// Give the process a moment to exit cleanly on EOF, then kill.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}

	// Fail any in-flight callers.
	c.pend.Lock()
	for id, ch := range c.pend.m {
		close(ch)
		delete(c.pend.m, id)
	}
	c.pend.Unlock()
	return nil
}

func (c *stdioClient) Initialize(ctx context.Context) error {
	if _, err := c.call(ctx, "initialize", initializeParams(c.cfg.Name)); err != nil {
		return err
	}
	// Send the spec-required follow-up notification. We must not wait for a
	// response (notifications have no id), so use a fire-and-forget write.
	notif, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	c.mu.Lock()
	w := c.writer
	c.mu.Unlock()
	if w != nil {
		_, _ = w.Write(append(notif, '\n'))
	}
	return nil
}

func (c *stdioClient) ListTools(ctx context.Context) ([]ToolDef, error) {
	raw, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var wire struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("mcpclient: %s: decode tools/list: %w", c.cfg.Name, err)
	}
	return wire.Tools, nil
}

func (c *stdioClient) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	raw, err := c.call(ctx, "tools/call", callToolParams(name, args))
	if err != nil {
		return CallResult{}, err
	}
	return flattenContent(raw)
}

// call writes one request frame and waits for its matching response on the
// per-id channel. Returns when (a) the response arrives, (b) the context is
// canceled, or (c) the subprocess exits.
func (c *stdioClient) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := c.ids.next()
	ch := make(chan rpcResponse, 1)
	c.pend.Lock()
	c.pend.m[id] = ch
	c.pend.Unlock()
	defer func() {
		c.pend.Lock()
		delete(c.pend.m, id)
		c.pend.Unlock()
	}()

	req, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	c.mu.Lock()
	w := c.writer
	done := c.doneCh
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("mcpclient: %s: closed", c.cfg.Name)
	}
	if w == nil {
		return nil, fmt.Errorf("mcpclient: %s: not started", c.cfg.Name)
	}
	if _, err := w.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("mcpclient: %s: write: %w", c.cfg.Name, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcpclient: %s: subprocess exited", c.cfg.Name)
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
		return nil, fmt.Errorf("mcpclient: %s: subprocess exited mid-call", c.cfg.Name)
	}
}

// readLoop consumes the subprocess's stdout. One JSON-RPC message per line.
// Responses go to their pending channel by id; notifications (no id) are
// discarded in v0.3 — server-initiated events aren't surfaced upward yet.
func (c *stdioClient) readLoop(r io.Reader) {
	scan := bufio.NewScanner(r)
	// Bump max line size — tools/list bodies can be 100KB+ for big servers.
	scan.Buffer(make([]byte, 64*1024), 8<<20)
	for scan.Scan() {
		line := scan.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Not all stdio servers strictly emit JSON-RPC framed lines —
			// some interleave human-readable logs. Skip silently.
			continue
		}
		if resp.ID == 0 {
			continue // notification — not subscribed in v0.3
		}
		c.pend.Lock()
		ch, ok := c.pend.m[resp.ID]
		c.pend.Unlock()
		if ok {
			ch <- resp
		}
	}
}
