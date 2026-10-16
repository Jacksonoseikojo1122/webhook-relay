package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openTest uses SQLite in a temp dir, or Postgres when TEST_DATABASE_URL is set (CI).
func openTest(t *testing.T) *Store {
	t.Helper()
	driver, dsn := "sqlite", filepath.Join(t.TempDir(), "test.db")
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		driver, dsn = "postgres", u
	}
	s, err := Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if driver == "postgres" {
		_, _ = s.db.Exec(`TRUNCATE attempts, events`)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var t0 = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

func event(id string) *Event {
	return &Event{Source: "paystack-live", Provider: "paystack", ProviderEventID: id, ReceivedAt: t0, NextAttemptAt: t0,
		Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"n":1}`)}
}

func TestInsertDeduplicates(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	first := event("charge.success:1")
	if dup, err := s.Insert(ctx, first); err != nil || dup {
		t.Fatalf("first insert: dup=%v err=%v", dup, err)
	}
	again := event("charge.success:1")
	dup, err := s.Insert(ctx, again)
	if err != nil || !dup {
		t.Fatalf("second insert should be a duplicate: dup=%v err=%v", dup, err)
	}
	if again.ID != first.ID {
		t.Fatalf("duplicate should point at the original event: %s vs %s", again.ID, first.ID)
	}
	// The same provider ID from a different source is a different event.
	other := event("charge.success:1")
	other.Source = "paystack-test"
	if dup, _ := s.Insert(ctx, other); dup {
		t.Fatal("different source treated as duplicate")
	}
}

func TestClaimLeasesEachEventOnce(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	e := event("a")
	_, _ = s.Insert(ctx, e)

	got, err := s.Claim(ctx, t0, time.Minute, 10)
	if err != nil || len(got) != 1 || got[0].Status != Delivering {
		t.Fatalf("first claim: %v %+v", err, got)
	}
	if again, _ := s.Claim(ctx, t0.Add(30*time.Second), time.Minute, 10); len(again) != 0 {
		t.Fatal("a leased event was handed out twice")
	}
	// The worker died: after the lease runs out, the event is due again.
	if again, _ := s.Claim(ctx, t0.Add(2*time.Minute), time.Minute, 10); len(again) != 1 {
		t.Fatal("an expired lease was not reclaimed")
	}
}

func TestFinishAndHistory(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	e := event("b")
	_, _ = s.Insert(ctx, e)
	_, _ = s.Claim(ctx, t0, time.Minute, 10)

	if err := s.Finish(ctx, e.ID, Attempt{N: 1, At: t0, StatusCode: 500, Error: "HTTP 500", DurationMs: 12}, Retrying, t0.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.Claim(ctx, t0.Add(5*time.Second), time.Minute, 10); len(due) != 0 {
		t.Fatal("retried before its backoff")
	}
	due, _ := s.Claim(ctx, t0.Add(11*time.Second), time.Minute, 10)
	if len(due) != 1 {
		t.Fatal("not retried after backoff")
	}
	at := t0.Add(11 * time.Second)
	if err := s.Finish(ctx, e.ID, Attempt{N: 2, At: at, StatusCode: 200, DurationMs: 9}, Delivered, at); err != nil {
		t.Fatal(err)
	}

	got, attempts, err := s.Get(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Delivered || got.DeliveredAt == nil || !got.DeliveredAt.Equal(at) || got.Attempts != 2 {
		t.Fatalf("unexpected final state: %+v", got)
	}
	if len(attempts) != 2 || attempts[0].StatusCode != 500 || attempts[1].StatusCode != 200 {
		t.Fatalf("history: %+v", attempts)
	}
	if string(got.Body) != `{"n":1}` || got.Headers["Content-Type"] != "application/json" {
		t.Fatal("body or headers not round-tripped")
	}
}

func TestReplayListCounts(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	e := event("c")
	_, _ = s.Insert(ctx, e)
	_, _ = s.Claim(ctx, t0, time.Minute, 10)
	_ = s.Finish(ctx, e.ID, Attempt{N: 12, At: t0, Error: "timeout"}, Dead, t0)

	if dead, _ := s.List(ctx, Filter{Status: Dead}); len(dead) != 1 {
		t.Fatal("dead filter")
	}
	if err := s.Replay(ctx, e.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get(ctx, e.ID)
	if got.Status != Pending || got.Attempts != 0 {
		t.Fatalf("replay should reset: %+v", got)
	}
	if err := s.Replay(ctx, "rly_missing", t0); !errors.Is(err, ErrNotFound) {
		t.Fatal("replaying a missing event should be ErrNotFound")
	}
	c, _ := s.Counts(ctx)
	if c[Pending] != 1 {
		t.Fatalf("counts: %v", c)
	}
}
