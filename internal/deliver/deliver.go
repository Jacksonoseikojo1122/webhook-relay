// Package deliver sends stored events to your app and retries until it says yes.
package deliver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/Jacksonoseikojo1122/webhook-relay/internal/config"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/store"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/verify"
)

// Schedule is the wait before each retry. Past the end, the last value repeats.
// Roughly: within a minute, within the hour, then hourly for most of a day.
var Schedule = []time.Duration{
	10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour,
}

// Backoff returns the wait after `attempt` failures, with up to 20% jitter so
// many failed events don't all retry in the same second.
func Backoff(attempt int, jitter func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := Schedule[min(attempt, len(Schedule))-1]
	return base + time.Duration(float64(base)*0.2*jitter())
}

type Metrics struct {
	Delivered, Failed, Dead atomic.Int64
}

type Worker struct {
	Store   *store.Store
	Config  *config.Config
	Client  *http.Client
	Log     *slog.Logger
	Batch   int
	Lease   time.Duration
	Poll    time.Duration
	Now     func() time.Time
	Jitter  func() float64
	Metrics *Metrics
}

func (w *Worker) defaults() {
	if w.Client == nil {
		w.Client = &http.Client{Timeout: w.Config.Timeout}
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	if w.Batch == 0 {
		w.Batch = 20
	}
	if w.Lease == 0 {
		w.Lease = w.Config.Timeout + 30*time.Second
	}
	if w.Poll == 0 {
		w.Poll = time.Second
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.Jitter == nil {
		w.Jitter = rand.Float64
	}
	if w.Metrics == nil {
		w.Metrics = &Metrics{}
	}
}

// Run polls for due events until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	w.defaults()
	t := time.NewTicker(w.Poll)
	defer t.Stop()
	for {
		if n, err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.Log.Error("delivery tick failed", "err", err)
		} else if n == w.Batch {
			continue // more are waiting; don't sleep
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick claims one batch of due events and delivers them. It returns how many it handled.
func (w *Worker) Tick(ctx context.Context) (int, error) {
	w.defaults()
	events, err := w.Store.Claim(ctx, w.Now(), w.Lease, w.Batch)
	if err != nil {
		return 0, err
	}
	for i := range events {
		w.deliver(ctx, &events[i])
	}
	return len(events), nil
}

func (w *Worker) deliver(ctx context.Context, e *store.Event) {
	ctx, span := otel.Tracer("webhook-relay").Start(ctx, "relay.deliver")
	defer span.End()
	span.SetAttributes(
		attribute.String("relay.event_id", e.ID),
		attribute.String("relay.source", e.Source),
		attribute.String("relay.provider", e.Provider),
		attribute.Int("relay.attempt", e.Attempts+1),
	)

	attempt := store.Attempt{N: e.Attempts + 1, At: w.Now()}
	src, ok := w.Config.Find(e.Source)
	if !ok {
		// The source was removed from the config after this event arrived.
		attempt.Error = "source no longer configured"
		w.finish(ctx, e, attempt, false)
		return
	}

	ts := attempt.At.Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, src.Destination, bytes.NewReader(e.Body))
	if err != nil {
		attempt.Error = err.Error()
		w.finish(ctx, e, attempt, false)
		return
	}
	ct := e.Headers["Content-Type"]
	if ct == "" {
		ct = "application/json"
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("User-Agent", "webhook-relay/0.1")
	req.Header.Set("X-Relay-Event-Id", e.ID)
	req.Header.Set("X-Relay-Source", e.Source)
	req.Header.Set("X-Relay-Provider", e.Provider)
	req.Header.Set("X-Relay-Provider-Event-Id", e.ProviderEventID)
	req.Header.Set("X-Relay-Attempt", strconv.Itoa(attempt.N))
	req.Header.Set("X-Relay-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Relay-Signature", verify.Outbound(src.DestinationSecret, ts, e.Body))

	start := time.Now()
	resp, err := w.Client.Do(req)
	attempt.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		attempt.Error = err.Error()
		span.RecordError(err)
		w.finish(ctx, e, attempt, false)
		return
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	resp.Body.Close()
	attempt.StatusCode = resp.StatusCode
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	ok2xx := resp.StatusCode >= 200 && resp.StatusCode < 300
	if !ok2xx {
		attempt.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}
	w.finish(ctx, e, attempt, ok2xx)
}

func (w *Worker) finish(ctx context.Context, e *store.Event, a store.Attempt, ok bool) {
	next, nextAt := store.Delivered, a.At
	switch {
	case ok:
		w.Metrics.Delivered.Add(1)
	case a.N >= w.Config.MaxAttempts:
		next = store.Dead
		w.Metrics.Dead.Add(1)
		w.Log.Warn("event is dead after max attempts", "event", e.ID, "source", e.Source, "attempts", a.N, "err", a.Error)
	default:
		next, nextAt = store.Retrying, a.At.Add(Backoff(a.N, w.Jitter))
		w.Metrics.Failed.Add(1)
		w.Log.Info("delivery failed, will retry", "event", e.ID, "attempt", a.N, "next", nextAt, "err", a.Error)
	}
	if !ok {
		_, span := otel.Tracer("webhook-relay").Start(ctx, "relay.deliver.failed")
		span.SetStatus(codes.Error, a.Error)
		span.End()
	}
	// Record on a fresh context so a shutdown mid-delivery still saves the outcome.
	if err := w.Store.Finish(context.WithoutCancel(ctx), e.ID, a, next, nextAt); err != nil {
		w.Log.Error("could not record delivery attempt", "event", e.ID, "err", err)
	}
}
