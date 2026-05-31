// Copyright 2026 Zibby Lab. Apache-2.0.

package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// RemoteInvoker is the minimal subset of mcpclient.Client that
// RemoteToolAdapter needs. Defined as an interface here (rather than
// importing mcpclient) so internal/tool stays free of mcpclient's deps and
// so unit tests can mock with a struct.
type RemoteInvoker interface {
	Name() string
	CallTool(ctx context.Context, name string, args json.RawMessage) (RemoteCallResult, error)
}

// RemoteCallResult mirrors mcpclient.CallResult — duplicated to avoid a
// circular dep (mcpclient consumes tool indirectly via the registry, and
// tool would otherwise import mcpclient).
type RemoteCallResult struct {
	Text    string
	IsError bool
}

// RemoteToolAdapter wraps one remote MCP tool as a local Tool. The
// daemon's local LLM driver sees it the same as any built-in Tool.
//
// Naming convention: the local name is `{clientName}_{remoteToolName}`.
// E.g. an MCP client named `zibby` advertising `trigger_workflow`
// surfaces as `zibby_trigger_workflow` in the registry. This matches the
// notify-clause hint in the bundled templates ("look for names ending in
// `_trigger_workflow`, `_post_message`, …").
//
// Conflict policy: registry.Register errors on duplicates. The daemon
// boot path translates that into a Warn log + last-wins (later integration
// overrides earlier) so an operator who renames an integration without
// removing the old one doesn't crash the daemon.
type RemoteToolAdapter struct {
	clientName  string
	remoteName  string
	description string
	schema      json.RawMessage
	invoker     RemoteInvoker
}

// NewRemoteToolAdapter builds an adapter. The schema is the inputSchema
// the remote server advertised — passed through unchanged to the local LLM.
func NewRemoteToolAdapter(clientName, remoteName, description string, schema json.RawMessage, invoker RemoteInvoker) *RemoteToolAdapter {
	if len(schema) == 0 {
		// Empty inputSchema is legal in MCP — pass a minimal object schema
		// so the local driver doesn't reject the tool definition.
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return &RemoteToolAdapter{
		clientName:  clientName,
		remoteName:  remoteName,
		description: description,
		schema:      schema,
		invoker:     invoker,
	}
}

func (a *RemoteToolAdapter) Name() string {
	return a.clientName + "_" + a.remoteName
}

func (a *RemoteToolAdapter) Description() string {
	if a.description == "" {
		return fmt.Sprintf("Remote MCP tool '%s' from client '%s'.", a.remoteName, a.clientName)
	}
	return a.description
}

func (a *RemoteToolAdapter) Schema() json.RawMessage { return a.schema }

// Invoke forwards the call to the underlying MCP client. The remote
// server's text-content blocks are flattened into Result.Output; an
// isError=true response is surfaced as an error so the LLM sees the
// failure semantics it expects (matches built-in shell tool's behavior).
func (a *RemoteToolAdapter) Invoke(ctx context.Context, args json.RawMessage) (Result, error) {
	res, err := a.invoker.CallTool(ctx, a.remoteName, args)
	if err != nil {
		return Result{}, fmt.Errorf("remote-tool %s: %w", a.Name(), err)
	}
	if res.IsError {
		// The text typically already explains what went wrong; surface it
		// verbatim so the driver shows the LLM the same message a direct
		// MCP call would have shown.
		return Result{Output: res.Text}, fmt.Errorf("remote-tool %s: %s", a.Name(), res.Text)
	}
	return Result{Output: res.Text}, nil
}

// RemoteClientName returns the configured client name (the prefix half of
// the registered tool name). Useful for the daemon's de-dup logic when
// re-registering after a config change.
func (a *RemoteToolAdapter) RemoteClientName() string { return a.clientName }

// RemoteToolName returns the remote-side tool name (the suffix half).
func (a *RemoteToolAdapter) RemoteToolName() string { return a.remoteName }
