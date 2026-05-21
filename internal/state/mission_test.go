package state

import (
	"context"
	"strings"
	"testing"
)

func TestGetMission_FreshInstall_ReturnsEmpty(t *testing.T) {
	s := openTest(t)
	m, err := s.GetMission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Statement != "" {
		t.Errorf("Statement = %q, want empty", m.Statement)
	}
	if len(m.Facts) != 0 {
		t.Errorf("Facts = %d, want 0", len(m.Facts))
	}
}

func TestSetStatement_PersistsAndAppendsEvent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const stmt = "I steward the OpenDesign instance. Upgrades require dry-run first."
	if err := s.SetStatement(ctx, stmt); err != nil {
		t.Fatal(err)
	}
	m, _ := s.GetMission(ctx)
	if m.Statement != stmt {
		t.Fatalf("Statement = %q", m.Statement)
	}
	events, _ := s.EventsSince(ctx, 0, 20)
	seen := false
	for _, e := range events {
		if e.Type == "mission.statement_set" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("mission.statement_set event not appended")
	}
}

func TestAddFact_AppendsInOrder(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for _, msg := range []string{"first thing learned", "second thing", "third thing"} {
		if _, err := s.AddFact(ctx, "auto", msg); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := s.GetMission(ctx)
	if len(m.Facts) != 3 {
		t.Fatalf("Facts len = %d, want 3", len(m.Facts))
	}
	if m.Facts[0].Fact != "first thing learned" || m.Facts[2].Fact != "third thing" {
		t.Fatalf("Facts order wrong: %+v", m.Facts)
	}
}

func TestAddFact_RejectsEmptyText(t *testing.T) {
	s := openTest(t)
	if _, err := s.AddFact(context.Background(), "auto", "   "); err == nil {
		t.Fatal("expected error for empty fact")
	}
}

func TestAddFact_DefaultsSourceToAuto(t *testing.T) {
	s := openTest(t)
	facts, err := s.AddFact(context.Background(), "", "something happened")
	if err != nil {
		t.Fatal(err)
	}
	if facts[0].Source != "auto" {
		t.Fatalf("Source = %q, want auto", facts[0].Source)
	}
}

func TestAddFact_CapsAtMaxFacts(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// Push 10 over the cap so we exercise the drop logic deterministically.
	for i := 0; i < MaxFacts+10; i++ {
		text := "f" + string(rune('A'+(i%26)))
		// Make sure they're all unique enough to spot ordering bugs.
		text += " " + string(rune('a'+(i%26)))
		if _, err := s.AddFact(ctx, "auto", text); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := s.GetMission(ctx)
	if len(m.Facts) != MaxFacts {
		t.Fatalf("Facts len = %d, want %d", len(m.Facts), MaxFacts)
	}
	// The first fact in the list should be the (10+1)th we inserted — older
	// ones must have been dropped.
	if !strings.HasPrefix(m.Facts[0].Fact, "fK") {
		// If MaxFacts changes this assertion needs to follow it. Compute
		// the expected leading character from the formula above.
		i := 10 // we dropped the first 10
		expectedFirst := "f" + string(rune('A'+(i%26)))
		if !strings.HasPrefix(m.Facts[0].Fact, expectedFirst) {
			t.Fatalf("oldest remaining fact = %q, want prefix %q (MaxFacts=%d)",
				m.Facts[0].Fact, expectedFirst, MaxFacts)
		}
	}
}

func TestAddFact_AppendsEvent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if _, err := s.AddFact(ctx, "user", "kept here for replication test"); err != nil {
		t.Fatal(err)
	}
	events, _ := s.EventsSince(ctx, 0, 20)
	count := 0
	for _, e := range events {
		if e.Type == "mission.fact_added" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("mission.fact_added events = %d, want 1", count)
	}
}

func TestMission_ConcurrentAddFactSafe(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// Spam adds from multiple goroutines. AddFact's read-modify-write must
	// stay correct under contention thanks to the store-level mutex + the
	// SQLite transaction.
	const N = 20
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			_, err := s.AddFact(ctx, "auto", "concurrent")
			errs <- err
		}(i)
	}
	for i := 0; i < N; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent AddFact %d: %v", i, err)
		}
	}
	m, _ := s.GetMission(ctx)
	if len(m.Facts) != N {
		t.Fatalf("after %d concurrent adds, len(Facts) = %d", N, len(m.Facts))
	}
}
