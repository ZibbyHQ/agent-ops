# agent-ops roadmap

Numbered, not dated. Listed in roughly the order I expect to ship.

## v0.1 — single-host MVP (this release)

- ✅ Go daemon, single binary, ~12MB
- ✅ Claude (Anthropic Messages API) driver
- ✅ Cron scheduler + on-demand triggers
- ✅ SQLite state with event-log table (Raft-ready schema)
- ✅ MCP server over streamable HTTP for remote-agent control
- ✅ Shell tool with timeout + output truncation
- ✅ Bootstrap-on-first-run task
- ✅ Docker image (alpine base)
- ✅ Apache 2.0

## v0.2 — host coverage

- More host tools: `http`, `fs` (scoped), `docker`, `kubectl`, `git`
- Per-task tool allowlist enforced + audit log
- OTel exporter (metrics + traces)
- Smarter run summaries (the LLM writes a structured "what I did" object)
- Token rotation via CLI subcommand + MCP tool

## v0.3 — multi-driver

- `codex` driver (OpenAI Responses API)
- `gemini` driver (Google AI API)
- `ollama` driver (local models)
- Per-task driver override (e.g. cheap model for hourly checks, expensive for upgrades)

## v0.4 — production polish

- Outbound long-poll connector (no inbound port required — pulls instructions from a control plane URL)
- Resource model: declarative spec for what's-on-this-host (services, secrets, certs, dashboards). Reconciler loop derives Tasks from Resources.
- Per-tool sandbox profiles (seccomp on Linux, restricted user)
- Backup/restore of state DB to S3 / GCS / Azure Blob

## v0.5 — cluster

- Node roles: `pilot` + `worker`
- Raft consensus on the event log (hashicorp/raft)
- Leader election if the pilot dies
- Shared SQLite-on-FUSE OR per-node SQLite with log replication
- MCP endpoint load-balances across cluster; pilot owns the scheduler

## v0.6+ — beyond ops

- Multi-tenant single-daemon mode (one agent-ops process manages N apps with isolation)
- Web UI for run history + live dashboard (`agent-ops dashboard` subcommand)
- Plugin SDK (Go interface + RPC) so third parties can ship tools / drivers / connectors out-of-tree
- Cross-language SDK so non-Go projects can register tools

## v1.0 — stable API

- Frozen MCP tool schemas
- Frozen YAML config schema
- Frozen plugin SDK
- All v0.x reserved namespaces (`cluster.*`, `telemetry.*`, `connectors.*`) light up

## Won't do

- Kubernetes operator pattern. agent-ops is intentionally NOT a k8s operator — it's a daemon you put on a single host (which might be a Pod). The cluster goal is internal coordination, not "operating on a cluster's resources."
- Replace Terraform / OpenTofu. We don't provision cloud infra. Use Terraform for that and let agent-ops live on the hosts Terraform stood up.
- A SaaS hosted version sold separately. Zibby's hosted control plane is the commercial layer; the daemon stays open source.
