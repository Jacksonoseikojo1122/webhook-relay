# Running Webhook Relay in production

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `RELAY_CONFIG` | `relay.json` | Path to the sources file |
| `DATABASE_URL` | none | `postgres://...`. If unset, SQLite is used |
| `RELAY_SQLITE` | `relay.db` | SQLite file path |
| `RELAY_ADMIN_TOKEN` | none | Required for the dashboard and `/api/*`. Without it they're switched off |
| `PORT` | `8080` | HTTP port |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | none | Turns on tracing, e.g. `http://localhost:4318` |

In `relay.json`: `max_attempts` (default 12) and `delivery_timeout` (default `15s`).

## Docker

```bash
docker build -t webhook-relay .
docker run -p 8080:8080 \
  -v $PWD/relay.json:/config/relay.json:ro -v relay-data:/data \
  -e RELAY_ADMIN_TOKEN=... -e PAYSTACK_SECRET_KEY=... -e RELAY_TO_APP_SECRET=... \
  webhook-relay
```

The image is distroless, runs as non-root and has no shell. Tagged releases are published to `ghcr.io/jacksonoseikojo1122/webhook-relay`.

## Render (free tier)

`render.yaml` deploys the Docker image as a free web service:

1. Create a free Postgres on Render or [Neon](https://neon.tech) and copy its URL. Render's free web service has no persistent disk, so SQLite would be wiped on restart.
2. New → Blueprint → this repo. Set `DATABASE_URL`.
3. Add `relay.json` as a Secret File named `relay.json`. It's mounted at `/etc/secrets/relay.json`.
4. Add the secrets your `relay.json` references (`PAYSTACK_SECRET_KEY`, ...) as environment variables.

Free services sleep when idle and take a few seconds to wake. Providers retry slow first requests, so nothing is lost, but use a paid instance for real traffic.

## Kubernetes

`deploy/k8s/relay.yaml` runs two replicas against one Postgres, with probes on `/healthz`, Prometheus scrape annotations, a read-only root filesystem and resource limits:

```bash
kubectl create secret generic webhook-relay \
  --from-literal=DATABASE_URL=postgres://... \
  --from-literal=RELAY_ADMIN_TOKEN=... \
  --from-file=relay.json=./relay.json
kubectl apply -f deploy/k8s/relay.yaml
```

The replicas split deliveries between them through `FOR UPDATE SKIP LOCKED`. No event is delivered by two replicas at once.

## Alerts worth setting

| Alert | PromQL |
| --- | --- |
| Something is dead | `relay_events{status="dead"} > 0` |
| Your app is failing | `rate(relay_deliveries_total{result="failed"}[10m]) > 0.1` |
| Backlog growing | `relay_events{status="retrying"} > 50` |
| Someone sending forged requests, or a rotated secret | `increase(relay_signature_failures_total[15m]) > 5` |

## Tracing

Point `OTEL_EXPORTER_OTLP_ENDPOINT` at any OTLP/HTTP collector (Jaeger, Grafana Tempo, Honeycomb):

```bash
docker run -d -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 go run ./cmd/relay
```

## Security checklist

- Put the relay behind HTTPS. Providers require it for live webhooks.
- Use a long random `RELAY_ADMIN_TOKEN` and a different random `destination_secret` per app.
- Keep secrets in environment variables and reference them as `$NAME` in `relay.json`.
- The database holds raw webhook bodies, which can include customer names, emails and amounts. Restrict access and set a retention policy that fits your obligations.
