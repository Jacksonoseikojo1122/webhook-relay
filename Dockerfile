FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /relay ./cmd/relay && mkdir -p /out/data

# Distroless: no shell, no package manager, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /relay /relay
# An empty /data the non-root user can write to (SQLite lives here when DATABASE_URL is unset).
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV PORT=8080 RELAY_CONFIG=/config/relay.json RELAY_SQLITE=/data/relay.db
EXPOSE 8080
ENTRYPOINT ["/relay"]
