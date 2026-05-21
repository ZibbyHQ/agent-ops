package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
