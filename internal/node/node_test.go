package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrInit_FirstRunGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	n, err := LoadOrInit(dir)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	if n.ID() == "" {
		t.Fatal("expected non-empty ID on first run")
	}
	if n.Role() != RoleSolo {
		t.Fatalf("expected RoleSolo, got %q", n.Role())
	}
	if !validID(string(n.ID())) {
		t.Fatalf("generated id %q is not valid hex", n.ID())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "node.id"))
	if err != nil {
		t.Fatalf("read persisted id: %v", err)
	}
	if string(raw) != string(n.ID()) {
		t.Fatalf("persisted id mismatch: file=%q in-memory=%q", raw, n.ID())
	}
}

func TestLoadOrInit_SecondCallReusesID(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrInit(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrInit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != second.ID() {
		t.Fatalf("expected stable id across restarts, got %q vs %q", first.ID(), second.ID())
	}
}

func TestLoadOrInit_RejectsCorruptID(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node.id"), []byte("not-a-valid-hex-id"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrInit(dir); err == nil {
		t.Fatal("expected error on corrupt id, got nil")
	}
}

func TestSolo_TouchUpdatesHeartbeat(t *testing.T) {
	n, err := LoadOrInit(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := n.Heartbeat()
	// Synchronously call Touch; heartbeat must advance or stay equal.
	n.Touch()
	after := n.Heartbeat()
	if after.Before(before) {
		t.Fatalf("heartbeat went backwards: before=%v after=%v", before, after)
	}
}
