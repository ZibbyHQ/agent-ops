// Copyright 2026 Zibby Lab. Apache-2.0.

package runreport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// stubEnv returns an Env func satisfying HTTPReporter.Env. Tests pre-load the
// map with whichever subset of vars they want set.
func stubEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func sampleRecord() RunRecord {
	return RunRecord{
		RunID:        "run-abc123",
		TaskName:     "weekly-upgrade",
		Trigger:      "schedule",
		Status:       "completed",
		StartedAt:    "2026-05-29T08:00:00Z",
		EndedAt:      "2026-05-29T08:01:30Z",
		ToolCalls:    4,
		NumTurns:     4,
		CostUSDMicro: 98765,
		Model:        "claude-haiku",
		SystemPrompt: "you are agent-ops",
		UserPrompt:   "check for upgrades",
		Result:       "no upgrades available",
		Summary:      "no upgrades available",
		Error:        "",
	}
}

func TestHTTPReporter_HappyPath_PostsContract(t *testing.T) {
	var gotPath, gotAuth, gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	rep := NewHTTPReporter()
	rep.Env = stubEnv(map[string]string{
		"ZIBBY_API_BASE_URL": srv.URL,
		"INSTANCE_ID":        "6d789691",
		"AGENT_OPS_TOKEN":    "bridge-secret",
	})

	if err := rep.Report(context.Background(), sampleRecord()); err != nil {
		t.Fatalf("Report returned error: %v", err)
	}

	if gotPath != "/apps/6d789691/runs" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer bridge-secret" {
		t.Fatalf("unexpected Authorization: %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("unexpected Content-Type: %q", gotContentType)
	}

	// Body must carry the exact contract field names.
	var parsed map[string]any
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("body not valid JSON: %v (raw=%s)", err, string(gotBody))
	}
	for _, k := range []string{
		"runId", "taskName", "trigger", "status", "startedAt", "endedAt",
		"toolCalls", "numTurns", "costUsdMicro", "model", "systemPrompt",
		"userPrompt", "result", "summary", "error",
	} {
		if _, ok := parsed[k]; !ok {
			t.Fatalf("body missing contract field %q: %s", k, string(gotBody))
		}
	}
	if parsed["runId"] != "run-abc123" {
		t.Fatalf("runId = %v", parsed["runId"])
	}
	if parsed["costUsdMicro"].(float64) != 98765 {
		t.Fatalf("costUsdMicro = %v", parsed["costUsdMicro"])
	}
}

func TestHTTPReporter_NoOpWhenEnvUnset(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"no base url", map[string]string{"INSTANCE_ID": "i", "AGENT_OPS_TOKEN": "t"}},
		{"no instance id", map[string]string{"ZIBBY_API_BASE_URL": "https://x", "AGENT_OPS_TOKEN": "t"}},
		{"no token", map[string]string{"ZIBBY_API_BASE_URL": "https://x", "INSTANCE_ID": "i"}},
		{"all empty", map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(200)
			}))
			defer srv.Close()

			// If a base URL is configured for the case, point it at the server
			// so a (buggy) non-no-op path would actually hit it.
			if _, ok := tc.env["ZIBBY_API_BASE_URL"]; ok {
				tc.env["ZIBBY_API_BASE_URL"] = srv.URL
			}

			rep := NewHTTPReporter()
			rep.Env = stubEnv(tc.env)
			if err := rep.Report(context.Background(), sampleRecord()); err != nil {
				t.Fatalf("no-op Report should not error; got %v", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Fatalf("expected no HTTP call when unconfigured, got %d", got)
			}
		})
	}
}

func TestHTTPReporter_RetriesThenGivesUp_NeverErrors(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(500) // always fail
	}))
	defer srv.Close()

	rep := NewHTTPReporter()
	rep.Env = stubEnv(map[string]string{
		"ZIBBY_API_BASE_URL": srv.URL,
		"INSTANCE_ID":        "i",
		"AGENT_OPS_TOKEN":    "t",
	})

	// Fire-and-forget: a server that always 500s must NOT surface an error.
	if err := rep.Report(context.Background(), sampleRecord()); err != nil {
		t.Fatalf("Report must never propagate failures; got %v", err)
	}
	// 1 initial + maxRetries attempts.
	if got := atomic.LoadInt32(&hits); got != int32(1+maxRetries) {
		t.Fatalf("expected %d attempts, got %d", 1+maxRetries, got)
	}
}

func TestHTTPReporter_RetrySucceedsOnSecondAttempt(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(503) // first attempt fails
			return
		}
		w.WriteHeader(200) // retry succeeds
	}))
	defer srv.Close()

	rep := NewHTTPReporter()
	rep.Env = stubEnv(map[string]string{
		"ZIBBY_API_BASE_URL": srv.URL,
		"INSTANCE_ID":        "i",
		"AGENT_OPS_TOKEN":    "t",
	})

	if err := rep.Report(context.Background(), sampleRecord()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected 2 attempts (1 fail + 1 success), got %d", got)
	}
}

var _ RunReporter = (*HTTPReporter)(nil)
