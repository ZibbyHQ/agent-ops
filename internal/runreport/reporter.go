// Copyright 2026 Zibby Lab. Apache-2.0.

// Package runreport pushes a structured record of each finished task run to
// the Zibby control plane, so the "Agent activity" UI can render runs from a
// durable DynamoDB table instead of grepping CloudWatch logs.
//
// The push is outbound (like register-port / zibby_workflow), so it doesn't
// depend on the ALB `/_zibby_ops/*` proxy path. It is strictly
// fire-and-forget: the run's terminal state is already persisted in SQLite
// before we report, so a failed/blocked report must NEVER fail or stall the
// run. On error we log and move on.
package runreport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// RunRecord is the wire shape POSTed to the backend. Field names + JSON tags
// match the `POST /apps/:instanceId/runs` contract in AGENT_RUNS_PLAN.md
// verbatim — keep them in lockstep with `exports.reportRun` in
// backend/src/handlers/apps.js.
type RunRecord struct {
	RunID        string `json:"runId"`
	TaskName     string `json:"taskName"`
	Trigger      string `json:"trigger"`
	Status       string `json:"status"`
	StartedAt    string `json:"startedAt"` // ISO-8601
	EndedAt      string `json:"endedAt"`   // ISO-8601
	ToolCalls    int    `json:"toolCalls"`
	NumTurns     int    `json:"numTurns"`
	CostUSDMicro int64  `json:"costUsdMicro"`
	Model        string `json:"model"`
	SystemPrompt string `json:"systemPrompt"`
	UserPrompt   string `json:"userPrompt"`
	Result       string `json:"result"`
	Summary      string `json:"summary"`
	Error        string `json:"error"`
}

// RunReporter reports a finished run to a sink (the backend, in prod).
type RunReporter interface {
	Report(ctx context.Context, rec RunRecord) error
}

// HTTPReporter POSTs each finished run to
// ${ZIBBY_API_BASE_URL}/apps/${INSTANCE_ID}/runs with
// `Authorization: Bearer ${AGENT_OPS_TOKEN}`.
//
// It is a no-op (Report returns nil immediately) when ANY of
// ZIBBY_API_BASE_URL / INSTANCE_ID / AGENT_OPS_TOKEN is unset — mirroring the
// no-op-when-unconfigured pattern in internal/tool/zibby_workflow.go so a
// non-Zibby (or half-wired) deployment never accidentally calls a wrong API.
type HTTPReporter struct {
	// HTTPClient is used for the outbound POST. Override in tests.
	HTTPClient *http.Client

	// Logger receives fire-and-forget failure lines. Defaults to slog.Default.
	Logger *slog.Logger

	// Env reads an environment variable. Override in tests so each test can
	// stand up its own ZIBBY_API_BASE_URL / INSTANCE_ID / AGENT_OPS_TOKEN
	// without leaking into process env.
	Env func(string) string
}

// NewHTTPReporter returns an HTTPReporter with sane defaults. The 10s client
// timeout bounds a single attempt; Report layers retries on top of it.
func NewHTTPReporter() *HTTPReporter {
	return &HTTPReporter{
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		Logger:     slog.Default(),
		Env:        os.Getenv,
	}
}

const (
	// maxRetries is the number of additional attempts after the first.
	maxRetries = 2
	// baseBackoff is the first inter-attempt sleep; it doubles each retry.
	baseBackoff = 500 * time.Millisecond
)

// Report POSTs rec to the backend. It is fire-and-forget: it logs on failure
// but always returns nil, because the run's terminal state is already durably
// persisted before this is ever called and a reporting failure must not be
// treated as a run failure.
func (r *HTTPReporter) Report(ctx context.Context, rec RunRecord) error {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}

	env := r.Env
	if env == nil {
		env = os.Getenv
	}
	baseURL := strings.TrimSpace(env("ZIBBY_API_BASE_URL"))
	instanceID := strings.TrimSpace(env("INSTANCE_ID"))
	token := strings.TrimSpace(env("AGENT_OPS_TOKEN"))

	// No-op when unconfigured — same gate as zibby_workflow. Silent: a
	// non-Zibby deployment reporting nothing is the expected steady state,
	// not a warning.
	if baseURL == "" || instanceID == "" || token == "" {
		return nil
	}

	endpoint := strings.TrimRight(baseURL, "/") +
		"/apps/" + url.PathEscape(instanceID) + "/runs"

	body, err := json.Marshal(rec)
	if err != nil {
		// Should be impossible for a struct of strings/ints; log and bail.
		logger.Warn("runreport: marshal record", "error", err, "run_id", rec.RunID)
		return nil
	}

	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Backoff, but bail early if the caller's context is done.
			backoff := baseBackoff << (attempt - 1)
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				goto done
			case <-time.After(backoff):
			}
		}

		err := r.attempt(ctx, client, endpoint, token, body)
		if err == nil {
			return nil // success
		}
		lastErr = err
	}

done:
	// Fire-and-forget: log the final failure, but never propagate it. The
	// run already succeeded/failed on its own terms; reporting is best-effort.
	logger.Warn("runreport: failed to report run after retries",
		"error", lastErr, "run_id", rec.RunID, "endpoint", endpoint)
	return nil
}

// attempt performs one POST and returns an error on transport failure or any
// non-2xx status (so the caller can retry).
func (r *HTTPReporter) attempt(ctx context.Context, client *http.Client, endpoint, token string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// AGENT_OPS_TOKEN == the instance's bridgeToken; the backend hashes the
	// bearer and timing-safe-compares it against the apps row.
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "agent-ops/runreport")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("unexpected status %d", resp.StatusCode)
}

// Ensure HTTPReporter satisfies the interface.
var _ RunReporter = (*HTTPReporter)(nil)
