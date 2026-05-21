// Copyright 2026 Zibby Lab. Apache-2.0.

// Package tool defines what the agent can do.
//
// Tools are first-class objects: each has a name, a JSON-schema input, and
// an Invoke method. The same Tool is exposed two ways:
//   - to the internal LLM driver (claude.go translates Tool.Schema() into the
//     provider's tool-use format), and
//   - to remote callers via the MCP server (mcp.go renders the same Schema()
//     into MCP's tools/list response).
//
// This single-definition rule keeps the surface consistent and forces
// permission/audit logic into one place.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Tool is one capability the agent can invoke.
type Tool interface {
	// Name uniquely identifies the tool. MUST be stable across versions; the
	// LLM may have learned this name.
	Name() string

	// Description is shown to the LLM and to MCP clients.
	Description() string

	// Schema is the JSON-schema of the input. Same object served to Claude's
	// tool-use API and to MCP's tools/list.
	Schema() json.RawMessage

	// Invoke runs the tool with input args. Returns a string result (which the
	// LLM sees as tool-result content). Errors are surfaced as
	// {"error": "...", "ok": false}-style strings so the LLM can adapt.
	Invoke(ctx context.Context, args json.RawMessage) (Result, error)
}

// Result is what Invoke returns.
type Result struct {
	// Output is what the LLM sees. Keep it parsable but not too long.
	Output string `json:"output"`

	// Truncated indicates Output was clamped to fit a size budget.
	Truncated bool `json:"truncated,omitempty"`

	// Sensitive marks the output as containing secrets — the MCP layer may
	// redact this from logs / history. Tools opt in via this flag.
	Sensitive bool `json:"sensitive,omitempty"`
}

// Registry is the daemon's set of available tools, looked up by Name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds a tool. Duplicate names error.
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return errors.New("tool.Register: nil tool")
	}
	name := t.Name()
	if name == "" {
		return errors.New("tool.Register: tool has empty Name()")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("tool.Register: %q already registered", name)
	}
	r.tools[name] = t
	return nil
}

// MustRegister panics on error. Use during daemon boot.
func (r *Registry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Get looks up a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List returns all tools sorted by name. Useful for MCP tools/list.
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Subset returns a Registry containing only tools named in allow. Empty allow
// returns the full Registry. Useful for per-Task tool allowlists.
func (r *Registry) Subset(allow []string) *Registry {
	if len(allow) == 0 {
		return r
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := NewRegistry()
	for _, name := range allow {
		if t, ok := r.tools[name]; ok {
			_ = out.Register(t)
		}
	}
	return out
}

// Names returns just the registered tool names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
