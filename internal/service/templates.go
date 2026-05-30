// Copyright 2026 Zibby Lab. Apache-2.0.

package service

import _ "embed"

// systemdUnit is the unit file written to /etc/systemd/system/agent-ops.service
// on Linux installs. ExecStart points at the daemon binary (`agent-opsd`),
// NOT `agent-ops daemon`, because the daemon binary is the stable contract
// that already-deployed Fargate task defs reference. Templated fields:
//
//	{{.ExecPath}}   absolute path to the agent-opsd binary
//	{{.ConfigPath}} absolute path to config.yaml
//	{{.User}}       unix user to run as (e.g. "agent-ops" or "root")
//	{{.Group}}      unix group (matches User by convention)
//	{{.StateDir}}   /var/lib/agent-ops (writable by User)
//	{{.LogPath}}    /var/log/agent-ops.log (journal is also fine — present for parity)
//
//go:embed systemd.service.tmpl
var systemdUnit string

// launchdPlist is written to ~/Library/LaunchAgents/dev.zibby.agent-ops.plist
// on darwin user installs, or /Library/LaunchDaemons/dev.zibby.agent-ops.plist
// for system installs. Templated fields mirror systemdUnit.
//
//go:embed launchd.plist.tmpl
var launchdPlist string
