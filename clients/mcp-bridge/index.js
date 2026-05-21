#!/usr/bin/env node
/**
 * Copyright 2026 Zibby Lab. Apache-2.0.
 *
 * agent-ops-mcp — stdio MCP bridge.
 *
 * Some MCP clients (Claude Code, Cursor, Codex CLI, Gemini CLI) handle the
 * "streamable HTTP" remote transport unevenly — auth-header propagation,
 * SSE channel survival, and HTTPS strictness all vary by version. The
 * stdio transport is the one all of them implement well.
 *
 * This bridge runs in the client's local process as a stdio MCP server,
 * forwards every JSON-RPC frame as one HTTP POST to a remote agent-ops
 * daemon, and writes the response back on stdout. Zero translation; full
 * pass-through. The daemon stays the authoritative source of tools,
 * methods, and behavior.
 *
 * No dependencies beyond Node 18+ (built-in fetch + readline).
 *
 * Configuration (env vars):
 *   AGENT_OPS_URL    — required. Full URL of the remote /mcp endpoint
 *                      (e.g. "https://my-instance.apps.zibby.app:7842/mcp")
 *   AGENT_OPS_TOKEN  — required. Bearer token the daemon issued
 *   AGENT_OPS_TIMEOUT_MS — optional. Per-request timeout (default 30000)
 *
 * Wire:
 *   stdin  ← one JSON-RPC frame per line from the client
 *   stdout → one JSON-RPC frame per line back to the client
 *   stderr → human-readable diagnostics (never JSON-RPC; clients ignore)
 */

import { createInterface } from 'node:readline';
import { pathToFileURL } from 'node:url';
import process from 'node:process';

const DEFAULT_TIMEOUT_MS = 30_000;

// ── JSON-RPC error codes (subset of the spec we surface) ─────────────────
//   -32700 parse error
//   -32600 invalid request
//   -32601 method not found     (we don't generate this — the daemon does)
//   -32602 invalid params       (we don't generate this — the daemon does)
//   -32603 internal error       (network / 5xx / timeout — bridge-side)
//   -32001 unauthorized         (matches what the agent-ops daemon emits)

/**
 * makeErrorResponse builds a JSON-RPC error frame.
 * id may be null when we couldn't parse the request well enough to know it.
 */
export function makeErrorResponse(id, code, message, data) {
  const err = { code, message };
  if (data !== undefined) err.data = data;
  return { jsonrpc: '2.0', id: id ?? null, error: err };
}

/**
 * forwardOne sends one JSON-RPC frame to the daemon and returns whatever
 * the daemon replied with, OR a synthesized JSON-RPC error if the network
 * round-trip itself failed.
 *
 * Notifications (frames without an `id`) are POSTed but their response is
 * discarded — JSON-RPC says servers must not respond to notifications.
 *
 * This function is the unit-testable core. The main() loop is a thin shell
 * around it that handles stdin/stdout I/O.
 */
export async function forwardOne(req, opts) {
  const { url, token, timeoutMs, fetchImpl } = opts;
  const f = fetchImpl || globalThis.fetch;
  if (typeof f !== 'function') {
    return makeErrorResponse(
      req?.id ?? null,
      -32603,
      'agent-ops-mcp: fetch is unavailable (need Node 18+)',
    );
  }

  const isNotification = !('id' in req);

  // AbortController gives us a hard timeout the daemon can't sandbag.
  const ac = new AbortController();
  const t = setTimeout(() => ac.abort(), timeoutMs);

  let resp;
  try {
    resp = await f(url, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        accept: 'application/json',
        authorization: `Bearer ${token}`,
      },
      body: JSON.stringify(req),
      signal: ac.signal,
    });
  } catch (err) {
    if (isNotification) return null;
    const isAbort = err?.name === 'AbortError';
    return makeErrorResponse(
      req.id ?? null,
      -32603,
      isAbort
        ? `agent-ops-mcp: request timed out after ${timeoutMs}ms`
        : `agent-ops-mcp: network error: ${err?.message || String(err)}`,
    );
  } finally {
    clearTimeout(t);
  }

  // Notifications: daemon shouldn't have a body to send back; even if it
  // does, we discard. The client never sees a response either way.
  if (isNotification) return null;

  let bodyText = '';
  try {
    bodyText = await resp.text();
  } catch (err) {
    return makeErrorResponse(
      req.id ?? null,
      -32603,
      `agent-ops-mcp: failed to read response body: ${err?.message || String(err)}`,
    );
  }

  if (!resp.ok) {
    // Surface the daemon's HTTP error as a JSON-RPC error so the client
    // sees a structured failure rather than a stuck wait. 401 → unauthorized
    // mirrors what the daemon itself emits on bad auth.
    const code = resp.status === 401 ? -32001 : -32603;
    return makeErrorResponse(
      req.id ?? null,
      code,
      `agent-ops-mcp: daemon returned HTTP ${resp.status}`,
      { httpStatus: resp.status, body: bodyText.slice(0, 1000) },
    );
  }

  // Happy path: parse and return the daemon's JSON-RPC frame verbatim.
  try {
    return JSON.parse(bodyText);
  } catch (err) {
    return makeErrorResponse(
      req.id ?? null,
      -32603,
      `agent-ops-mcp: daemon returned malformed JSON: ${err?.message || String(err)}`,
      { rawBody: bodyText.slice(0, 1000) },
    );
  }
}

/**
 * readConfig pulls + validates env vars. Returns null + writes an
 * actionable error to stderr if anything is missing or malformed.
 */
export function readConfig(env) {
  const url = env.AGENT_OPS_URL;
  const token = env.AGENT_OPS_TOKEN;
  if (!url) {
    return { error: 'agent-ops-mcp: AGENT_OPS_URL env var is required' };
  }
  if (!token) {
    return { error: 'agent-ops-mcp: AGENT_OPS_TOKEN env var is required' };
  }
  try {
    // Validate URL early so we fail at start, not on first request.
    // Doesn't require any specific scheme — http for local tests is OK.
    new URL(url);
  } catch {
    return { error: `agent-ops-mcp: AGENT_OPS_URL is not a valid URL: ${url}` };
  }
  const timeoutMs = Number(env.AGENT_OPS_TIMEOUT_MS) || DEFAULT_TIMEOUT_MS;
  if (!Number.isFinite(timeoutMs) || timeoutMs < 1000) {
    return { error: 'agent-ops-mcp: AGENT_OPS_TIMEOUT_MS must be a number >= 1000' };
  }
  return { url, token, timeoutMs };
}

/**
 * runBridge wires stdin → forwardOne → stdout. Exits cleanly when stdin
 * closes (which is the MCP client signalling shutdown).
 *
 * Exposed for tests so they can drive the loop with controlled streams.
 */
export async function runBridge({ stdin, stdout, stderr, config, fetchImpl }) {
  const rl = createInterface({ input: stdin, terminal: false });

  // Serialize requests so the daemon sees them in order. MCP clients can
  // fire-and-forget multiple frames; the daemon's session state assumes
  // ordered delivery (e.g. initialize before any tools/call).
  let chain = Promise.resolve();

  rl.on('line', (line) => {
    if (!line.trim()) return;

    let req;
    try {
      req = JSON.parse(line);
    } catch (err) {
      const out = makeErrorResponse(null, -32700, `parse error: ${err?.message || String(err)}`);
      stdout.write(JSON.stringify(out) + '\n');
      return;
    }

    chain = chain.then(async () => {
      try {
        const resp = await forwardOne(req, { ...config, fetchImpl });
        if (resp !== null) {
          stdout.write(JSON.stringify(resp) + '\n');
        }
      } catch (err) {
        // Catch-all so a single bad frame can't kill the bridge.
        stderr?.write?.(`agent-ops-mcp: unexpected error: ${err?.message || String(err)}\n`);
        const out = makeErrorResponse(
          req?.id ?? null,
          -32603,
          `internal bridge error: ${err?.message || String(err)}`,
        );
        stdout.write(JSON.stringify(out) + '\n');
      }
    });
  });

  return new Promise((resolve) => {
    rl.on('close', async () => {
      // Drain in-flight requests before exiting so the last replies land
      // on stdout before the client sees EOF.
      try { await chain; } catch { /* already logged */ }
      resolve();
    });
  });
}

// ── CLI entrypoint ───────────────────────────────────────────────────────

async function main() {
  const cfg = readConfig(process.env);
  if ('error' in cfg) {
    process.stderr.write(cfg.error + '\n');
    process.stderr.write(
      'Usage:\n' +
      '  AGENT_OPS_URL=https://host:7842/mcp \\\n' +
      '  AGENT_OPS_TOKEN=ao_xxx \\\n' +
      '  agent-ops-mcp\n\n' +
      'Configure your AI agent (Claude Code, Cursor, Codex, Gemini) to spawn this\n' +
      'binary via "command": "npx", "args": ["-y", "agent-ops-mcp"]; pass the\n' +
      'URL + TOKEN through the "env" block.\n',
    );
    process.exit(1);
  }
  await runBridge({
    stdin: process.stdin,
    stdout: process.stdout,
    stderr: process.stderr,
    config: cfg,
  });
}

// Only auto-run when invoked as a script — not when imported by tests.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((err) => {
    process.stderr.write(`agent-ops-mcp: fatal: ${err?.stack || err?.message || err}\n`);
    process.exit(1);
  });
}
