// Copyright 2026 Zibby Lab. Apache-2.0.

package disku

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Measure should return >0 bytes for a directory containing a non-empty
// file. We can't assert the exact value (du counts blocks + inodes
// differently on different filesystems), but a known-content file's
// reported size should be >= the file's data size.
func TestMeasure_ReturnsNonZeroForPopulatedDir(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("hello world, this is a non-trivial payload that's at least 50 bytes long for the test.")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), payload, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := Measure(context.Background(), dir)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if got < int64(len(payload)) {
		t.Fatalf("expected du >= %d bytes, got %d", len(payload), got)
	}
}

func TestMeasure_MissingDirReturnsError(t *testing.T) {
	if _, err := Measure(context.Background(), "/nonexistent/path/that/should/not/exist-xyzzy"); err == nil {
		t.Fatal("expected error for missing dir, got nil")
	}
}
