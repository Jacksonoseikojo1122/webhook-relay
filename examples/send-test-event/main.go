// Sends a signed test webhook to the relay, the way Paystack, Stripe or
// Flutterwave would, so you can try everything locally.
//
//	go run ./examples/send-test-event -provider paystack -source paystack-test -secret sk_test_demo
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

func main() {
	relay := flag.String("relay", "http://localhost:8080", "relay base URL")
	provider := flag.String("provider", "paystack", "paystack | stripe | flutterwave | generic")
	source := flag.String("source", "paystack-test", "source name from relay.json")
	secret := flag.String("secret", "sk_test_demo", "the source's secret")
	id := flag.String("id", "", "event ID (random if empty); reuse one to see duplicate handling")
	amount := flag.Int("amount", 25000, "amount in minor units")
	flag.Parse()

	if *id == "" {
		*id = strconv.Itoa(100000 + rand.IntN(900000))
	}
	var body string
	h := http.Header{"Content-Type": {"application/json"}}
	switch *provider {
	case "paystack":
		body = fmt.Sprintf(`{"event":"charge.success","data":{"id":%s,"reference":"T%s","amount":%d,"currency":"GHS","channel":"mobile_money","status":"success"}}`, *id, *id, *amount)
		m := hmac.New(sha512.New, []byte(*secret))
		m.Write([]byte(body))
		h.Set("X-Paystack-Signature", hex.EncodeToString(m.Sum(nil)))
	case "stripe":
		body = fmt.Sprintf(`{"id":"evt_%s","type":"payment_intent.succeeded","data":{"object":{"amount":%d,"currency":"usd"}}}`, *id, *amount)
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		m := hmac.New(sha256.New, []byte(*secret))
		m.Write([]byte(ts + "." + body))
		h.Set("Stripe-Signature", "t="+ts+",v1="+hex.EncodeToString(m.Sum(nil)))
	case "flutterwave":
		body = fmt.Sprintf(`{"event":"charge.completed","data":{"id":%s,"tx_ref":"tx-%s","amount":%d,"currency":"GHS","status":"successful"}}`, *id, *id, *amount)
		h.Set("Verif-Hash", *secret)
	default:
		body = fmt.Sprintf(`{"type":"order.paid","order_id":"%s","amount":%d}`, *id, *amount)
		h.Set("X-Webhook-Id", "ord_"+*id)
		if *secret != "" {
			m := hmac.New(sha256.New, []byte(*secret))
			m.Write([]byte(body))
			h.Set("X-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
	}

	req, _ := http.NewRequest(http.MethodPost, *relay+"/in/"+*source, bytes.NewBufferString(body))
	req.Header = h
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	fmt.Printf("%d %s", resp.StatusCode, out)
}
