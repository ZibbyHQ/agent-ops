// Copyright 2026 Zibby Lab. Apache-2.0.

import { test, describe, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { PassThrough } from 'node:stream';
import { once } from 'node:events';
import { setTimeout as wait } from 'node:timers/promises';

import { forwardOne, makeErrorResponse, readConfig, runBridge } from './index.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const BRIDGE_PATH = join(__dirname, 'index.js');

// ── Tiny mock daemon ────────────────────────────────────────────────────
// Records every request the bridge makes and lets the test script replies.
function startMock({ replies = [], onRequest = null } = {}) {
  const calls = [];
  const server = createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const body = Buffer.concat(chunks).toString('utf-8');
    let json = null;
    try { json = JSON.parse(body); } catch { /* let test see the raw */ }
    calls.push({
      method: req.method,
      url: req.url,
      headers: req.headers,
      body,
      json,
    });
    if (onRequest) {
      const handled = await onRequest(json, res);
      if (handled) return;
    }
    const reply = replies.shift();
    if (!reply) {
      res.writeHead(500, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ error: 'mock: out of scripted replies' }));
      return;
    }
    res.writeHead(reply.status ?? 200, reply.headers ?? { 'content-type': 'application/json' });
    res.end(typeof reply.body === 'string' ? reply.body : JSON.stringify(reply.body));
  });
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({
        server,
        calls,
        url: `http://127.0.0.1:${port}/mcp`,
        close: () => new Promise((r) => server.close(r)),
      });
    });
  });
}

// ─────────────────────────────────────────────────────────────────────────
// Unit tests — exercise forwardOne / readConfig / makeErrorResponse directly
// ─────────────────────────────────────────────────────────────────────────

describe('makeErrorResponse', () => {
  test('builds a JSON-RPC error envelope with id passed through', () => {
    const out = makeErrorResponse(42, -32601, 'method not found');
    assert.deepEqual(out, {
      jsonrpc: '2.0',
      id: 42,
      error: { code: -32601, message: 'method not found' },
    });
  });

  test('null id when caller cannot determine it (parse error case)', () => {
    const out = makeErrorResponse(undefined, -32700, 'parse error');
    assert.equal(out.id, null);
  });

  test('attaches data when provided', () => {
    const out = makeErrorResponse(1, -32603, 'oops', { detail: 'x' });
    assert.deepEqual(out.error.data, { detail: 'x' });
  });
});

describe('readConfig', () => {
  test('rejects missing URL', () => {
    const r = readConfig({ AGENT_OPS_TOKEN: 't' });
    assert.match(r.error, /AGENT_OPS_URL/);
  });

  test('rejects missing token', () => {
    const r = readConfig({ AGENT_OPS_URL: 'http://x:1/mcp' });
    assert.match(r.error, /AGENT_OPS_TOKEN/);
  });

  test('rejects malformed URL', () => {
    const r = readConfig({ AGENT_OPS_URL: '::not-a-url', AGENT_OPS_TOKEN: 't' });
    assert.match(r.error, /AGENT_OPS_URL/);
  });

  test('rejects timeout below floor', () => {
    const r = readConfig({
      AGENT_OPS_URL: 'http://x:1/mcp',
      AGENT_OPS_TOKEN: 't',
      AGENT_OPS_TIMEOUT_MS: '500',
    });
    assert.match(r.error, /AGENT_OPS_TIMEOUT_MS/);
  });

  test('accepts a minimal valid env + applies defaults', () => {
    const r = readConfig({
      AGENT_OPS_URL: 'http://localhost:7842/mcp',
      AGENT_OPS_TOKEN: 'ao_x',
    });
    assert.equal(r.url, 'http://localhost:7842/mcp');
    assert.equal(r.token, 'ao_x');
    assert.equal(r.timeoutMs, 30_000);
  });
});

describe('forwardOne', () => {
  let mock;
  before(async () => { mock = await startMock(); });
  after(async () => { await mock.close(); });

  test('happy path: forwards request, returns daemon JSON-RPC verbatim', async () => {
    mock.calls.length = 0;
    const reply = { jsonrpc: '2.0', id: 1, result: { ok: true } };
    mock.server.removeAllListeners('request');
    const server2 = await startMock({ replies: [{ body: reply }] });
    try {
      const out = await forwardOne(
        { jsonrpc: '2.0', id: 1, method: 'initialize', params: {} },
        { url: server2.url, token: 'ao_test', timeoutMs: 5000 },
      );
      assert.deepEqual(out, reply);
      assert.equal(server2.calls.length, 1);
      assert.equal(server2.calls[0].method, 'POST');
      assert.equal(server2.calls[0].headers.authorization, 'Bearer ao_test');
      assert.deepEqual(server2.calls[0].json, {
        jsonrpc: '2.0', id: 1, method: 'initialize', params: {},
      });
    } finally {
      await server2.close();
    }
  });

  test('notification (no id): forwards but returns null, no client reply', async () => {
    const server2 = await startMock({ replies: [{ status: 202, body: '' }] });
    try {
      const out = await forwardOne(
        { jsonrpc: '2.0', method: 'notifications/initialized' },
        { url: server2.url, token: 'ao_test', timeoutMs: 5000 },
      );
      assert.equal(out, null);
      assert.equal(server2.calls.length, 1);
    } finally {
      await server2.close();
    }
  });

  test('daemon HTTP 401 → -32001 unauthorized', async () => {
    const server2 = await startMock({ replies: [{ status: 401, body: { error: 'nope' } }] });
    try {
      const out = await forwardOne(
        { jsonrpc: '2.0', id: 5, method: 'tools/list' },
        { url: server2.url, token: 'wrong', timeoutMs: 5000 },
      );
      assert.equal(out.id, 5);
      assert.equal(out.error.code, -32001);
      assert.match(out.error.message, /HTTP 401/);
    } finally {
      await server2.close();
    }
  });

  test('daemon HTTP 500 → -32603 internal error with body excerpt', async () => {
    const server2 = await startMock({ replies: [{ status: 500, body: 'kaboom' }] });
    try {
      const out = await forwardOne(
        { jsonrpc: '2.0', id: 6, method: 'tools/call', params: {} },
        { url: server2.url, token: 'ao_test', timeoutMs: 5000 },
      );
      assert.equal(out.error.code, -32603);
      assert.match(out.error.message, /HTTP 500/);
      assert.equal(out.error.data.httpStatus, 500);
    } finally {
      await server2.close();
    }
  });

  test('daemon returns malformed JSON → -32603 with rawBody for debugging', async () => {
    const server2 = await startMock({ replies: [{ body: '{not json' }] });
    try {
      const out = await forwardOne(
        { jsonrpc: '2.0', id: 7, method: 'ping' },
        { url: server2.url, token: 'ao_test', timeoutMs: 5000 },
      );
      assert.equal(out.error.code, -32603);
      assert.match(out.error.message, /malformed JSON/);
      assert.ok(out.error.data.rawBody);
    } finally {
      await server2.close();
    }
  });

  test('network error → -32603 (and notification swallows error entirely)', async () => {
    const out1 = await forwardOne(
      { jsonrpc: '2.0', id: 8, method: 'ping' },
      { url: 'http://127.0.0.1:1/mcp', token: 'ao_test', timeoutMs: 1500 },
    );
    assert.equal(out1.error.code, -32603);
    assert.match(out1.error.message, /network|fetch|refused|connect/i);

    const out2 = await forwardOne(
      { jsonrpc: '2.0', method: 'notifications/x' }, // notification
      { url: 'http://127.0.0.1:1/mcp', token: 'ao_test', timeoutMs: 1500 },
    );
    assert.equal(out2, null);
  });

  test('honors timeoutMs', async () => {
    // Start a server that never responds.
    const server2 = await startMock({
      onRequest: async () => {
        await wait(5000);
        return false;
      },
    });
    try {
      const start = Date.now();
      const out = await forwardOne(
        { jsonrpc: '2.0', id: 9, method: 'slow' },
        { url: server2.url, token: 'ao_test', timeoutMs: 1500 },
      );
      const elapsed = Date.now() - start;
      assert.ok(elapsed < 3500, `timeout did not fire promptly: ${elapsed}ms`);
      assert.equal(out.error.code, -32603);
      assert.match(out.error.message, /timed out/i);
    } finally {
      await server2.close();
    }
  });
});

// ─────────────────────────────────────────────────────────────────────────
// runBridge — exercise the stdin/stdout loop with in-memory streams
// ─────────────────────────────────────────────────────────────────────────

describe('runBridge', () => {
  test('roundtrips a sequence of frames over in-memory streams', async () => {
    const mock = await startMock({
      replies: [
        { body: { jsonrpc: '2.0', id: 1, result: { protocolVersion: 'x' } } },
        { body: { jsonrpc: '2.0', id: 2, result: { tools: [{ name: 'agent_status' }] } } },
      ],
    });
    const stdin = new PassThrough();
    const stdout = new PassThrough();
    const stderr = new PassThrough();
    const lines = [];
    stdout.on('data', (b) => {
      for (const line of b.toString().split('\n')) {
        if (line.trim()) lines.push(line);
      }
    });
    const done = runBridge({
      stdin, stdout, stderr,
      config: { url: mock.url, token: 'ao_test', timeoutMs: 5000 },
    });
    stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'initialize' }) + '\n');
    stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 2, method: 'tools/list' }) + '\n');
    // Trigger graceful drain.
    stdin.end();
    await done;
    await mock.close();
    assert.equal(lines.length, 2);
    assert.equal(JSON.parse(lines[0]).id, 1);
    assert.equal(JSON.parse(lines[1]).id, 2);
  });

  test('parse error on bad input emits a -32700 frame but keeps the loop alive', async () => {
    const mock = await startMock({
      replies: [{ body: { jsonrpc: '2.0', id: 7, result: 'ok' } }],
    });
    const stdin = new PassThrough();
    const stdout = new PassThrough();
    const stderr = new PassThrough();
    const lines = [];
    stdout.on('data', (b) => {
      for (const l of b.toString().split('\n')) if (l.trim()) lines.push(l);
    });
    const done = runBridge({
      stdin, stdout, stderr,
      config: { url: mock.url, token: 'ao_test', timeoutMs: 5000 },
    });
    stdin.write('{not valid json\n');
    stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 7, method: 'ping' }) + '\n');
    stdin.end();
    await done;
    await mock.close();
    assert.equal(lines.length, 2);
    const first = JSON.parse(lines[0]);
    assert.equal(first.error.code, -32700);
    assert.equal(first.id, null);
    const second = JSON.parse(lines[1]);
    assert.equal(second.id, 7);
    assert.equal(second.result, 'ok');
  });

  test('skips empty stdin lines silently', async () => {
    const mock = await startMock({
      replies: [{ body: { jsonrpc: '2.0', id: 1, result: 'ok' } }],
    });
    const stdin = new PassThrough();
    const stdout = new PassThrough();
    const stderr = new PassThrough();
    const lines = [];
    stdout.on('data', (b) => {
      for (const l of b.toString().split('\n')) if (l.trim()) lines.push(l);
    });
    const done = runBridge({
      stdin, stdout, stderr,
      config: { url: mock.url, token: 'ao_test', timeoutMs: 5000 },
    });
    stdin.write('\n\n');
    stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'ping' }) + '\n');
    stdin.write('\n');
    stdin.end();
    await done;
    await mock.close();
    assert.equal(lines.length, 1, `got: ${lines.join('|')}`);
  });

  test('notification frames produce no stdout line', async () => {
    const mock = await startMock({ replies: [{ status: 202, body: '' }] });
    const stdin = new PassThrough();
    const stdout = new PassThrough();
    const stderr = new PassThrough();
    const lines = [];
    stdout.on('data', (b) => {
      for (const l of b.toString().split('\n')) if (l.trim()) lines.push(l);
    });
    const done = runBridge({
      stdin, stdout, stderr,
      config: { url: mock.url, token: 'ao_test', timeoutMs: 5000 },
    });
    stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n');
    stdin.end();
    await done;
    assert.equal(mock.calls.length, 1, 'notification should still POST to daemon');
    assert.equal(lines.length, 0, 'but bridge must not write any stdout frame');
    await mock.close();
  });
});

// ─────────────────────────────────────────────────────────────────────────
// End-to-end — spawn the bridge as a real subprocess, talk to a real
// localhost HTTP mock. This is the path the real MCP client will exercise.
// ─────────────────────────────────────────────────────────────────────────

describe('e2e: spawned subprocess', () => {
  test('initialize → tools/list → tools/call roundtrip through real stdio', async () => {
    const mock = await startMock({
      replies: [
        { body: { jsonrpc: '2.0', id: 1, result: { protocolVersion: '2024-11-05', serverInfo: { name: 'agent-ops', version: '0.1.1' } } } },
        { body: { jsonrpc: '2.0', id: 2, result: { tools: [{ name: 'agent_status' }, { name: 'host_shell' }] } } },
        { body: { jsonrpc: '2.0', id: 3, result: { content: [{ type: 'text', text: 'ok' }], isError: false } } },
      ],
    });

    const child = spawn(process.execPath, [BRIDGE_PATH], {
      env: {
        ...process.env,
        AGENT_OPS_URL: mock.url,
        AGENT_OPS_TOKEN: 'ao_e2e',
      },
      stdio: ['pipe', 'pipe', 'pipe'],
    });

    const stdoutLines = [];
    let buf = '';
    child.stdout.on('data', (chunk) => {
      buf += chunk.toString();
      let nl;
      while ((nl = buf.indexOf('\n')) !== -1) {
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + 1);
        if (line.trim()) stdoutLines.push(line);
      }
    });

    const stderrBuf = [];
    child.stderr.on('data', (c) => stderrBuf.push(c.toString()));

    child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} }) + '\n');
    child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 2, method: 'tools/list' }) + '\n');
    child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 3, method: 'tools/call', params: { name: 'agent_status', arguments: {} } }) + '\n');

    // Wait until we've seen 3 stdout lines or timeout.
    const deadline = Date.now() + 5000;
    while (stdoutLines.length < 3 && Date.now() < deadline) {
      await wait(20);
    }

    child.stdin.end();
    await once(child, 'exit');
    await mock.close();

    assert.equal(stdoutLines.length, 3, `wanted 3 frames, got ${stdoutLines.length}; stderr: ${stderrBuf.join('')}`);
    const init = JSON.parse(stdoutLines[0]);
    assert.equal(init.id, 1);
    assert.equal(init.result.protocolVersion, '2024-11-05');

    const tools = JSON.parse(stdoutLines[1]);
    assert.equal(tools.id, 2);
    assert.equal(tools.result.tools.length, 2);

    const call = JSON.parse(stdoutLines[2]);
    assert.equal(call.id, 3);
    assert.equal(call.result.isError, false);

    // All 3 frames must have hit the mock with the right auth header.
    assert.equal(mock.calls.length, 3);
    for (const c of mock.calls) {
      assert.equal(c.method, 'POST');
      assert.equal(c.headers.authorization, 'Bearer ao_e2e');
      assert.equal(c.headers['content-type'], 'application/json');
    }
  });

  test('subprocess: missing AGENT_OPS_URL fails fast with helpful stderr', async () => {
    const child = spawn(process.execPath, [BRIDGE_PATH], {
      env: { ...process.env, AGENT_OPS_TOKEN: 'x', AGENT_OPS_URL: '' },
      stdio: ['pipe', 'pipe', 'pipe'],
    });
    const stderr = [];
    child.stderr.on('data', (c) => stderr.push(c.toString()));
    const [code] = await once(child, 'exit');
    assert.equal(code, 1);
    assert.match(stderr.join(''), /AGENT_OPS_URL/);
  });
});
