// A minimal "your app" that receives deliveries from Webhook Relay and checks
// their signature. Set FAIL_FIRST=2 to make it fail the first two tries of each
// event, so you can watch the relay retry.
//
//	RELAY_TO_APP_SECRET=dev-app-secret go run ./examples/go-receiver
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func validSignature(secret, ts, sig string, body []byte) bool {
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	// Reject old deliveries so a captured request can't be replayed later.
	if d := time.Since(time.Unix(unix, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + string(body)))
	want := "v1=" + hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(want))
}

func main() {
	secret := os.Getenv("RELAY_TO_APP_SECRET")
	failFirst, _ := strconv.Atoi(os.Getenv("FAIL_FIRST"))

	http.HandleFunc("POST /webhooks/{provider}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !validSignature(secret, r.Header.Get("X-Relay-Timestamp"), r.Header.Get("X-Relay-Signature"), body) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		attempt, _ := strconv.Atoi(r.Header.Get("X-Relay-Attempt"))
		if attempt <= failFirst {
			http.Error(w, "database is busy, try again", http.StatusServiceUnavailable)
			return
		}
		// X-Relay-Event-Id is stable across retries: use it as your idempotency key.
		log.Printf("got %s event %s (attempt %d): %s", r.PathValue("provider"), r.Header.Get("X-Relay-Event-Id"), attempt, body)
		fmt.Fprintln(w, "ok")
	})
	addr := ":" + envOr("PORT", "9090")
	log.Printf("receiver listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
