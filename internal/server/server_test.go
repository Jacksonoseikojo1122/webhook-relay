package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jacksonoseikojo1122/webhook-relay/internal/config"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/store"
)

const body = `{"event":"charge.success","data":{"id":42,"reference":"ref_42","amount":250000}}`

func sig(secret, b string) string {
	m := hmac.New(sha512.New, []byte(secret))
	m.Write([]byte(b))
	return hex.EncodeToString(m.Sum(nil))
}

func newServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{MaxAttempts: 12, Sources: []config.Source{
		{Name: "paystack-live", Provider: "paystack", Secret: "sk_live", Destination: "https://app.example/hooks", DestinationSecret: "d"},
	}}
	s := &Server{Store: st, Config: cfg, AdminToken: "admin"}
	return s.Handler(), st
}

func do(h http.Handler, method, path, b string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(b))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestReceive(t *testing.T) {
	h, st := newServer(t)
	good := map[string]string{"X-Paystack-Signature": sig("sk_live", body), "Content-Type": "application/json"}

	if w := do(h, "POST", "/in/nope", body, good); w.Code != 404 {
		t.Fatalf("unknown source: %d", w.Code)
	}
	if w := do(h, "POST", "/in/paystack-live", body, map[string]string{"X-Paystack-Signature": sig("wrong", body)}); w.Code != 401 {
		t.Fatalf("bad signature: %d", w.Code)
	}

	w := do(h, "POST", "/in/paystack-live", body, good)
	var first map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &first)
	if w.Code != 200 || first["status"] != "accepted" {
		t.Fatalf("accept: %d %s", w.Code, w.Body)
	}

	// Paystack resends the same event: acknowledged, not stored twice.
	w = do(h, "POST", "/in/paystack-live", body, good)
	var second map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	if w.Code != 200 || second["status"] != "duplicate" || second["id"] != first["id"] {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body)
	}

	events, _ := st.List(context.Background(), store.Filter{})
	if len(events) != 1 || events[0].ProviderEventID != "charge.success:42" {
		t.Fatalf("stored: %+v", events)
	}
}

func TestRejectsHugeBodies(t *testing.T) {
	h, _ := newServer(t)
	big := strings.Repeat("x", MaxBody+1)
	if w := do(h, "POST", "/in/paystack-live", big, nil); w.Code != 413 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestAdmin(t *testing.T) {
	h, _ := newServer(t)
	do(h, "POST", "/in/paystack-live", body, map[string]string{"X-Paystack-Signature": sig("sk_live", body)})

	if w := do(h, "GET", "/api/events", "", nil); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	auth := map[string]string{"Authorization": "Bearer admin"}
	w := do(h, "GET", "/api/events", "", auth)
	var list struct{ Events []store.Event }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Events) != 1 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	id := list.Events[0].ID

	w = do(h, "GET", "/api/events/"+id, "", auth)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"amount":250000`) {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/events/"+id+"/replay", "", auth); w.Code != 200 {
		t.Fatalf("replay: %d", w.Code)
	}
	if w := do(h, "POST", "/api/events/rly_nope/replay", "", auth); w.Code != 404 {
		t.Fatalf("replay missing: %d", w.Code)
	}
}

func TestDashboardAndCSRF(t *testing.T) {
	h, _ := newServer(t)
	do(h, "POST", "/in/paystack-live", body, map[string]string{"X-Paystack-Signature": sig("sk_live", body)})

	r := httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("me", "admin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "charge.success:42") {
		t.Fatalf("dashboard: %d", w.Code)
	}

	// A form on another site posting with the browser's saved Basic credentials.
	r = httptest.NewRequest("POST", "/api/events/x/replay", nil)
	r.SetBasicAuth("me", "admin")
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-site replay should be refused, got %d", w.Code)
	}
}

func TestMetrics(t *testing.T) {
	h, _ := newServer(t)
	do(h, "POST", "/in/paystack-live", body, map[string]string{"X-Paystack-Signature": sig("sk_live", body)})
	do(h, "POST", "/in/paystack-live", body, map[string]string{"X-Paystack-Signature": "00"})
	w := do(h, "GET", "/metrics", "", nil)
	for _, want := range []string{
		`relay_webhooks_received_total{source="paystack-live"} 1`,
		`relay_signature_failures_total{source="paystack-live"} 1`,
		`relay_events{status="pending"} 1`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("metrics missing %q:\n%s", want, w.Body)
		}
	}
}
