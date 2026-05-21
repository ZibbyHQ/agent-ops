// Copyright 2026 Zibby Lab. Apache-2.0.

// Package state owns the daemon's persistent store.
//
// Schema is built around an append-only `events` table that doubles as a
// replication log when clustering arrives. Domain tables (tasks, task_runs)
// are derived views: every mutation is first appended to events, then
// applied to its table inside the same transaction. v1.0's Raft layer can
// ship the same events to followers without touching domain tables.
package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver — works in static binaries
)

// Store is the SQLite-backed persistent state. Safe for concurrent use.
type Store struct {
	db *sql.DB

	mu sync.Mutex // serializes Apply to keep the events table monotone.
}

// Open returns a Store at <stateDir>/state.db. Creates schema on first call.
func Open(stateDir string) (*Store, error) {
	if stateDir == "" {
		return nil, errors.New("state.Open: stateDir required")
	}
	path := filepath.Join(stateDir, "state.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: open: %w", err)
	}
	db.SetMaxOpenConns(8)
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw connection pool for callers that need transactional
// composition (tests, the MCP layer). Use sparingly.
func (s *Store) DB() *sql.DB { return s.db }

// ─── Schema ─────────────────────────────────────────────────────────────────

const schema = `
CREATE TABLE IF NOT EXISTS events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    ts  INTEGER NOT NULL,                       -- unix nanos UTC
    typ TEXT    NOT NULL,
    payload BLOB NOT NULL                       -- JSON
);

CREATE TABLE IF NOT EXISTS tasks (
    name        TEXT PRIMARY KEY,
    cron        TEXT NOT NULL,
    prompt      TEXT NOT NULL,
    tools_json  TEXT NOT NULL DEFAULT '[]',
    enabled     INTEGER NOT NULL DEFAULT 1,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS task_runs (
    id         TEXT PRIMARY KEY,                -- uuid
    task_name  TEXT NOT NULL,
    trigger    TEXT NOT NULL,                   -- "schedule"|"manual"|"bootstrap"
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    status     TEXT NOT NULL,                   -- "running"|"completed"|"failed"|"canceled"|"timeout"
    summary    TEXT,
    tool_calls INTEGER NOT NULL DEFAULT 0,
    cost_usd_micro INTEGER NOT NULL DEFAULT 0,
    error      TEXT
);

CREATE INDEX IF NOT EXISTS task_runs_by_task_started
    ON task_runs(task_name, started_at DESC);

CREATE INDEX IF NOT EXISTS task_runs_by_started
    ON task_runs(started_at DESC);

CREATE TABLE IF NOT EXISTS task_run_logs (
    run_id     TEXT NOT NULL,
    seq        INTEGER NOT NULL,
    ts         INTEGER NOT NULL,
    level      TEXT NOT NULL,                   -- "info"|"tool"|"error"|"debug"
    message    TEXT NOT NULL,
    PRIMARY KEY (run_id, seq),
    FOREIGN KEY (run_id) REFERENCES task_runs(id) ON DELETE CASCADE
);

-- Reserved for v1.0 clustering: a "watermark" each replica advances to
-- record what events it has applied. Solo mode keeps row replica='solo'.
CREATE TABLE IF NOT EXISTS event_watermarks (
    replica   TEXT PRIMARY KEY,
    applied_seq INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL
);
`

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("state: migrate: %w", err)
	}
	// Seed solo watermark idempotently.
	_, err := db.Exec(
		`INSERT OR IGNORE INTO event_watermarks(replica, applied_seq, updated_at) VALUES('solo', 0, ?)`,
		time.Now().UTC().UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("state: seed watermark: %w", err)
	}
	// Mission journal lives in its own file (mission.go) — same db handle.
	if err := migrateMission(db); err != nil {
		return err
	}
	return nil
}

// ─── Events ─────────────────────────────────────────────────────────────────

// Event is one append to the log. Subscribers see these in seq order.
type Event struct {
	Seq     int64           `json:"seq"`
	TS      time.Time       `json:"ts"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Append writes one event. Idempotency / dedupe is up to the caller's payload
// design — this layer is purely an ordered, durable log.
func (s *Store) Append(ctx context.Context, typ string, payload any) (Event, error) {
	if typ == "" {
		return Event{}, errors.New("state.Append: type required")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("state.Append: marshal: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events(ts, typ, payload) VALUES(?, ?, ?)`,
		now.UnixNano(), typ, raw,
	)
	if err != nil {
		return Event{}, fmt.Errorf("state.Append: insert: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Event{}, err
	}
	return Event{Seq: seq, TS: now, Type: typ, Payload: raw}, nil
}

// EventsSince returns events with seq > fromSeq, up to limit. Used by the
// reconciler loop + the future Raft replicator.
func (s *Store) EventsSince(ctx context.Context, fromSeq int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, ts, typ, payload FROM events WHERE seq > ? ORDER BY seq ASC LIMIT ?`,
		fromSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("state.EventsSince: %w", err)
	}
	defer rows.Close()
	out := make([]Event, 0, limit)
	for rows.Next() {
		var (
			seq, ts int64
			typ     string
			payload []byte
		)
		if err := rows.Scan(&seq, &ts, &typ, &payload); err != nil {
			return nil, err
		}
		out = append(out, Event{
			Seq:     seq,
			TS:      time.Unix(0, ts).UTC(),
			Type:    typ,
			Payload: payload,
		})
	}
	return out, rows.Err()
}

// ─── Tasks ──────────────────────────────────────────────────────────────────

// Task is the persisted form of a config.Schedule entry. The scheduler hydrates
// from this on boot so manual updates via MCP survive restart.
type Task struct {
	Name      string    `json:"name"`
	Cron      string    `json:"cron"`
	Prompt    string    `json:"prompt"`
	Tools     []string  `json:"tools"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UpsertTask writes (or updates) one Task and appends a TaskUpserted event.
func (s *Store) UpsertTask(ctx context.Context, t Task) error {
	if t.Name == "" {
		return errors.New("state.UpsertTask: Name required")
	}
	if t.Tools == nil {
		t.Tools = []string{}
	}
	tools, err := json.Marshal(t.Tools)
	if err != nil {
		return err
	}
	t.UpdatedAt = time.Now().UTC()
	enabled := 0
	if t.Enabled {
		enabled = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks(name, cron, prompt, tools_json, enabled, updated_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET cron=excluded.cron,
		    prompt=excluded.prompt, tools_json=excluded.tools_json,
		    enabled=excluded.enabled, updated_at=excluded.updated_at`,
		t.Name, t.Cron, t.Prompt, string(tools), enabled, t.UpdatedAt.UnixNano(),
	); err != nil {
		return fmt.Errorf("state.UpsertTask: %w", err)
	}
	// Same transaction → event log stays in sync with table state.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(ts, typ, payload) VALUES(?, 'task.upserted', ?)`,
		t.UpdatedAt.UnixNano(),
		mustJSON(t),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// GetTask returns the task by name, or sql.ErrNoRows if absent.
func (s *Store) GetTask(ctx context.Context, name string) (Task, error) {
	var (
		t          Task
		toolsJSON  string
		enabled    int
		updated    int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT name, cron, prompt, tools_json, enabled, updated_at FROM tasks WHERE name = ?`,
		name,
	).Scan(&t.Name, &t.Cron, &t.Prompt, &toolsJSON, &enabled, &updated)
	if err != nil {
		return Task{}, err
	}
	t.Enabled = enabled == 1
	t.UpdatedAt = time.Unix(0, updated).UTC()
	if err := json.Unmarshal([]byte(toolsJSON), &t.Tools); err != nil {
		return Task{}, fmt.Errorf("state.GetTask: tools_json: %w", err)
	}
	return t, nil
}

// ListTasks returns every persisted task, in name order.
func (s *Store) ListTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, cron, prompt, tools_json, enabled, updated_at FROM tasks ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var (
			t         Task
			toolsJSON string
			enabled   int
			updated   int64
		)
		if err := rows.Scan(&t.Name, &t.Cron, &t.Prompt, &toolsJSON, &enabled, &updated); err != nil {
			return nil, err
		}
		t.Enabled = enabled == 1
		t.UpdatedAt = time.Unix(0, updated).UTC()
		if err := json.Unmarshal([]byte(toolsJSON), &t.Tools); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ─── Task runs ──────────────────────────────────────────────────────────────

// RunStatus tracks the lifecycle of a single task execution.
type RunStatus string

const (
	StatusRunning   RunStatus = "running"
	StatusCompleted RunStatus = "completed"
	StatusFailed    RunStatus = "failed"
	StatusCanceled  RunStatus = "canceled"
	StatusTimeout   RunStatus = "timeout"
)

// IsTerminal reports whether s is a final status (no further transitions).
func (s RunStatus) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCanceled, StatusTimeout:
		return true
	}
	return false
}

// TaskRun is one execution attempt of a Task (or an ad-hoc run).
type TaskRun struct {
	ID           string    `json:"id"`
	TaskName     string    `json:"task_name"`
	Trigger      string    `json:"trigger"` // schedule | manual | bootstrap
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
	Status       RunStatus `json:"status"`
	Summary      string    `json:"summary,omitempty"`
	ToolCalls    int       `json:"tool_calls"`
	CostUSDMicro int64     `json:"cost_usd_micro"`
	Error        string    `json:"error,omitempty"`
}

// StartRun inserts a new task_run row at status=running.
func (s *Store) StartRun(ctx context.Context, run TaskRun) error {
	if run.ID == "" || run.TaskName == "" {
		return errors.New("state.StartRun: ID and TaskName required")
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	run.Status = StatusRunning
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_runs(id, task_name, trigger, started_at, status)
		 VALUES(?,?,?,?,?)`,
		run.ID, run.TaskName, run.Trigger, run.StartedAt.UnixNano(), string(run.Status),
	); err != nil {
		return fmt.Errorf("state.StartRun: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(ts, typ, payload) VALUES(?, 'run.started', ?)`,
		run.StartedAt.UnixNano(), mustJSON(run),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishRun closes the task_run row with a terminal status.
func (s *Store) FinishRun(ctx context.Context, id string, status RunStatus, summary, errMsg string, toolCalls int) error {
	if !status.IsTerminal() {
		return fmt.Errorf("state.FinishRun: status %q is not terminal", status)
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE task_runs
		 SET ended_at=?, status=?, summary=?, error=?, tool_calls=?
		 WHERE id=? AND status='running'`,
		now.UnixNano(), string(status), summary, errMsg, toolCalls, id,
	)
	if err != nil {
		return fmt.Errorf("state.FinishRun: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("state.FinishRun: run %q is not in 'running' state", id)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(ts, typ, payload) VALUES(?, 'run.finished', ?)`,
		now.UnixNano(),
		mustJSON(map[string]any{
			"id": id, "status": status, "summary": summary, "error": errMsg, "tool_calls": toolCalls,
		}),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// GetRun returns one run by id, or sql.ErrNoRows.
func (s *Store) GetRun(ctx context.Context, id string) (TaskRun, error) {
	var (
		r       TaskRun
		started int64
		ended   sql.NullInt64
		status  string
		summary sql.NullString
		errMsg  sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, task_name, trigger, started_at, ended_at, status, summary, tool_calls, cost_usd_micro, error
		 FROM task_runs WHERE id = ?`,
		id,
	).Scan(&r.ID, &r.TaskName, &r.Trigger, &started, &ended, &status, &summary, &r.ToolCalls, &r.CostUSDMicro, &errMsg)
	if err != nil {
		return TaskRun{}, err
	}
	r.StartedAt = time.Unix(0, started).UTC()
	if ended.Valid {
		r.EndedAt = time.Unix(0, ended.Int64).UTC()
	}
	r.Status = RunStatus(status)
	r.Summary = summary.String
	r.Error = errMsg.String
	return r, nil
}

// ListRuns returns the most recent runs, newest first. taskName may be empty
// to list across all tasks. limit is capped at 1000.
func (s *Store) ListRuns(ctx context.Context, taskName string, limit int) ([]TaskRun, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if taskName == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, task_name, trigger, started_at, ended_at, status, summary, tool_calls, cost_usd_micro, error
			 FROM task_runs ORDER BY started_at DESC LIMIT ?`,
			limit,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, task_name, trigger, started_at, ended_at, status, summary, tool_calls, cost_usd_micro, error
			 FROM task_runs WHERE task_name = ? ORDER BY started_at DESC LIMIT ?`,
			taskName, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskRun
	for rows.Next() {
		var (
			r       TaskRun
			started int64
			ended   sql.NullInt64
			status  string
			summary sql.NullString
			errMsg  sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.TaskName, &r.Trigger, &started, &ended, &status, &summary, &r.ToolCalls, &r.CostUSDMicro, &errMsg); err != nil {
			return nil, err
		}
		r.StartedAt = time.Unix(0, started).UTC()
		if ended.Valid {
			r.EndedAt = time.Unix(0, ended.Int64).UTC()
		}
		r.Status = RunStatus(status)
		r.Summary = summary.String
		r.Error = errMsg.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// AppendRunLog appends one structured log line to a run. seq must monotonically
// increase per run; callers are expected to manage their own counter (the run
// has a single writer in the daemon).
func (s *Store) AppendRunLog(ctx context.Context, runID string, seq int, level, message string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_run_logs(run_id, seq, ts, level, message) VALUES(?,?,?,?,?)`,
		runID, seq, time.Now().UTC().UnixNano(), level, message,
	)
	return err
}

// RunLog is one row of task_run_logs.
type RunLog struct {
	RunID   string    `json:"run_id"`
	Seq     int       `json:"seq"`
	TS      time.Time `json:"ts"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// LogsForRun returns ordered logs for one run. limit is capped at 5000.
func (s *Store) LogsForRun(ctx context.Context, runID string, limit int) ([]RunLog, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id, seq, ts, level, message FROM task_run_logs
		 WHERE run_id = ? ORDER BY seq ASC LIMIT ?`,
		runID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunLog
	for rows.Next() {
		var (
			r  RunLog
			ts int64
		)
		if err := rows.Scan(&r.RunID, &r.Seq, &ts, &r.Level, &r.Message); err != nil {
			return nil, err
		}
		r.TS = time.Unix(0, ts).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Errorf("state: marshal: %w", err))
	}
	return b
}
