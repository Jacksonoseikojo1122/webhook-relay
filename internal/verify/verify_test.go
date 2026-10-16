package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Jacksonoseikojo1122/webhook-relay/internal/config"
)

func hexHMAC512(secret, body string) string {
	m := hmac.New(sha512.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func hexHMAC256(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

const paystackBody = `{"event":"charge.success","data":{"id":302961,"reference":"T685312322670591"}}`

func TestPaystack(t *testing.T) {
	src := &config.Source{Provider: config.Paystack, Secret: "sk_test_abc"}
	good := hexHMAC512("sk_test_abc", paystackBody)

	if err := Signature(src, hdr("X-Paystack-Signature", good), []byte(paystackBody)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := Signature(src, hdr("X-Paystack-Signature", good), []byte(paystackBody+" ")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body accepted: %v", err)
	}
	if err := Signature(src, hdr(), []byte(paystackBody)); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("missing header: %v", err)
	}
	if err := Signature(src, hdr("X-Paystack-Signature", "not-hex"), []byte(paystackBody)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("garbage header: %v", err)
	}
}

func TestFlutterwave(t *testing.T) {
	src := &config.Source{Provider: config.Flutterwave, Secret: "my-secret-hash"}
	if err := Signature(src, hdr("Verif-Hash", "my-secret-hash"), nil); err != nil {
		t.Fatal(err)
	}
	if err := Signature(src, hdr("Verif-Hash", "wrong"), nil); !errors.Is(err, ErrBadSignature) {
		t.Fatal("wrong hash accepted")
	}
}

func TestStripe(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	Now = func() time.Time { return now }
	defer func() { Now = time.Now }()

	body := `{"id":"evt_1QX","type":"payment_intent.succeeded"}`
	src := &config.Source{Provider: config.Stripe, Secret: "whsec_test"}
	ts := fmt.Sprint(now.Unix())
	sig := hexHMAC256("whsec_test", ts+"."+body)

	if err := Signature(src, hdr("Stripe-Signature", "t="+ts+",v1="+sig), []byte(body)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	// During secret rotation Stripe sends several v1 values; any match is enough.
	if err := Signature(src, hdr("Stripe-Signature", "t="+ts+",v1=deadbeef,v1="+sig), []byte(body)); err != nil {
		t.Fatalf("rotation: %v", err)
	}
	old := fmt.Sprint(now.Add(-10 * time.Minute).Unix())
	oldSig := hexHMAC256("whsec_test", old+"."+body)
	if err := Signature(src, hdr("Stripe-Signature", "t="+old+",v1="+oldSig), []byte(body)); !errors.Is(err, ErrStale) {
		t.Fatalf("replayed old signature: %v", err)
	}
	if err := Signature(src, hdr("Stripe-Signature", "t="+ts+",v1="+sig), []byte(body+"x")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered: %v", err)
	}
}

func TestGeneric(t *testing.T) {
	open := &config.Source{Provider: config.Generic}
	if err := Signature(open, hdr(), []byte("x")); err != nil {
		t.Fatal("no secret should mean no check")
	}
	locked := &config.Source{Provider: config.Generic, Secret: "s"}
	if err := Signature(locked, hdr("X-Signature", "sha256="+hexHMAC256("s", "x")), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := Signature(locked, hdr(), []byte("x")); !errors.Is(err, ErrMissingSignature) {
		t.Fatal("missing signature accepted")
	}
}

func TestEventID(t *testing.T) {
	cases := []struct {
		provider string
		h        http.Header
		body     string
		want     string
	}{
		{config.Paystack, hdr(), paystackBody, "charge.success:302961"},
		{config.Paystack, hdr(), `{"event":"transfer.success","data":{"reference":"TRF_1"}}`, "transfer.success:TRF_1"},
		{config.Flutterwave, hdr(), `{"event":"charge.completed","data":{"id":285959875,"tx_ref":"x"}}`, "charge.completed:285959875"},
		{config.Stripe, hdr(), `{"id":"evt_1QX","type":"x"}`, "evt_1QX"},
		{config.Generic, hdr("X-Webhook-Id", "wh_9"), `{}`, "wh_9"},
	}
	for _, c := range cases {
		if got := EventID(c.provider, c.h, []byte(c.body)); got != c.want {
			t.Errorf("%s: got %q want %q", c.provider, got, c.want)
		}
	}
	// No ID anywhere: identical bodies still get the same ID.
	a := EventID(config.Generic, hdr(), []byte(`{"x":1}`))
	if a != EventID(config.Generic, hdr(), []byte(`{"x":1}`)) || a == EventID(config.Generic, hdr(), []byte(`{"x":2}`)) {
		t.Error("body hash fallback is not stable")
	}
}

func TestOutboundRoundTrip(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	Now = func() time.Time { return now }
	defer func() { Now = time.Now }()

	body := []byte(`{"ok":true}`)
	sig := Outbound("dest", now.Unix(), body)
	if err := CheckOutbound("dest", fmt.Sprint(now.Unix()), sig, body, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := CheckOutbound("other", fmt.Sprint(now.Unix()), sig, body, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatal("wrong secret accepted")
	}
	if err := CheckOutbound("dest", fmt.Sprint(now.Add(-time.Hour).Unix()), Outbound("dest", now.Add(-time.Hour).Unix(), body), body, 5*time.Minute); !errors.Is(err, ErrStale) {
		t.Fatal("stale delivery accepted")
	}
}
