// Package server receives webhooks and serves the admin API, metrics and dashboard.
package server

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Jacksonoseikojo1122/webhook-relay/internal/config"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/deliver"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/store"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/verify"
)

//go:embed templates/*.html
var templates embed.FS

// MaxBody is the largest webhook the relay accepts. Payment webhooks are a few KB.
const MaxBody = 1 << 20

type Server struct {
	Store      *store.Store
	Config     *config.Config
	AdminToken string
	Log        *slog.Logger
	Metrics    *deliver.Metrics
	Now        func() time.Time

	mu       sync.Mutex
	received map[string]int64 // per source
	dupes    map[string]int64
	badSig   map[string]int64
	tpl      *template.Template
}

func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Metrics == nil {
		s.Metrics = &deliver.Metrics{}
	}
	s.received, s.dupes, s.badSig = map[string]int64{}, map[string]int64{}, map[string]int64{}
	s.tpl = template.Must(template.New("").Funcs(template.FuncMap{
		"ago": func(t time.Time) string { return ago(s.Now(), t) },
		"fmt": func(v any) string {
			switch t := v.(type) {
			case time.Time:
				return t.Format("2006-01-02 15:04:05 MST")
			case *time.Time:
				if t != nil {
					return t.Format("2006-01-02 15:04:05 MST")
				}
			}
			return ""
		},
	}).ParseFS(templates, "templates/*.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /in/{source}", s.receive)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /api/events", s.admin(s.listEvents))
	mux.HandleFunc("GET /api/events/{id}", s.admin(s.getEvent))
	mux.HandleFunc("POST /api/events/{id}/replay", s.admin(s.replay))
	mux.HandleFunc("GET /api/stats", s.admin(s.stats))
	mux.HandleFunc("GET /{$}", s.admin(s.dashboard))
	mux.HandleFunc("GET /events/{id}", s.admin(s.eventPage))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) count(m map[string]int64, k string) {
	s.mu.Lock()
	m[k]++
	s.mu.Unlock()
}

// receive is what Paystack, Flutterwave, Stripe or any sender calls.
func (s *Server) receive(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("webhook-relay").Start(r.Context(), "relay.receive")
	defer span.End()

	name := r.PathValue("source")
	src, ok := s.Config.Find(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown source"})
		return
	}
	span.SetAttributes(attribute.String("relay.source", name), attribute.String("relay.provider", src.Provider))

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large or unreadable"})
		return
	}
	if err := verify.Signature(src, r.Header, body); err != nil {
		s.count(s.badSig, name)
		s.Log.Warn("rejected webhook", "source", name, "reason", err)
		// Don't say which part failed: that only helps someone forging requests.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "signature check failed"})
		return
	}

	now := s.Now()
	e := &store.Event{
		Source:          name,
		Provider:        src.Provider,
		ProviderEventID: verify.EventID(src.Provider, r.Header, body),
		ReceivedAt:      now,
		NextAttemptAt:   now,
		Headers:         map[string]string{"Content-Type": r.Header.Get("Content-Type")},
		Body:            body,
	}
	dup, err := s.Store.Insert(ctx, e)
	if err != nil {
		// Not saved, so tell the provider to try again later.
		s.Log.Error("could not store webhook", "source", name, "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		return
	}
	span.SetAttributes(attribute.String("relay.event_id", e.ID), attribute.Bool("relay.duplicate", dup))
	if dup {
		s.count(s.dupes, name)
		// 200, so the provider stops resending something we already have.
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate", "id": e.ID})
		return
	}
	s.count(s.received, name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted", "id": e.ID})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// admin accepts the token as a Bearer header (API) or as the password of HTTP
// Basic auth (so a browser can open the dashboard).
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AdminToken == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "set RELAY_ADMIN_TOKEN to use the admin API and dashboard"})
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		_, pass, basic := r.BasicAuth()
		if basic {
			got = pass
			// Browsers resend Basic credentials on cross-site form posts, so a
			// change made with them must come from the relay's own pages.
			if r.Method != http.MethodGet && !sameOrigin(r) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request refused"})
				return
			}
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.AdminToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="webhook-relay"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "admin token required"})
			return
		}
		next(w, r)
	}
}

func sameOrigin(r *http.Request) bool {
	ref := r.Header.Get("Origin")
	if ref == "" {
		ref = r.Header.Get("Referer")
	}
	if ref == "" {
		return false
	}
	u, err := url.Parse(ref)
	return err == nil && u.Host == r.Host
}

func filterFrom(r *http.Request) store.Filter {
	return store.Filter{Status: store.Status(r.URL.Query().Get("status")), Source: r.URL.Query().Get("source"), Limit: 100}
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.Store.List(r.Context(), filterFrom(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if events == nil {
		events = []store.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func bodyView(b []byte) any {
	var v any
	if json.Unmarshal(b, &v) == nil {
		return v
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return fmt.Sprintf("<%d bytes of binary>", len(b))
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	e, attempts, err := s.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if attempts == nil {
		attempts = []store.Attempt{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": e, "body": bodyView(e.Body), "attempts": attempts})
}

func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.Store.Replay(r.Context(), id, s.Now())
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if r.URL.Query().Get("redirect") == "1" {
		http.Redirect(w, r, "/events/"+id, http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued", "id": id})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.Counts(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// metrics is Prometheus text format, so any Prometheus or Grafana Agent can scrape it.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.mu.Lock()
	defer s.mu.Unlock()
	family := func(name, help, kind string, m map[string]int64, label string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, k, m[k])
		}
	}
	family("relay_webhooks_received_total", "Webhooks accepted and stored.", "counter", s.received, "source")
	family("relay_webhooks_duplicate_total", "Webhooks dropped as duplicates.", "counter", s.dupes, "source")
	family("relay_signature_failures_total", "Webhooks rejected for a bad or missing signature.", "counter", s.badSig, "source")
	family("relay_deliveries_total", "Delivery attempts by result.", "counter", map[string]int64{
		"delivered": s.Metrics.Delivered.Load(), "failed": s.Metrics.Failed.Load(), "dead": s.Metrics.Dead.Load(),
	}, "result")
	if c, err := s.Store.Counts(r.Context()); err == nil {
		m := map[string]int64{}
		for k, v := range c {
			m[string(k)] = int64(v)
		}
		family("relay_events", "Stored events by status.", "gauge", m, "status")
	}
}

type row struct {
	store.Event
	Pill string
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	f := filterFrom(r)
	events, err := s.Store.List(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	counts, _ := s.Store.Counts(r.Context())
	rows := make([]row, len(events))
	for i, e := range events {
		rows[i] = row{e, string(e.Status)}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tpl.ExecuteTemplate(w, "dashboard.html", map[string]any{
		"Events":   rows,
		"Counts":   counts,
		"Filter":   f,
		"Sources":  s.Config.Sources,
		"Statuses": []store.Status{store.Pending, store.Retrying, store.Delivered, store.Dead},
	})
}

func (s *Server) eventPage(w http.ResponseWriter, r *http.Request) {
	e, attempts, err := s.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pretty, _ := json.MarshalIndent(bodyView(e.Body), "", "  ")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tpl.ExecuteTemplate(w, "event.html", map[string]any{"E": e, "Attempts": attempts, "Body": string(pretty)})
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	var out string
	switch {
	case d < time.Minute:
		out = fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		out = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		out = fmt.Sprintf("%dh", int(d.Hours()))
	default:
		out = fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if future {
		return "in " + out
	}
	return out + " ago"
}
