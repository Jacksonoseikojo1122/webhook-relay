// Package store keeps every event the relay accepts, de-duplicates them, and
// acts as the delivery queue. The same SQL runs on SQLite (local, single node)
// and Postgres (production, several relays sharing the work).
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type Status string

const (
	Pending    Status = "pending"    // waiting for its first delivery
	Delivering Status = "delivering" // claimed by a worker right now
	Retrying   Status = "retrying"   // a delivery failed; another is scheduled
	Delivered  Status = "delivered"  // your app answered 2xx
	Dead       Status = "dead"       // gave up after max attempts; replay it by hand
)

type Event struct {
	ID              string            `json:"id"`
	Source          string            `json:"source"`
	Provider        string            `json:"provider"`
	ProviderEventID string            `json:"provider_event_id"`
	ReceivedAt      time.Time         `json:"received_at"`
	Headers         map[string]string `json:"headers"`
	Body            []byte            `json:"-"`
	Status          Status            `json:"status"`
	Attempts        int               `json:"attempts"`
	NextAttemptAt   time.Time         `json:"next_attempt_at"`
	LastError       string            `json:"last_error,omitempty"`
	LastStatusCode  int               `json:"last_status_code,omitempty"`
	DeliveredAt     *time.Time        `json:"delivered_at,omitempty"`
}

type Attempt struct {
	N          int       `json:"n"`
	At         time.Time `json:"at"`
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	DurationMs int64     `json:"duration_ms"`
}

type Store struct {
	db       *sql.DB
	postgres bool
}

const schemaSQLite = `
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY,
  source TEXT NOT NULL,
  provider TEXT NOT NULL,
  provider_event_id TEXT NOT NULL,
  received_at INTEGER NOT NULL,
  headers TEXT NOT NULL,
  body BLOB NOT NULL,
  status TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  lease_until INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  last_status_code INTEGER NOT NULL DEFAULT 0,
  delivered_at INTEGER,
  UNIQUE (source, provider_event_id)
);
CREATE INDEX IF NOT EXISTS events_due ON events (status, next_attempt_at);
CREATE INDEX IF NOT EXISTS events_received ON events (received_at);
CREATE TABLE IF NOT EXISTS attempts (
  event_id TEXT NOT NULL REFERENCES events(id),
  n INTEGER NOT NULL,
  at INTEGER NOT NULL,
  status_code INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL,
  PRIMARY KEY (event_id, n, at)
);`

var schemaPostgres = strings.NewReplacer("BLOB", "BYTEA", "INTEGER", "BIGINT").Replace(schemaSQLite)

// Open connects and creates the tables. driver is "sqlite" or "postgres".
func Open(driver, dsn string) (*Store, error) {
	name := driver
	if driver == "postgres" {
		name = "pgx"
	}
	db, err := sql.Open(name, dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, postgres: driver == "postgres"}
	if !s.postgres {
		// One writer at a time is SQLite's model; WAL keeps readers unblocked.
		db.SetMaxOpenConns(1)
		for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
			if _, err := db.Exec(p); err != nil {
				return nil, err
			}
		}
	}
	schema := schemaSQLite
	if s.postgres {
		schema = schemaPostgres
	}
	for _, stmt := range strings.Split(schema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "rly_" + hex.EncodeToString(b)
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// Insert saves a new event. If this source already sent an event with the same
// provider ID, nothing is written and the existing event's ID comes back with
// duplicate=true.
func (s *Store) Insert(ctx context.Context, e *Event) (duplicate bool, err error) {
	if e.ID == "" {
		e.ID = newID()
	}
	h, _ := json.Marshal(e.Headers)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO events (id, source, provider, provider_event_id, received_at, headers, body, status, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (source, provider_event_id) DO NOTHING`,
		e.ID, e.Source, e.Provider, e.ProviderEventID, ms(e.ReceivedAt), string(h), e.Body, string(Pending), ms(e.NextAttemptAt))
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		e.Status = Pending
		return false, nil
	}
	err = s.db.QueryRowContext(ctx, `SELECT id FROM events WHERE source = $1 AND provider_event_id = $2`, e.Source, e.ProviderEventID).Scan(&e.ID)
	return true, err
}

const eventCols = `id, source, provider, provider_event_id, received_at, headers, body, status, attempts, next_attempt_at, last_error, last_status_code, delivered_at`

func scanEvent(row interface{ Scan(...any) error }) (Event, error) {
	var e Event
	var received, next int64
	var delivered sql.NullInt64
	var headers, status string
	if err := row.Scan(&e.ID, &e.Source, &e.Provider, &e.ProviderEventID, &received, &headers, &e.Body, &status,
		&e.Attempts, &next, &e.LastError, &e.LastStatusCode, &delivered); err != nil {
		return e, err
	}
	e.ReceivedAt = time.UnixMilli(received).UTC()
	e.NextAttemptAt = time.UnixMilli(next).UTC()
	e.Status = Status(status)
	_ = json.Unmarshal([]byte(headers), &e.Headers)
	if delivered.Valid {
		t := time.UnixMilli(delivered.Int64).UTC()
		e.DeliveredAt = &t
	}
	return e, nil
}

// Claim leases up to n events that are due, so only one worker delivers each.
// A lease that runs out (a worker crashed mid-delivery) makes the event due again.
func (s *Store) Claim(ctx context.Context, now time.Time, lease time.Duration, n int) ([]Event, error) {
	lock := ""
	if s.postgres {
		lock = "FOR UPDATE SKIP LOCKED"
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		UPDATE events SET status = $1, lease_until = $2
		WHERE id IN (
			SELECT id FROM events
			WHERE (status IN ($3, $4) AND next_attempt_at <= $5)
			   OR (status = $1 AND lease_until < $5)
			ORDER BY next_attempt_at
			LIMIT $6 %s
		)
		RETURNING %s`, lock, eventCols),
		string(Delivering), ms(now.Add(lease)), string(Pending), string(Retrying), ms(now), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Finish records one delivery attempt and moves the event to its next state.
func (s *Store) Finish(ctx context.Context, id string, a Attempt, next Status, nextAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempts (event_id, n, at, status_code, error, duration_ms) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, a.N, ms(a.At), a.StatusCode, a.Error, a.DurationMs); err != nil {
		return err
	}
	var delivered any
	if next == Delivered {
		delivered = ms(a.At)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE events SET status = $1, attempts = $2, next_attempt_at = $3, lease_until = 0,
		       last_error = $4, last_status_code = $5, delivered_at = COALESCE($6, delivered_at)
		WHERE id = $7`,
		string(next), a.N, ms(nextAt), a.Error, a.StatusCode, delivered, id); err != nil {
		return err
	}
	return tx.Commit()
}

var ErrNotFound = errors.New("event not found")

// Get returns an event with its body and full attempt history.
func (s *Store) Get(ctx context.Context, id string) (Event, []Attempt, error) {
	e, err := scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventCols+` FROM events WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, nil, ErrNotFound
	}
	if err != nil {
		return e, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT n, at, status_code, error, duration_ms FROM attempts WHERE event_id = $1 ORDER BY at`, id)
	if err != nil {
		return e, nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var at int64
		if err := rows.Scan(&a.N, &at, &a.StatusCode, &a.Error, &a.DurationMs); err != nil {
			return e, nil, err
		}
		a.At = time.UnixMilli(at).UTC()
		out = append(out, a)
	}
	return e, out, rows.Err()
}

type Filter struct {
	Status Status
	Source string
	Limit  int
}

// List returns recent events, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Event, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q := `SELECT ` + eventCols + ` FROM events WHERE ($1 = '' OR status = $1) AND ($2 = '' OR source = $2) ORDER BY received_at DESC LIMIT $3`
	rows, err := s.db.QueryContext(ctx, q, string(f.Status), f.Source, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Replay queues an event for delivery again, with a fresh retry budget.
// Its earlier attempts stay in the history.
func (s *Store) Replay(ctx context.Context, id string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE events SET status = $1, attempts = 0, next_attempt_at = $2, lease_until = 0 WHERE id = $3`,
		string(Pending), ms(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Counts returns how many events are in each status.
func (s *Store) Counts(ctx context.Context) (map[Status]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM events GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Status]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[Status(st)] = n
	}
	return out, rows.Err()
}
