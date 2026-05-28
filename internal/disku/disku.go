// Copyright 2026 Zibby Lab. Apache-2.0.

// Package disku periodically measures the on-disk size of the state
// directory and emits a structured log line. The Zibby control plane
// reads those lines from CloudWatch to populate per-instance storage
// gauges in the UI — AWS does not expose per-EFS-AccessPoint usage as
// a CloudWatch metric, so emitting it ourselves is the cheapest way
// to get per-tenant numbers.
package disku

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Start launches a goroutine that ticks every `interval` and logs the
// disk usage of `dir`. Goroutine exits when ctx is cancelled. Returns
// immediately. Designed to be fire-and-forget alongside the daemon's
// other background loops.
func Start(ctx context.Context, logger *slog.Logger, dir string, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	go func() {
		emit(ctx, logger, dir)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				emit(ctx, logger, dir)
			}
		}
	}()
}

// emit measures + logs once. A single failed measurement is a Warn —
// transient `du` errors (a temp file vanishing mid-walk) shouldn't
// page anyone.
func emit(ctx context.Context, logger *slog.Logger, dir string) {
	// 30s cap so a wedged EFS mount can't stall the goroutine forever.
	mctx, cancel := withTimeout(ctx, 30*time.Second)
	defer cancel()
	n, err := Measure(mctx, dir)
	if err != nil {
		logger.Warn("efs_usage: du failed", "path", dir, "error", err.Error())
		return
	}
	logger.Info("efs_usage", "path", dir, "bytes", n)
}

// Measure walks `dir` and sums the size of every regular file. Returns
// (0, error) if dir doesn't exist or can't be opened at all; transient
// errors on individual children (a file vanishing mid-walk, a denied
// subdir) are ignored — we want a "best-effort total", not a fatal
// failure for an unreadable temp file. Symlinks are NOT followed, so
// a symlink to / won't blow up the count.
//
// Go-native (filepath.WalkDir) rather than shelling to du because BSD
// `du` (macOS dev box) lacks `-b`; the walker is portable + about as
// fast on the <10GB EFS volumes managed apps use.
func Measure(ctx context.Context, dir string) (int64, error) {
	if _, err := os.Stat(dir); err != nil {
		return 0, err
	}
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			// File disappeared between Readdir and Stat — common on
			// EFS during writes. Skip and keep totaling.
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return 0, err
	}
	return total, nil
}

// timeout enforces a wall-clock cap on Measure. Wraps the caller's ctx
// so cancellation from above (daemon shutdown) still propagates.
func withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
