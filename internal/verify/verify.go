// Package verify checks provider signatures and works out each event's ID, so
// the same event delivered twice is recognised as one.
package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jacksonoseikojo1122/webhook-relay/internal/config"
)

var (
	ErrMissingSignature = errors.New("missing signature header")
	ErrBadSignature     = errors.New("signature does not match")
	ErrStale            = errors.New("signature timestamp is too old")
)

// StripeTolerance is how old a Stripe signature may be, to stop replays.
var StripeTolerance = 5 * time.Minute

// Now is replaceable in tests.
var Now = time.Now

func sign256(secret, msg []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(msg)
	return m.Sum(nil)
}

func sign512(secret, msg []byte) []byte {
	m := hmac.New(sha512.New, secret)
	m.Write(msg)
	return m.Sum(nil)
}

// equalHex compares a hex signature from a header with the expected bytes in
// constant time, so timing can't leak the secret.
func equalHex(got string, want []byte) bool {
	b, err := hex.DecodeString(strings.TrimSpace(got))
	if err != nil {
		return false
	}
	return hmac.Equal(b, want)
}

// Signature checks the request against the source's provider rules.
func Signature(src *config.Source, h http.Header, body []byte) error {
	switch src.Provider {
	case config.Paystack:
		// x-paystack-signature: hex HMAC-SHA512 of the raw body with your secret key.
		sig := h.Get("X-Paystack-Signature")
		if sig == "" {
			return ErrMissingSignature
		}
		if !equalHex(sig, sign512([]byte(src.Secret), body)) {
			return ErrBadSignature
		}
	case config.Flutterwave:
		// verif-hash: the secret hash you set in the dashboard, sent as-is.
		sig := h.Get("Verif-Hash")
		if sig == "" {
			return ErrMissingSignature
		}
		if subtle.ConstantTimeCompare([]byte(sig), []byte(src.Secret)) != 1 {
			return ErrBadSignature
		}
	case config.Stripe:
		return stripe(src.Secret, h.Get("Stripe-Signature"), body)
	case config.Generic:
		// Optional: X-Signature = hex HMAC-SHA256 of the body. No secret, no check.
		if src.Secret == "" {
			return nil
		}
		sig := strings.TrimPrefix(h.Get("X-Signature"), "sha256=")
		if sig == "" {
			return ErrMissingSignature
		}
		if !equalHex(sig, sign256([]byte(src.Secret), body)) {
			return ErrBadSignature
		}
	default:
		return fmt.Errorf("unknown provider %q", src.Provider)
	}
	return nil
}

// stripe checks "t=<unix>,v1=<hex>[,v1=...]" against HMAC-SHA256("t.body").
func stripe(secret, header string, body []byte) error {
	if header == "" {
		return ErrMissingSignature
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == "" || len(sigs) == 0 {
		return ErrMissingSignature
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if Now().Sub(time.Unix(unix, 0)) > StripeTolerance {
		return ErrStale
	}
	want := sign256([]byte(secret), []byte(ts+"."+string(body)))
	for _, s := range sigs {
		if equalHex(s, want) {
			return nil
		}
	}
	return ErrBadSignature
}

// EventID returns the provider's own ID for this event, used to drop
// duplicates. It falls back to a hash of the body, so an identical retry of a
// payload with no ID is still recognised.
func EventID(provider string, h http.Header, body []byte) string {
	var v struct {
		ID    any    `json:"id"`
		Event string `json:"event"`
		Type  string `json:"event.type"`
		Data  struct {
			ID        any    `json:"id"`
			Reference string `json:"reference"`
			TxRef     string `json:"tx_ref"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &v)
	str := func(x any) string {
		switch t := x.(type) {
		case string:
			return t
		case float64:
			return strconv.FormatFloat(t, 'f', -1, 64)
		}
		return ""
	}
	switch provider {
	case config.Stripe:
		if id := str(v.ID); id != "" {
			return id
		}
	case config.Paystack:
		ref := str(v.Data.ID)
		if ref == "" {
			ref = v.Data.Reference
		}
		if v.Event != "" && ref != "" {
			return v.Event + ":" + ref
		}
	case config.Flutterwave:
		ev := v.Event
		if ev == "" {
			ev = v.Type
		}
		ref := str(v.Data.ID)
		if ref == "" {
			ref = v.Data.TxRef
		}
		if ref != "" {
			return ev + ":" + ref
		}
	default:
		for _, k := range []string{"X-Webhook-Id", "Webhook-Id", "Idempotency-Key", "X-Request-Id"} {
			if id := h.Get(k); id != "" {
				return id
			}
		}
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Outbound signs what the relay sends to your app:
//
//	X-Relay-Timestamp: <unix seconds>
//	X-Relay-Signature: v1=<hex HMAC-SHA256 of "<timestamp>.<body>">
func Outbound(secret string, ts int64, body []byte) string {
	return "v1=" + hex.EncodeToString(sign256([]byte(secret), []byte(strconv.FormatInt(ts, 10)+"."+string(body))))
}

// CheckOutbound is what your app does on receipt (also used in tests).
func CheckOutbound(secret, tsHeader, sigHeader string, body []byte, tolerance time.Duration) error {
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return ErrMissingSignature
	}
	if d := Now().Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return ErrStale
	}
	if !hmac.Equal([]byte(sigHeader), []byte(Outbound(secret, ts, body))) {
		return ErrBadSignature
	}
	return nil
}
