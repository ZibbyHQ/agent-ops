// Copyright 2026 Zibby Lab. Apache-2.0.

package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Mission is the per-instance "what am I stewarding" record.
//
// One row per daemon (singleton, id=1). Statement is the natural-language
// charter set by the user (or the bootstrap task). Facts is an append-only
// log of things the agent has learned — bounded so it doesn't grow unbounded.
//
// At task-run time, Runner reads the current Mission and prepends it to the
// driver's system prompt so every iteration carries instance self-knowledge.
type Mission struct {
	Statement string    `json:"statement"`
	Facts     []Fact    `json:"facts"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Fact is one observation the agent (or the user) has captured.
type Fact struct {
	TS     time.Time `json:"ts"`
	Source string    `json:"source"` // "user" | "bootstrap" | "auto" | "task:<name>"
	Fact   string    `json:"fact"`
}

// MaxFacts caps the in-memory + persisted facts list. When AddFact would push
// the count past this, the oldest entries are dropped. Bound exists so we
// never blow past DDB's / SQLite's row-size budgets and so the LLM prompt
// stays bounded — a runaway agent writing 10k "fixed it" facts shouldn't
// blow up its own context window.
const MaxFacts = 100

const missionSchema = `
CREATE TABLE IF NOT EXISTS instance_mission (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    statement  TEXT NOT NULL DEFAULT '',
    facts_json TEXT NOT NULL DEFAULT '[]',
    updated_at INTEGER NOT NULL
);
`

// migrateMission is called by Open() — splits out so callers can read the
// shape without scrolling through state.go.
func migrateMission(db *sql.DB) error {
	if _, err := db.Exec(missionSchema); err != nil {
		return fmt.Errorf("state: migrate mission: %w", err)
	}
	// Seed singleton row idempotently.
	_, err := db.Exec(
		`INSERT OR IGNORE INTO instance_mission(id, statement, facts_json, updated_at)
		 VALUES(1, '', '[]', ?)`,
		time.Now().UTC().UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("state: seed mission: %w", err)
	}
	return nil
}

// GetMission returns the current mission. Always returns a non-nil record;
// fresh installs see an empty Statement + empty Facts.
func (s *Store) GetMission(ctx context.Context) (Mission, error) {
	var (
		statement string
		factsJSON string
		updated   int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT statement, facts_json, updated_at FROM instance_mission WHERE id = 1`,
	).Scan(&statement, &factsJSON, &updated)
	if err != nil {
		return Mission{}, fmt.Errorf("state.GetMission: %w", err)
	}
	var facts []Fact
	if err := json.Unmarshal([]byte(factsJSON), &facts); err != nil {
		return Mission{}, fmt.Errorf("state.GetMission: parse facts: %w", err)
	}
	return Mission{
		Statement: statement,
		Facts:     facts,
		UpdatedAt: time.Unix(0, updated).UTC(),
	}, nil
}

// SetStatement replaces the mission statement. Empty string is allowed (clears
// the charter). Appends a state event for the future replication log.
func (s *Store) SetStatement(ctx context.Context, statement string) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE instance_mission SET statement = ?, updated_at = ? WHERE id = 1`,
		statement, now.UnixNano(),
	); err != nil {
		return fmt.Errorf("state.SetStatement: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(ts, typ, payload) VALUES(?, 'mission.statement_set', ?)`,
		now.UnixNano(),
		mustJSON(map[string]any{"statement": statement}),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// AddFact appends one fact to the mission's facts list. When over MaxFacts,
// the oldest entries are dropped. Source labels who wrote it ("user",
// "bootstrap", "auto", or "task:<name>"). Returns the resulting bounded list.
func (s *Store) AddFact(ctx context.Context, source, text string) ([]Fact, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("state.AddFact: text required")
	}
	if source == "" {
		source = "auto"
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var factsJSON string
	if err := tx.QueryRowContext(ctx,
		`SELECT facts_json FROM instance_mission WHERE id = 1`,
	).Scan(&factsJSON); err != nil {
		return nil, fmt.Errorf("state.AddFact: read: %w", err)
	}
	var facts []Fact
	if err := json.Unmarshal([]byte(factsJSON), &facts); err != nil {
		return nil, fmt.Errorf("state.AddFact: parse: %w", err)
	}
	facts = append(facts, Fact{TS: now, Source: source, Fact: text})
	if len(facts) > MaxFacts {
		facts = facts[len(facts)-MaxFacts:]
	}
	out, err := json.Marshal(facts)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE instance_mission SET facts_json = ?, updated_at = ? WHERE id = 1`,
		string(out), now.UnixNano(),
	); err != nil {
		return nil, fmt.Errorf("state.AddFact: write: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(ts, typ, payload) VALUES(?, 'mission.fact_added', ?)`,
		now.UnixNano(),
		mustJSON(map[string]any{"source": source, "fact": text}),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return facts, nil
}
