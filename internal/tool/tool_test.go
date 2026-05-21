package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeTool struct{ name string }

func (f fakeTool) Name() string                                                 { return f.name }
func (f fakeTool) Description() string                                          { return "fake" }
func (f fakeTool) Schema() json.RawMessage                                      { return json.RawMessage(`{"type":"object"}`) }
func (f fakeTool) Invoke(context.Context, json.RawMessage) (Result, error)      { return Result{Output: "ok"}, nil }

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(fakeTool{name: "a"}); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("a")
	if !ok || got.Name() != "a" {
		t.Fatalf("Get(a) failed")
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get(missing) should have failed")
	}
}

func TestRegistry_DuplicateError(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(fakeTool{name: "a"})
	if err := r.Register(fakeTool{name: "a"}); err == nil {
		t.Fatal("duplicate registration should error")
	}
}

func TestRegistry_NilOrEmptyName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("nil tool should error")
	}
	if err := r.Register(fakeTool{name: ""}); err == nil {
		t.Fatal("empty name should error")
	}
}

func TestRegistry_ListSorted(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"z", "a", "m"} {
		_ = r.Register(fakeTool{name: n})
	}
	names := r.Names()
	if len(names) != 3 || names[0] != "a" || names[1] != "m" || names[2] != "z" {
		t.Fatalf("Names not sorted: %+v", names)
	}
}

func TestRegistry_Subset(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"a", "b", "c"} {
		_ = r.Register(fakeTool{name: n})
	}
	sub := r.Subset([]string{"a", "c", "missing"})
	if names := sub.Names(); len(names) != 2 || names[0] != "a" || names[1] != "c" {
		t.Fatalf("Subset names: %+v", names)
	}
	full := r.Subset(nil)
	if len(full.Names()) != 3 {
		t.Fatalf("empty allow should return full registry; got %d", len(full.Names()))
	}
}

func TestShellTool_RunsEchoAndReportsExit(t *testing.T) {
	sh := NewShellTool()
	ctx := context.Background()
	res, err := sh.Invoke(ctx, json.RawMessage(`{"command":"echo hello-from-shell-tool"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "hello-from-shell-tool") {
		t.Fatalf("output missing echo: %q", res.Output)
	}
	if !strings.Contains(res.Output, "exit=0") {
		t.Fatalf("output missing exit=0: %q", res.Output)
	}
}

func TestShellTool_CapturesNonzeroExit(t *testing.T) {
	sh := NewShellTool()
	res, err := sh.Invoke(context.Background(), json.RawMessage(`{"command":"exit 7"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "exit=7") {
		t.Fatalf("expected exit=7, got %q", res.Output)
	}
}

func TestShellTool_RejectsEmptyCommand(t *testing.T) {
	sh := NewShellTool()
	if _, err := sh.Invoke(context.Background(), json.RawMessage(`{"command":""}`)); err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestShellTool_RejectsBadJSON(t *testing.T) {
	sh := NewShellTool()
	if _, err := sh.Invoke(context.Background(), json.RawMessage(`not json`)); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestShellTool_TruncatesLargeOutput(t *testing.T) {
	sh := NewShellTool()
	sh.MaxOutputBytes = 100
	res, err := sh.Invoke(context.Background(),
		json.RawMessage(`{"command":"yes | head -n 1000"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("expected Truncated=true on large output")
	}
}

func TestShellTool_TimeoutKills(t *testing.T) {
	sh := NewShellTool()
	res, err := sh.Invoke(context.Background(),
		json.RawMessage(`{"command":"sleep 10","timeout_seconds":1}`))
	if err != nil {
		t.Fatalf("invoke returned error: %v", err)
	}
	if !strings.Contains(res.Output, "exit=") {
		t.Fatalf("expected exit code line, got %q", res.Output)
	}
}

// sanity-check that Tool interface is wide enough
var _ Tool = (*ShellTool)(nil)
var _ = errors.New
