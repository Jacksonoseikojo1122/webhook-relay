# How Webhook Relay works

![How Webhook Relay works](images/how-it-works.svg)

## Receiving: answer fast, lose nothing

`POST /in/{source}`:

1. **Find the source** in `relay.json`. Unknown source: 404.
2. **Check the signature** with that provider's rules (HMAC-SHA512 for Paystack, `t=,v1=` HMAC-SHA256 with a 5-minute window for Stripe, the shared hash for Flutterwave). Comparisons are constant-time. Bad or missing signature: 401, with no detail about what was wrong.
3. **Work out the provider's event ID**: Stripe's `evt_...`, Paystack's `event:data.id`, Flutterwave's `event:data.id`, or a header or body hash for anything else.
4. **Store it**, unless this source already sent that ID. The database enforces uniqueness on `(source, provider_event_id)`, so two copies arriving at the same moment can't both get in.
5. **Reply 200**: `accepted` for a new event, `duplicate` for one already stored. Either way the provider stops resending. If the database is down, the reply is 503 so the provider tries again later.

Bodies over 1 MB are refused with 413.

## Delivering: at least once, never twice at the same time

A worker polls for due events every second and **claims** a batch by giving each a lease (the delivery timeout plus 30 seconds):

```sql
UPDATE events SET status = 'delivering', lease_until = :now + :lease
WHERE id IN (SELECT id FROM events
             WHERE (status IN ('pending','retrying') AND next_attempt_at <= :now)
                OR (status = 'delivering' AND lease_until < :now)
             ORDER BY next_attempt_at LIMIT :n
             FOR UPDATE SKIP LOCKED)   -- Postgres only
RETURNING ...
```

- On Postgres, `FOR UPDATE SKIP LOCKED` lets several relays share the work without two of them taking the same event.
- If a relay crashes mid-delivery, the lease runs out and the event becomes due again.

Each delivery is a POST of the original body with the `X-Relay-*` headers, signed with the destination secret. The outcome is written in a transaction: one row in `attempts`, and the event moved to its next state:

| Result | Next state |
| --- | --- |
| 2xx | `delivered` |
| Anything else, or no answer within `delivery_timeout` | `retrying`, due after the next backoff step |
| Failed and `attempts` reached `max_attempts` | `dead` |

## Backoff

10s, 30s, 2m, 10m, 30m, then every hour, each plus up to 20% random jitter, so a hundred events that failed together don't all retry in the same second. With the default `max_attempts` of 12, the relay keeps trying for just under 7 hours.

## Replay

Replay sets an event back to `pending` with a fresh retry budget. Its earlier attempts stay in the history. Replays from the dashboard must come from the dashboard's own pages: the relay refuses cross-site form posts, because browsers resend saved login details automatically.

## Observability

- **Dashboard** at `/`: counts by status, recent events, failure reasons, per-event history and payload.
- **Prometheus metrics** at `/metrics`: received, duplicates and signature failures per source; deliveries by result; stored events by status.
- **OpenTelemetry traces** when `OTEL_EXPORTER_OTLP_ENDPOINT` is set: a span for each receive and each delivery, with source, provider, event ID, attempt and status code.
- **JSON logs** on stdout.

## Storage

The same SQL runs on SQLite and Postgres:

- **SQLite** (WAL mode, a single connection) is great for one relay on one machine.
- **Postgres** is for production, several replicas, or hosts without a persistent disk.

Times are stored as Unix milliseconds and bodies as bytes, so nothing depends on database-specific types.
