# agent-ops

> An autonomous DevOps engineer that lives next to your application.

> ⚠️ **Experimental — DO NOT run on production hosts you can't afford to lose.**
> This is a research project exploring "what if a small autonomous LLM agent acted as the operator of a single host?" It can run arbitrary shell commands on the box. The API, config schema, on-disk state format, and security model will all change. Pin to a commit if you depend on a specific behavior. We're publishing in the open so the design can be debated; we are NOT promising stability, support, or backwards compatibility at this stage.

`agent-ops` is a small open-source daemon that wraps an LLM agent (Claude today; Codex / Gemini / Ollama on the roadmap) and runs scheduled + on-demand ops tasks on a host. Bring your own API key. Tell it what should be true, in natural language. It uses tools (shell, soon docker / kubectl / http) to keep things that way.

**Status: experimental (v0.1).** Single-host MVP. The abstractions are cluster-ready (Node + Resource + event-log state) so v1.0 *could* grow into a Kubernetes-flavored multi-node design — that's the hypothesis we're testing, not a delivery commitment.

## How it differs from neighboring tools

| | Terraform | Ansible | Kubernetes operators | **agent-ops** |
|---|---|---|---|---|
| Spec language | HCL | YAML | Go types | **Natural language** |
| Scope | Provisioning | One-shot push | Cluster-scoped reconcile | **Single host, ops continuous** |
| Decides what to do | You | You | Code | **LLM, per your prompt** |
| Stays running | No | No | Yes (operator pod) | Yes (sidecar) |
| Reconciles on cron | No | No | On change | **On cron + on demand** |

agent-ops is not a Terraform replacement. It's an extra layer for "things that change too often or too contextually to encode as code" — upgrades, log triage, capacity adjustments, incident response.

## Quickstart (Docker)

```bash
docker run -d \
  --name agent-ops \
  -p 7842:7842 \
  -v /var/lib/agent-ops:/var/lib/agent-ops \
  -v $(pwd)/config.yaml:/etc/agent-ops/config.yaml:ro \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e AGENT_OPS_TOKEN=$(openssl rand -hex 32) \
  ghcr.io/zibbyhq/agent-ops:latest
```

Then point your local AI agent (Claude Code, Cursor, Codex CLI, Gemini CLI) at the daemon's MCP endpoint:

```json
{
  "mcpServers": {
    "agent-ops-prod": {
      "url": "http://your-host:7842/mcp",
      "headers": { "Authorization": "Bearer YOUR_AGENT_OPS_TOKEN" }
    }
  }
}
```

Now in your editor's AI chat you can say:

> "Show me what agent-ops is doing right now."
> "Update the weekly_upgrade task to also notify me on Slack."
> "Run a one-off cleanup of /tmp on the host."

## Configuration

A complete `config.yaml` lives at [`config.example.yaml`](./config.example.yaml). Highlights:

```yaml
agent:
  provider: claude              # v0.1: claude only
  model: claude-sonnet-4-6
  api_key_env: ANTHROPIC_API_KEY

mcp:
  listen_addr: ":7842"
  token_env: AGENT_OPS_TOKEN

bootstrap:
  prompt: "Set up this host from scratch..."

schedules:
  - name: hourly_health_check
    cron: "0 * * * *"
    prompt: "Verify the app is reachable..."
    tools: [shell]
```

## MCP tool surface

The daemon's MCP server exposes:

| Tool | Purpose |
|---|---|
| `agent_status` | Daemon health + last run |
| `agent_list_tasks` / `agent_get_task` / `agent_set_task` | Manage scheduled tasks |
| `agent_run_now` | Trigger an ad-hoc run |
| `agent_history` | Recent runs |
| `agent_logs` | Per-line log of one run |
| `host_shell` | Direct shell exec (skip the LLM) |

Remote agents (Claude Code etc.) see all of these. The internal Claude driver sees just the host tools (`shell`) — it picks what to invoke based on the user's prompt.

## Architecture

```
┌── compute host (Fargate / EC2 / k8s pod / Pi) ──────────┐
│  ┌── agent-ops daemon (Go, ~12MB) ──────────────┐       │
│  │ Scheduler ─► Task Runner ─► Driver (Claude) │       │
│  │                            │                 │       │
│  │                            ▼ tool calls      │       │
│  │                       Tool registry          │       │
│  │                       (shell, ...)           │       │
│  │ MCP server (:7842) ◄─ local Claude/Cursor ─► │       │
│  │ SQLite state (event-log + tables)            │       │
│  └──────────────────────────────────────────────┘       │
│  ┌── your application ─────────────────────────┐        │
│  │ OpenDesign / Gastown / Postgres / whatever │        │
│  └─────────────────────────────────────────────┘        │
└─────────────────────────────────────────────────────────┘
```

- **Single Go binary, ~12MB**. Runs anywhere Linux runs.
- **SQLite + event log**: every state change is appended to `events` before its table is updated. v1.0's Raft layer will ship the same events to replicas.
- **Sidecar by design**: doesn't replace your app, lives next to it.

## What's not done yet

- **No clustering**. v0.1 is single-node. Cluster mode (Raft consensus, pilot/worker, leader election) is on the roadmap.
- **One LLM driver** (Claude). Codex / Gemini / Ollama are stub interfaces — implementations land in v0.2.
- **One tool** (shell). `http`, `fs`, `docker`, `kubectl`, `git` are in the design but not implemented.
- **No telemetry export**. Internal logs only. OTel exporter in v0.2.
- **No outbound RPC to a control plane**. Designed for it (Zibby's hosted version uses outbound long-poll) but the MVP listens only.
- **No token rotation**. v0.1 mints once on first boot; restart with a new env var to rotate.

See [`ROADMAP.md`](./ROADMAP.md) for the post-v0.1 plan.

## Relationship to Zibby

This is part of [Zibby](https://zibby.dev)'s open ecosystem. The Zibby control plane uses `agent-ops` as the sidecar inside every hosted application instance, and the Zibby CLI / MCP (`@zibby/mcp-cli`) provisions instances + handles token lifecycle. Run agent-ops without Zibby too: it has no required runtime dependency on us.

## Contributing

PRs welcome. We require a [CLA](./CONTRIBUTING.md) for non-trivial contributions so the project stays cleanly Apache-2.0 licensable.

For security reports, see [`SECURITY.md`](./SECURITY.md).

## Experiment, not a product

This repo is an active design experiment. Specifically we want to find out:

1. **Does an LLM agent on a single host actually replace a human ops person for small workloads?** Or does it churn out plausible-looking actions that quietly drift state.
2. **Is "natural language mission journal" a stable abstraction?** Or do users want stricter Resource specs (Kubernetes-CRD style)?
3. **Where's the right line between in-process tools (`shell`) and outbound integrations (Slack, GitHub, …)?** And how does the agent learn that line.
4. **How does the cluster shape land?** v0.1 reserves Node + Resource + event-log seams for a future Raft pilot/worker design — we want to know if that shape survives real multi-node usage.

If those questions don't pan out, the project may pivot or shut down. If they do, v1.0 freezes a stable API.

In the meantime:
- Expect breaking changes between every minor version
- Expect bugs in the agent's judgment, not just its code
- Don't point this at anything you can't lose
- Telemetry from your runs (when implemented) goes only to your own backend — we are not collecting anything

## License

[Apache 2.0](./LICENSE) — © 2026 Zibby Lab.
