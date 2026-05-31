// Copyright 2026 Zibby Lab. Apache-2.0.

package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpClient is the streamable-HTTP transport. Each MCP method is one
// POST /mcp with Authorization: Bearer <token>. We do NOT keep an SSE
// stream open — server-initiated notifications aren't used yet.
type httpClient struct {
	cfg    Config
	hc     *http.Client
	ids    idGen
	closed bool
}

func newHTTPClient(cfg Config) *httpClient {
	return &httpClient{
		cfg: cfg,
		hc: &http.Client{
			// Generous on the call side — tool calls may be slow. The
			// daemon's per-task TaskTimeout still caps the overall run.
			Timeout: 90 * time.Second,
		},
	}
}

func (c *httpClient) Name() string                    { return c.cfg.Name }
func (c *httpClient) Start(ctx context.Context) error { return nil }
func (c *httpClient) Close() error                    { c.closed = true; return nil }

func (c *httpClient) Initialize(ctx context.Context) error {
	_, err := c.call(ctx, "initialize", initializeParams(c.cfg.Name))
	if err != nil {
		return err
	}
	// Spec wants a follow-up `notifications/initialized` (no response). Our
	// own server tolerates skipping it, but most third-party servers expect
	// it — send it best-effort.
	_, _ = c.call(ctx, "notifications/initialized", nil)
	return nil
}

func (c *httpClient) ListTools(ctx context.Context) ([]ToolDef, error) {
	raw, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var wire struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/list: %w", err)
	}
	return wire.Tools, nil
}

func (c *httpClient) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	raw, err := c.call(ctx, "tools/call", callToolParams(name, args))
	if err != nil {
		return CallResult{}, err
	}
	return flattenContent(raw)
}

// call is the one HTTP round-trip helper. Encodes a JSON-RPC request, POSTs
// to cfg.URL with bearer auth, decodes the response, and surfaces either
// the result blob or a typed rpcError. Non-200 statuses are surfaced with
// the body included so the caller's logs show what the remote server
// actually said (4xx auth issues are the most common case).
func (c *httpClient) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if c.closed {
		return nil, fmt.Errorf("mcpclient: %s: closed", c.cfg.Name)
	}
	id := c.ids.next()
	reqBody, _ := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.cfg.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: %s: %s: http: %w", c.cfg.Name, method, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("mcpclient: %s: read body: %w", c.cfg.Name, err)
	}
	// Notifications (no response expected): some servers return 202 with no
	// body. Treat that as success with empty result.
	if method == "notifications/initialized" {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcpclient: %s: %s: http %d: %s", c.cfg.Name, method, resp.StatusCode, truncate(string(body), 400))
	}
	var rpcResp rpcResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("mcpclient: %s: decode response: %w (body=%s)", c.cfg.Name, err, truncate(string(body), 400))
	}
	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}
	return rpcResp.Result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
