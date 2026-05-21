package state

import (
	"context"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAppendAndEventsSince(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	e1, err := s.Append(ctx, "test.one", map[string]string{"k": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.Append(ctx, "test.two", map[string]string{"k": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if e1.Seq >= e2.Seq {
		t.Fatalf("seq not monotonic: %d then %d", e1.Seq, e2.Seq)
	}
	events, err := s.EventsSince(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if events[0].Type != "test.one" || events[1].Type != "test.two" {
		t.Fatalf("bad order: %+v", events)
	}
}

func TestUpsertAndGetTask(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	in := Task{
		Name:    "weekly",
		Cron:    "0 9 * * 1",
		Prompt:  "do the thing",
		Tools:   []string{"shell", "http"},
		Enabled: true,
	}
	if err := s.UpsertTask(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := s.GetTask(ctx, "weekly")
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != "weekly" || out.Prompt != "do the thing" || !out.Enabled {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
	if len(out.Tools) != 2 || out.Tools[0] != "shell" {
		t.Fatalf("tools roundtrip mismatch: %+v", out.Tools)
	}
	// Update path
	in.Prompt = "do it differently"
	if err := s.UpsertTask(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, _ = s.GetTask(ctx, "weekly")
	if out.Prompt != "do it differently" {
		t.Fatalf("upsert didn't update: %+v", out)
	}
	events, _ := s.EventsSince(ctx, 0, 10)
	upserts := 0
	for _, e := range events {
		if e.Type == "task.upserted" {
			upserts++
		}
	}
	if upserts != 2 {
		t.Fatalf("want 2 task.upserted events, got %d", upserts)
	}
}

func TestRunLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	run := TaskRun{
		ID:        "run-1",
		TaskName:  "weekly",
		Trigger:   "manual",
		StartedAt: time.Now().UTC(),
	}
	if err := s.StartRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("want running, got %q", got.Status)
	}
	if err := s.FinishRun(ctx, "run-1", StatusCompleted, "ok", "", 5); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRun(ctx, "run-1")
	if got.Status != StatusCompleted || got.Summary != "ok" || got.ToolCalls != 5 {
		t.Fatalf("finish didn't take: %+v", got)
	}
	// Double-finish should fail.
	if err := s.FinishRun(ctx, "run-1", StatusCompleted, "ok", "", 5); err == nil {
		t.Fatal("expected error on double-finish")
	}
}

func TestListRuns_FilterAndOrder(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for i, name := range []string{"a", "a", "b"} {
		r := TaskRun{
			ID:        "r" + string(rune('0'+i)),
			TaskName:  name,
			Trigger:   "schedule",
			StartedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}
		if err := s.StartRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := s.ListRuns(ctx, "", 10)
	if len(all) != 3 {
		t.Fatalf("want 3 runs, got %d", len(all))
	}
	// Newest first
	if all[0].StartedAt.Before(all[1].StartedAt) {
		t.Fatalf("not newest-first: %+v", all)
	}
	onlyA, _ := s.ListRuns(ctx, "a", 10)
	if len(onlyA) != 2 {
		t.Fatalf("want 2 runs for task a, got %d", len(onlyA))
	}
}

func TestRunLogs(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_ = s.StartRun(ctx, TaskRun{ID: "r1", TaskName: "t", Trigger: "manual", StartedAt: time.Now().UTC()})
	for i, msg := range []string{"first", "second", "third"} {
		if err := s.AppendRunLog(ctx, "r1", i, "info", msg); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := s.LogsForRun(ctx, "r1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 || logs[0].Message != "first" || logs[2].Message != "third" {
		t.Fatalf("logs wrong: %+v", logs)
	}
}

func TestRunStatusIsTerminal(t *testing.T) {
	for _, s := range []RunStatus{StatusCompleted, StatusFailed, StatusCanceled, StatusTimeout} {
		if !s.IsTerminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	if StatusRunning.IsTerminal() {
		t.Error("running should not be terminal")
	}
}
