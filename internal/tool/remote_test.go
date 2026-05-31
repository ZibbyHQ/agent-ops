// Copyright 2026 Zibby Lab. Apache-2.0.

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeInvoker struct {
	name    string
	gotName string
	gotArgs json.RawMessage
	result  RemoteCallResult
	err     error
}

func (f *fakeInvoker) Name() string { return f.name }
func (f *fakeInvoker) CallTool(_ context.Context, name string, args json.RawMessage) (RemoteCallResult, error) {
	f.gotName = name
	f.gotArgs = args
	return f.result, f.err
}

// TestRemoteToolAdapter_NameSchema pins the contract: local name is
// {clientName}_{remoteName}, the schema is passed through unchanged.
func TestRemoteToolAdapter_NameSchema(t *testing.T) {
	a := NewRemoteToolAdapter("zibby", "trigger_workflow", "fire a workflow",
		json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
		&fakeInvoker{name: "zibby"})
	if a.Name() != "zibby_trigger_workflow" {
		t.Errorf("Name = %q", a.Name())
	}
	if a.RemoteClientName() != "zibby" || a.RemoteToolName() != "trigger_workflow" {
		t.Errorf("client/tool split wrong: %s / %s", a.RemoteClientName(), a.RemoteToolName())
	}
	if !strings.Contains(string(a.Schema()), `"id"`) {
		t.Errorf("schema not passed through: %s", a.Schema())
	}
}

// TestRemoteToolAdapter_NoDoublePrefix — the Zibby Remote MCP advertises
// every tool already namespaced (`zibby_list_workflows`); registering
// under client name `zibby` must NOT produce `zibby_zibby_list_workflows`.
// Single-prefix is the canonical form.
func TestRemoteToolAdapter_NoDoublePrefix(t *testing.T) {
	cases := []struct {
		clientName string
		remoteName string
		wantName   string
		desc       string
	}{
		{"zibby", "zibby_list_workflows", "zibby_list_workflows",
			"remote name already prefixed → single prefix"},
		{"zibby", "zibby_create_pat", "zibby_create_pat",
			"remote name already prefixed → single prefix"},
		{"zibby", "zibby_list_marketplace_trending", "zibby_list_marketplace_trending",
			"deeply suffixed remote name still collapses"},
		{"zibby", "trigger_workflow", "zibby_trigger_workflow",
			"un-prefixed remote name → join with underscore"},
		{"gh", "lint_repo", "gh_lint_repo",
			"different client+remote → normal join"},
		{"zibby", "zibby", "zibby_zibby",
			"exact-equal name (no trailing underscore in remote) → still joined"},
		{"zibby", "zibbylike_tool", "zibby_zibbylike_tool",
			"prefix-without-underscore is NOT a match → normal join"},
		{"", "anything", "_anything",
			"empty clientName: empty-string + '_' + remote (existing behavior, unchanged)"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			a := NewRemoteToolAdapter(tc.clientName, tc.remoteName, "", nil, &fakeInvoker{})
			if a.Name() != tc.wantName {
				t.Errorf("client=%q remote=%q: Name = %q, want %q",
					tc.clientName, tc.remoteName, a.Name(), tc.wantName)
			}
			// RemoteToolName / RemoteClientName must still report the
			// pre-join components — Invoke() forwards remoteName to the
			// MCP server, and the server expects its own original name.
			if a.RemoteClientName() != tc.clientName {
				t.Errorf("RemoteClientName = %q, want %q", a.RemoteClientName(), tc.clientName)
			}
			if a.RemoteToolName() != tc.remoteName {
				t.Errorf("RemoteToolName = %q, want %q", a.RemoteToolName(), tc.remoteName)
			}
		})
	}
}

// TestRemoteToolAdapter_EmptySchema_FallsBackToObject — empty/missing
// inputSchema must still produce a valid JSON-schema so the local LLM
// driver doesn't choke on the registered tool.
func TestRemoteToolAdapter_EmptySchema_FallsBackToObject(t *testing.T) {
	a := NewRemoteToolAdapter("c", "t", "d", nil, &fakeInvoker{})
	if !strings.Contains(string(a.Schema()), `"type":"object"`) {
		t.Errorf("fallback schema missing type=object: %s", a.Schema())
	}
}

func TestRemoteToolAdapter_InvokeForwards(t *testing.T) {
	fi := &fakeInvoker{result: RemoteCallResult{Text: "hello world"}}
	a := NewRemoteToolAdapter("c", "echo", "", nil, fi)
	res, err := a.Invoke(context.Background(), json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "hello world" {
		t.Errorf("output = %q", res.Output)
	}
	if fi.gotName != "echo" || string(fi.gotArgs) != `{"x":1}` {
		t.Errorf("forward args wrong: name=%s args=%s", fi.gotName, fi.gotArgs)
	}
}

func TestRemoteToolAdapter_IsErrorBecomesGoError(t *testing.T) {
	a := NewRemoteToolAdapter("c", "t", "", nil,
		&fakeInvoker{result: RemoteCallResult{Text: "remote said no", IsError: true}})
	res, err := a.Invoke(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for isError result")
	}
	if !strings.Contains(err.Error(), "remote said no") {
		t.Errorf("error doesn't include remote text: %v", err)
	}
	if res.Output != "remote said no" {
		t.Errorf("output should still surface text: %q", res.Output)
	}
}

func TestRemoteToolAdapter_InvokerErrorPropagates(t *testing.T) {
	a := NewRemoteToolAdapter("c", "t", "", nil, &fakeInvoker{err: errors.New("network down")})
	_, err := a.Invoke(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "network down") {
		t.Errorf("err = %v", err)
	}
}
