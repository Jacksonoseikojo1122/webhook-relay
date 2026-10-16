# Integrating Webhook Relay

Two changes: point your payment provider at the relay instead of your app, and have your app check the relay's signature instead of the provider's.

```
Paystack ──▶ relay /in/paystack-live ──▶ your app /webhooks/paystack
                 checks Paystack's            checks the relay's
                 signature                    signature (one format for all providers)
```

## 1. Point your provider at the relay

Add a source to `relay.json` for each provider account, then set the provider's webhook URL to `https://<your-relay>/in/<source-name>`.

| Provider | Where to set the URL | `secret` in relay.json |
| --- | --- | --- |
| **Paystack** | Dashboard → Settings → API Keys & Webhooks → Webhook URL | Your secret key (`sk_live_...` / `sk_test_...`). Paystack signs with HMAC-SHA512 of the body in `x-paystack-signature`. |
| **Stripe** | Dashboard → Developers → Webhooks → Add endpoint | The endpoint's signing secret (`whsec_...`). Checked against `Stripe-Signature`, rejecting signatures older than 5 minutes. |
| **Flutterwave** | Dashboard → Settings → Webhooks | The "Secret hash" you set there. Flutterwave sends it as-is in `verif-hash`. |
| **Anything else** | Wherever it lets you set a URL | Optional. If set, the relay checks `X-Signature: sha256=<hex HMAC-SHA256 of body>`. Duplicates are matched on `X-Webhook-Id` / `Idempotency-Key`, or a hash of the body. |

```json
{
  "sources": [
    { "name": "paystack-live", "provider": "paystack", "secret": "$PAYSTACK_SECRET_KEY",
      "destination": "https://api.your-app.com/webhooks/paystack", "destination_secret": "$RELAY_TO_APP_SECRET" }
  ]
}
```

Values written as `$NAME` are read from environment variables, so `relay.json` can be committed without secrets in it.

## 2. Verify deliveries in your app

Every delivery carries:

| Header | Meaning |
| --- | --- |
| `X-Relay-Signature` | `v1=<hex HMAC-SHA256 of "<timestamp>.<raw body>">` using the source's `destination_secret` |
| `X-Relay-Timestamp` | Unix seconds when this attempt was signed |
| `X-Relay-Event-Id` | The relay's ID for the event. **Same on every retry**: use it as your idempotency key |
| `X-Relay-Attempt` | 1 for the first try, 2 for the first retry, and so on |
| `X-Relay-Source`, `X-Relay-Provider`, `X-Relay-Provider-Event-Id` | Where it came from |

The body is exactly what the provider sent.

Reply with any **2xx** once you've saved the event. Anything else (or no reply within the timeout) makes the relay try again later. Don't do slow work before replying; save the event and process it in the background.

### Node.js / TypeScript (Express)

```ts
import crypto from "node:crypto";
import express from "express";

const SECRET = process.env.RELAY_TO_APP_SECRET!;

function verifyRelay(rawBody: Buffer, ts: string, sig: string) {
  const age = Math.abs(Date.now() / 1000 - Number(ts));
  if (!Number.isFinite(age) || age > 300) return false; // too old: possible replay
  const want = "v1=" + crypto.createHmac("sha256", SECRET).update(`${ts}.${rawBody}`).digest("hex");
  return sig.length === want.length && crypto.timingSafeEqual(Buffer.from(sig), Buffer.from(want));
}

const app = express();
// The signature covers the raw bytes, so read the body raw, not parsed.
app.post("/webhooks/paystack", express.raw({ type: "*/*" }), async (req, res) => {
  if (!verifyRelay(req.body, req.get("x-relay-timestamp") ?? "", req.get("x-relay-signature") ?? "")) {
    return res.status(401).send("bad signature");
  }
  const eventId = req.get("x-relay-event-id")!;
  const event = JSON.parse(req.body.toString("utf8"));

  // Idempotent: a retry of something you already saved is a no-op.
  await db.webhookEvent.upsert({ where: { id: eventId }, create: { id: eventId, payload: event }, update: {} });
  res.sendStatus(200);
  queue.add("process-payment", { eventId }); // the slow work happens after the reply
});
```

### Python (FastAPI)

```python
import hashlib, hmac, os, time
from fastapi import FastAPI, Header, HTTPException, Request

SECRET = os.environ["RELAY_TO_APP_SECRET"].encode()
app = FastAPI()

def verify_relay(raw: bytes, ts: str, sig: str) -> bool:
    try:
        if abs(time.time() - int(ts)) > 300:
            return False
    except ValueError:
        return False
    want = "v1=" + hmac.new(SECRET, f"{ts}.".encode() + raw, hashlib.sha256).hexdigest()
    return hmac.compare_digest(sig, want)

@app.post("/webhooks/paystack")
async def paystack(request: Request,
                   x_relay_timestamp: str = Header(""), x_relay_signature: str = Header(""),
                   x_relay_event_id: str = Header("")):
    raw = await request.body()
    if not verify_relay(raw, x_relay_timestamp, x_relay_signature):
        raise HTTPException(401, "bad signature")
    save_once(x_relay_event_id, raw)   # ignore if already saved
    return {"ok": True}
```

### PHP (Laravel)

```php
Route::post('/webhooks/paystack', function (Request $request) {
    $ts = $request->header('X-Relay-Timestamp', '');
    $sig = $request->header('X-Relay-Signature', '');
    $raw = $request->getContent();

    $want = 'v1=' . hash_hmac('sha256', $ts . '.' . $raw, env('RELAY_TO_APP_SECRET'));
    if (abs(time() - (int) $ts) > 300 || !hash_equals($want, $sig)) {
        abort(401, 'bad signature');
    }

    WebhookEvent::firstOrCreate(['id' => $request->header('X-Relay-Event-Id')], ['payload' => $raw]);
    return response('ok');
});
```

### Go

See [`examples/go-receiver/main.go`](../examples/go-receiver/main.go): about 20 lines with no dependencies.

## 3. Handle failures

- **A delivery failed**: nothing to do. The relay retries after 10s, 30s, 2m, 10m, 30m, then hourly, with a little random jitter.
- **An event is dead** (`max_attempts` reached, 12 by default, which is a bit under 7 hours of trying): fix whatever was wrong in your app, then press **Replay** on the dashboard, or:

  ```bash
  curl -X POST -H "Authorization: Bearer $RELAY_ADMIN_TOKEN" https://<relay>/api/events/<id>/replay
  ```

- **Find dead events from a script or alert**: `GET /api/events?status=dead`, or alert on the `relay_events{status="dead"}` metric.

## Things to know

- The relay answers the provider within milliseconds, after the event is safely stored. That alone stops most "webhook timed out" retries from providers.
- Delivery is **at least once**. A network failure *after* your app saved an event but before the relay saw the 200 means it will arrive again. That's why `X-Relay-Event-Id` exists: make your handler idempotent.
- Events from one source can arrive out of order (a retried event can land after a newer one). If order matters, compare timestamps or statuses in the payload.
- The relay stores webhook bodies. They can contain customer data, so protect the database and the admin token accordingly.
