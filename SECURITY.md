# Security policy

## Reporting a vulnerability

Don't open a public GitHub issue.

Email **security@zibby.app** with:

- Description of the issue
- Steps to reproduce (PoC welcome)
- Affected version (`agent-opsd version`)
- Whether you've disclosed elsewhere

You'll get an acknowledgement within 3 business days. We aim to ship a fix within 14 days for high-severity issues; lower severity may take longer.

If you don't hear back in 7 days, you can escalate by opening a private GitHub Security Advisory on the repository.

## Disclosure

We follow a 90-day disclosure window. After a fix ships, you're welcome to publish a write-up. We'll credit you (unless you'd rather stay anonymous) and add the advisory to GitHub's vulnerability database.

## Scope

In scope:

- The `agent-opsd` daemon binary and all packages under `internal/`
- The Docker images published at `ghcr.io/zibbyhq/agent-ops`
- The MCP server's auth + authorization model
- The shell-tool sandboxing (such as it is)

Out of scope:

- Bugs in upstream tools the daemon shells out to (curl, docker, kubectl, etc.) — report those upstream
- Bugs in the Anthropic / OpenAI / Google APIs — report those to the relevant vendor
- Issues only reachable by an attacker who already has root on the host (the daemon does not promise to defend against the OS user it runs as)

## Defense-in-depth notes for operators

- Run agent-opsd as a non-root user where possible (the official Docker image does)
- Treat the bearer token as a secret — anyone with the token can run arbitrary shell on the host via `host_shell`
- Mount the state directory on a volume you back up
- Restrict network ingress to the MCP port (`:7842`) to your trusted clients
- Restrict the API key used by `ANTHROPIC_API_KEY` to a separate project / budget so a compromised daemon can't drain your main bill
