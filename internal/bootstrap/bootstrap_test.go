package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZibbyHQ/agent-ops/internal/config"
	"github.com/ZibbyHQ/agent-ops/internal/state"
)

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "plain JSON",
			in:   `{"pass": true, "evidence": "ok"}`,
			want: `{"pass": true, "evidence": "ok"}`,
			ok:   true,
		},
		{
			name: "leading prose",
			in:   "Sure! Here is the JSON:\n{\"pass\": false, \"fail_reason\": \"port closed\"}\n",
			want: `{"pass": false, "fail_reason": "port closed"}`,
			ok:   true,
		},
		{
			name: "nested objects",
			in:   `prose {"pass": true, "nested": {"k": "v"}, "ok": 1} trailing`,
			want: `{"pass": true, "nested": {"k": "v"}, "ok": 1}`,
			ok:   true,
		},
		{
			name: "braces inside strings ignored",
			in:   `{"evidence": "saw } and { in output", "pass": true}`,
			want: `{"evidence": "saw } and { in output", "pass": true}`,
			ok:   true,
		},
		{
			name: "no JSON",
			in:   `the agent just said yes, looks good`,
			ok:   false,
		},
		{
			name: "unbalanced",
			in:   `{"pass": true`,
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractJSONObject(c.in)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && got != c.want {
				t.Fatalf("got %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestParseVerifierJSON(t *testing.T) {
	t.Run("pass true with prose around it", func(t *testing.T) {
		r, err := parseVerifierJSON(`Sure: {"pass": true, "evidence": "ps shows pid 4231, curl 200"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Pass {
			t.Fatal("expected pass=true")
		}
		if !strings.Contains(r.Evidence, "pid 4231") {
			t.Fatalf("evidence = %q", r.Evidence)
		}
	})
	t.Run("pass false with fail_reason", func(t *testing.T) {
		r, err := parseVerifierJSON(`{"pass": false, "evidence": "no process", "fail_reason": "n8n binary missing"}`)
		if err != nil {
			t.Fatal(err)
		}
		if r.Pass {
			t.Fatal("expected pass=false")
		}
		if r.FailReason != "n8n binary missing" {
			t.Fatalf("fail_reason = %q", r.FailReason)
		}
	})
	t.Run("no JSON at all is an error", func(t *testing.T) {
		_, err := parseVerifierJSON("the agent forgot to emit JSON")
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("malformed JSON is an error", func(t *testing.T) {
		_, err := parseVerifierJSON(`{"pass": notbool}`)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestEnsureToken_PreferEnvWhenSet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MY_TOKEN", "from-env")
	tok, err := EnsureToken(dir, "MY_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "from-env" {
		t.Fatalf("token = %q", tok)
	}
	// File must NOT be written when env was used (it would clash with a
	// later EnsureToken call that lacks the env var).
	if _, err := os.Stat(filepath.Join(dir, "mcp.token")); err == nil {
		t.Fatal("file should not be written when env wins")
	}
}

func TestEnsureToken_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	tok1, err := EnsureToken(dir, "UNSET_VAR")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok1, "ao_") {
		t.Fatalf("token missing prefix: %q", tok1)
	}
	if len(tok1) < 32 {
		t.Fatalf("token too short: %q", tok1)
	}
	// Second call must reuse the persisted file.
	tok2, err := EnsureToken(dir, "UNSET_VAR")
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != tok2 {
		t.Fatalf("token not stable across calls: %q vs %q", tok1, tok2)
	}
}

func TestEnsureToken_ReadsPersistedFile(t *testing.T) {
	dir := t.TempDir()
	prePersist := "ao_preplaced"
	if err := os.WriteFile(filepath.Join(dir, "mcp.token"), []byte(prePersist), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := EnsureToken(dir, "UNSET_VAR")
	if err != nil {
		t.Fatal(err)
	}
	if tok != prePersist {
		t.Fatalf("expected pre-placed token, got %q", tok)
	}
}

// TestRunScriptBootstrap_ExecutesAndStreams confirms the script-mode path
// runs verbatim bash, captures the exit code, and writes a `script_ok` fact.
// This is the core v0.1.12 change — the LLM-free install path.
func TestRunScriptBootstrap_ExecutesAndStreams(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Use a marker file to confirm bash actually ran (vs `cmd.Start` lying
	// about success); script also echoes so the stdout-streaming path is
	// exercised. With a short timeout to keep CI fast.
	marker := filepath.Join(dir, "script-ran")
	t.Setenv("AGENT_OPS_BOOTSTRAP_SCRIPT",
		"echo hello from script; touch "+marker+"; exit 0")
	t.Setenv("AGENT_OPS_BOOTSTRAP_TIMEOUT", "5s")
	cfg := &config.Config{StateDir: dir}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runScriptBootstrap(ctx, cfg, store); err != nil {
		t.Fatalf("runScriptBootstrap: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file missing — script did not actually run: %v", err)
	}
	// Confirm a bootstrap fact was added (returned list comes from AddFact —
	// we add one in the OK path).
	got, _ := store.AddFact(ctx, "bootstrap", "test-probe")
	var sawScriptOK bool
	for _, f := range got {
		if strings.Contains(f.Fact, "script_ok") {
			sawScriptOK = true
			break
		}
	}
	if !sawScriptOK {
		t.Fatalf("expected script_ok fact among %d facts", len(got))
	}
}

func TestRunScriptBootstrap_NonZeroExitReturnsError(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	t.Setenv("AGENT_OPS_BOOTSTRAP_SCRIPT", "echo about to fail; exit 42")
	t.Setenv("AGENT_OPS_BOOTSTRAP_TIMEOUT", "5s")
	cfg := &config.Config{StateDir: dir}

	err = runScriptBootstrap(context.Background(), cfg, store)
	if err == nil {
		t.Fatal("expected error for exit 42")
	}
	if !strings.Contains(err.Error(), "42") {
		t.Fatalf("error should mention exit code 42, got %q", err)
	}
}

func TestRunScriptBootstrap_EmptyScriptIsError(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	t.Setenv("AGENT_OPS_BOOTSTRAP_SCRIPT", "")
	cfg := &config.Config{StateDir: dir}

	err = runScriptBootstrap(context.Background(), cfg, store)
	if err == nil {
		t.Fatal("expected error for empty script")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error should mention empty, got %q", err)
	}
}

func TestRunScriptBootstrap_TimeoutKills(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// `sleep 60` with a 1s timeout — bash -c should be killed.
	t.Setenv("AGENT_OPS_BOOTSTRAP_SCRIPT", "sleep 60")
	t.Setenv("AGENT_OPS_BOOTSTRAP_TIMEOUT", "1s")
	cfg := &config.Config{StateDir: dir}

	start := time.Now()
	err = runScriptBootstrap(context.Background(), cfg, store)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error should mention timeout, got %q", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("kill took too long: %v", elapsed)
	}
}
