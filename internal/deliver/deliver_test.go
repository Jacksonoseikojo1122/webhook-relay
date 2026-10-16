package deliver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jacksonoseikojo1122/webhook-relay/internal/config"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/store"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/verify"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T, handler http.HandlerFunc, maxAttempts int) (*Worker, *store.Store, *clock) {
	t.Helper()
	app := httptest.NewServer(handler)
	t.Cleanup(app.Close)
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{MaxAttempts: maxAttempts, Timeout: 2 * time.Second, Sources: []config.Source{
		{Name: "ps", Provider: "paystack", Secret: "sk", Destination: app.URL, DestinationSecret: "dest"},
	}}
	c := &clock{time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)}
	w := &Worker{Store: st, Config: cfg, Now: c.now, Jitter: func() float64 { return 0 }}
	return w, st, c
}

func insert(t *testing.T, st *store.Store, at time.Time) *store.Event {
	e := &store.Event{Source: "ps", Provider: "paystack", ProviderEventID: "charge.success:1", ReceivedAt: at, NextAttemptAt: at,
		Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"event":"charge.success"}`)}
	if _, err := st.Insert(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRetriesUntilYourAppSaysYes(t *testing.T) {
	var calls atomic.Int32
	var lastSig, lastTs, lastAttempt string
	var lastBody []byte
	w, st, c := setup(t, func(rw http.ResponseWriter, r *http.Request) {
		lastSig, lastTs, lastAttempt = r.Header.Get("X-Relay-Signature"), r.Header.Get("X-Relay-Timestamp"), r.Header.Get("X-Relay-Attempt")
		lastBody, _ = io.ReadAll(r.Body)
		if calls.Add(1) < 3 {
			http.Error(rw, "database is down", http.StatusInternalServerError)
			return
		}
		rw.WriteHeader(http.StatusOK)
	}, 12)
	ctx := context.Background()
	e := insert(t, st, c.now())

	_, _ = w.Tick(ctx) // attempt 1 fails
	got, _, _ := st.Get(ctx, e.ID)
	if got.Status != store.Retrying || got.LastStatusCode != 500 {
		t.Fatalf("after 1st failure: %+v", got)
	}
	if !got.NextAttemptAt.Equal(c.now().Add(10 * time.Second)) {
		t.Fatalf("first retry should be 10s out, got %v", got.NextAttemptAt.Sub(c.now()))
	}

	c.advance(5 * time.Second)
	if n, _ := w.Tick(ctx); n != 0 {
		t.Fatal("retried before its backoff")
	}
	c.advance(6 * time.Second)
	_, _ = w.Tick(ctx) // attempt 2 fails
	c.advance(31 * time.Second)
	_, _ = w.Tick(ctx) // attempt 3 succeeds

	got, attempts, _ := st.Get(ctx, e.ID)
	if got.Status != store.Delivered || len(attempts) != 3 || calls.Load() != 3 {
		t.Fatalf("expected delivered on the 3rd try: %+v, %d attempts, %d calls", got, len(attempts), calls.Load())
	}
	if lastAttempt != "3" {
		t.Fatalf("X-Relay-Attempt = %s", lastAttempt)
	}
	// Your app can verify the delivery with the destination secret.
	verify.Now = c.now
	defer func() { verify.Now = time.Now }()
	if err := verify.CheckOutbound("dest", lastTs, lastSig, lastBody, 5*time.Minute); err != nil {
		t.Fatalf("delivery signature: %v", err)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	w, st, c := setup(t, func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(http.StatusBadGateway) }, 3)
	ctx := context.Background()
	e := insert(t, st, c.now())
	for i := 0; i < 3; i++ {
		_, _ = w.Tick(ctx)
		c.advance(2 * time.Hour)
	}
	got, _, _ := st.Get(ctx, e.ID)
	if got.Status != store.Dead || got.Attempts != 3 {
		t.Fatalf("expected dead after 3: %+v", got)
	}
	if n, _ := w.Tick(ctx); n != 0 {
		t.Fatal("dead events must not be retried automatically")
	}
	if w.Metrics.Dead.Load() != 1 || w.Metrics.Failed.Load() != 2 {
		t.Fatalf("metrics: dead=%d failed=%d", w.Metrics.Dead.Load(), w.Metrics.Failed.Load())
	}
}

func TestUnreachableAppIsRetried(t *testing.T) {
	w, st, c := setup(t, func(rw http.ResponseWriter, r *http.Request) {}, 12)
	w.Config.Sources[0].Destination = "http://127.0.0.1:1" // nothing listens here
	ctx := context.Background()
	e := insert(t, st, c.now())
	_, _ = w.Tick(ctx)
	got, _, _ := st.Get(ctx, e.ID)
	if got.Status != store.Retrying || got.LastError == "" {
		t.Fatalf("connection error should schedule a retry: %+v", got)
	}
}

func TestBackoffSchedule(t *testing.T) {
	zero := func() float64 { return 0 }
	want := []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := Backoff(i+1, zero); got != w {
			t.Errorf("attempt %d: got %v want %v", i+1, got, w)
		}
	}
	if got := Backoff(1, func() float64 { return 1 }); got != 12*time.Second {
		t.Errorf("jitter should add at most 20%%, got %v", got)
	}
}
