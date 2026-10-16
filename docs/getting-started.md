# Getting started

Ten minutes, start to finish, with nothing to install except Go 1.27+ (or Docker). Everything below runs on your own machine with test secrets, so no real payments are involved.

## 1. Run the relay

```bash
git clone https://github.com/Jacksonoseikojo1122/webhook-relay.git
cd webhook-relay
cp relay.example.json relay.json
```

For this walkthrough, replace `relay.json` with a test setup that points at the example receiver:

```json
{
  "sources": [
    {
      "name": "paystack-test",
      "provider": "paystack",
      "secret": "sk_test_demo",
      "destination": "http://localhost:9090/webhooks/paystack",
      "destination_secret": "dev-app-secret"
    }
  ]
}
```

```bash
RELAY_ADMIN_TOKEN=change-me go run ./cmd/relay
# {"msg":"webhook relay listening","addr":":8080","store":"sqlite","sources":1}
```

It stores events in `relay.db` (SQLite) in the current folder. Set `DATABASE_URL=postgres://...` to use Postgres instead.

## 2. Run an app that receives deliveries

The repo includes a tiny receiver that checks the relay's signature. `FAIL_FIRST=1` makes it fail the first try of every event, so you can watch a retry:

```bash
RELAY_TO_APP_SECRET=dev-app-secret FAIL_FIRST=1 go run ./examples/go-receiver
```

## 3. Send a webhook

`send-test-event` signs a payload exactly the way Paystack does:

```bash
go run ./examples/send-test-event -provider paystack -source paystack-test -secret sk_test_demo -id 4815162
# 200 {"id":"rly_49889f...","status":"accepted"}
```

Send the same ID again. The relay recognises it and doesn't deliver it twice:

```bash
go run ./examples/send-test-event -provider paystack -source paystack-test -secret sk_test_demo -id 4815162
# 200 {"id":"rly_49889f...","status":"duplicate"}
```

Try a wrong secret, which is what a forged request looks like:

```bash
go run ./examples/send-test-event -provider paystack -source paystack-test -secret wrong -id 1
# 401 {"error":"signature check failed"}
```

## 4. Watch it deliver

The receiver says *database is busy* on the first try. About 10 seconds later the relay tries again and the receiver logs:

```
got paystack event rly_49889f... (attempt 2): {"event":"charge.success", ...}
```

Open **http://localhost:8080** (any username, password `change-me`):

![The Webhook Relay dashboard](images/dashboard.png)

Click an event to see every attempt, the payload and a **Replay** button:

![One event: a 503, then delivered on the second try](images/event-delivered.png)

## Next

- [Connect Paystack, Stripe and Flutterwave](integration.md#1-point-your-provider-at-the-relay)
- [Verify deliveries in your app](integration.md#2-verify-deliveries-in-your-app) in Node, Python, PHP or Go
- [Run it in production](operations.md): Postgres, Docker, Render, Kubernetes, metrics and tracing
- [How it works](how-it-works.md)
