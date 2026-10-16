// Command relay runs Webhook Relay: it receives webhooks, stores them, and
// delivers them to your app with retries.
//
//	RELAY_CONFIG=relay.json RELAY_ADMIN_TOKEN=... go run ./cmd/relay
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Jacksonoseikojo1122/webhook-relay/internal/config"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/deliver"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/server"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/store"
	"github.com/Jacksonoseikojo1122/webhook-relay/internal/telemetry"
)

const version = "0.1.0"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("relay stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(env("RELAY_CONFIG", "relay.json"))
	if err != nil {
		return err
	}

	// DATABASE_URL (postgres://...) wins; otherwise a local SQLite file.
	driver, dsn := "sqlite", env("RELAY_SQLITE", "relay.db")
	if u := os.Getenv("DATABASE_URL"); u != "" {
		driver, dsn = "postgres", u
	}
	st, err := store.Open(driver, dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	shutdownTracing, err := telemetry.Setup(ctx, version)
	if err != nil {
		return err
	}
	defer shutdownTracing(context.Background())

	metrics := &deliver.Metrics{}
	worker := &deliver.Worker{Store: st, Config: cfg, Log: log, Metrics: metrics}
	go worker.Run(ctx)

	srv := &server.Server{Store: st, Config: cfg, AdminToken: os.Getenv("RELAY_ADMIN_TOKEN"), Log: log, Metrics: metrics}
	httpSrv := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("webhook relay listening", "addr", httpSrv.Addr, "store", driver, "sources", len(cfg.Sources), "version", version)
	if os.Getenv("RELAY_ADMIN_TOKEN") == "" {
		log.Warn("RELAY_ADMIN_TOKEN is not set: the dashboard and admin API are switched off")
	}
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
