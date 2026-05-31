// Copyright 2026 Zibby Lab. Apache-2.0.

package mcpclient

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBoot_SkipsBadClientButKeepsGood asserts that one misconfigured
// integration cannot prevent the daemon from booting. The good HTTP
// client comes back wired; the bad transport is skipped with a Warn log.
func TestBoot_SkipsBadClientButKeepsGood(t *testing.T) {
	fake := &fakeMCPServer{tools: []ToolDef{{Name: "ping"}}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	mgr, started, err := Boot(context.Background(), []Config{
		{Name: "broken", Transport: "smtp"}, // bad transport — should be skipped
		{Name: "good", Transport: TransportHTTP, URL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if len(started) != 1 || started[0].Client.Name() != "good" {
		t.Fatalf("expected only 'good' client; got %+v", started)
	}
	if len(started[0].Tools) != 1 {
		t.Errorf("tools/list result lost: %+v", started[0].Tools)
	}
}

// TestBoot_HTTPServerDown_Skipped exercises the "Initialize failed →
// don't register the client" path so a temporary outage of an MCP
// endpoint doesn't prevent the daemon from starting.
func TestBoot_HTTPServerDown_Skipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	mgr, started, err := Boot(context.Background(), []Config{
		{Name: "outage", Transport: TransportHTTP, URL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if len(started) != 0 {
		t.Errorf("client with failing Initialize should be skipped; got %d", len(started))
	}
}

// TestBoot_EmptyClients ensures a daemon with no mcp_clients in its
// config boots cleanly through Boot, returning an empty list. This is
// the back-compat path for v0.2.x configs.
func TestBoot_EmptyClients(t *testing.T) {
	mgr, started, err := Boot(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if len(started) != 0 {
		t.Errorf("expected no started clients; got %+v", started)
	}
}
