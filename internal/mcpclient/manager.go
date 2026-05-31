// Copyright 2026 Zibby Lab. Apache-2.0.

package mcpclient

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Manager owns the daemon's set of MCP clients. Constructor takes a list of
// Configs, dials each one, lists its tools, and hands the resulting
// (Client, []ToolDef) pairs back to the caller. The caller (daemon.Run) is
// responsible for wrapping each pair into a tool.RemoteToolAdapter and
// registering it.
//
// Manager also supervises stdio subprocesses: if one exits unexpectedly it
// is restarted with exponential backoff. HTTP clients have no supervision
// needed — they're stateless per request.
//
// Lifecycle:
//
//	mgr, started := mcpclient.Boot(ctx, cfgs, log)
//	// register the tools from `started` into the tool registry
//	defer mgr.Close()
//
// On Close, every client gets Close() called; supervisors exit.
type Manager struct {
	log     *slog.Logger
	mu      sync.Mutex
	clients []Client
	stop    chan struct{}
	wg      sync.WaitGroup
}

// Started is one running client + its initial tool listing. The caller
// uses Tools to register tool.RemoteToolAdapter entries.
type Started struct {
	Client Client
	Tools  []ToolDef
}

// Boot dials every Config in order, returns the successful ones, and
// starts background supervisors for stdio transports.
//
// Per-client failures (bad URL, missing token, subprocess exec failure)
// are logged + skipped — they MUST NOT prevent the daemon from booting,
// because an misconfigured integration shouldn't take down the operator's
// schedule. The returned slice contains only clients that initialized.
func Boot(ctx context.Context, cfgs []Config, log *slog.Logger) (*Manager, []Started, error) {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{log: log, stop: make(chan struct{})}
	var out []Started

	for _, cfg := range cfgs {
		c, err := New(cfg)
		if err != nil {
			log.Warn("mcpclient: skipping bad config", "name", cfg.Name, "error", err)
			continue
		}
		if err := c.Start(ctx); err != nil {
			log.Warn("mcpclient: start failed", "name", cfg.Name, "error", err)
			continue
		}
		if err := c.Initialize(ctx); err != nil {
			log.Warn("mcpclient: initialize failed", "name", cfg.Name, "error", err)
			_ = c.Close()
			continue
		}
		tools, err := c.ListTools(ctx)
		if err != nil {
			log.Warn("mcpclient: tools/list failed", "name", cfg.Name, "error", err)
			_ = c.Close()
			continue
		}
		log.Info("mcpclient: connected",
			"name", cfg.Name,
			"transport", string(cfg.Transport),
			"tool_count", len(tools))
		m.clients = append(m.clients, c)
		out = append(out, Started{Client: c, Tools: tools})

		// Supervise stdio subprocesses for crash-restart.
		if sc, ok := c.(*stdioClient); ok {
			m.wg.Add(1)
			go m.superviseStdio(sc, cfg)
		}
	}
	return m, out, nil
}

// superviseStdio watches one stdio subprocess and re-starts it on crash
// with exponential backoff (1s → 2s → 4s → 8s → 16s → 30s capped). The
// re-Started client retains the same Client identity / tool list — the
// registered RemoteToolAdapter doesn't need re-registering.
//
// Why don't we re-list tools on restart? Because a subprocess that crashed
// and came back with a DIFFERENT tool set is a configuration / version
// change the operator should know about — silently changing the local
// LLM's available tools mid-run would be surprising. Out of scope for v0.3
// but noted as a follow-up.
func (m *Manager) superviseStdio(sc *stdioClient, cfg Config) {
	defer m.wg.Done()
	maxBackoff := time.Duration(cfg.MaxBackoff) * time.Second
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	backoff := time.Second

	for {
		select {
		case <-sc.Done():
			// Subprocess exited. Was it because we asked it to?
			sc.mu.Lock()
			closed := sc.closed
			sc.mu.Unlock()
			if closed {
				return
			}
			m.log.Warn("mcpclient: stdio subprocess exited, will restart",
				"name", cfg.Name,
				"err", sc.ExitErr(),
				"backoff_ms", backoff.Milliseconds())
			select {
			case <-time.After(backoff):
			case <-m.stop:
				return
			}
			// Reset the cmd handle so Start can re-exec.
			sc.mu.Lock()
			sc.cmd = nil
			sc.writer = nil
			sc.doneCh = nil
			sc.mu.Unlock()

			if err := sc.Start(context.Background()); err != nil {
				m.log.Error("mcpclient: stdio restart failed", "name", cfg.Name, "error", err)
				backoff = nextBackoff(backoff, maxBackoff)
				continue
			}
			if err := sc.Initialize(context.Background()); err != nil {
				m.log.Error("mcpclient: stdio re-initialize failed", "name", cfg.Name, "error", err)
				backoff = nextBackoff(backoff, maxBackoff)
				continue
			}
			m.log.Info("mcpclient: stdio restart ok", "name", cfg.Name)
			backoff = time.Second // reset on success
		case <-m.stop:
			return
		}
	}
}

func nextBackoff(b, max time.Duration) time.Duration {
	b *= 2
	if b > max {
		return max
	}
	return b
}

// Close shuts down every supervised client. Idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.stop == nil {
		m.mu.Unlock()
		return nil
	}
	close(m.stop)
	m.stop = nil
	clients := m.clients
	m.clients = nil
	m.mu.Unlock()

	var firstErr error
	for _, c := range clients {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("mcpclient: close %s: %w", c.Name(), err)
		}
	}
	m.wg.Wait()
	return firstErr
}
