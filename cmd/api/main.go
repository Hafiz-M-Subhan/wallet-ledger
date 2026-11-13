// Command api serves the wallet ledger HTTP API.
//
//	api              run the server
//	api healthcheck  probe /health on localhost (used by the Docker healthcheck,
//	                 since the distroless image has no curl/wget)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"wallet-ledger/internal/httpapi"
	"wallet-ledger/internal/ledger"
	"wallet-ledger/migrations"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// The token only lives in the environment. It is never logged.
	token := os.Getenv("API_TOKEN")
	if token == "" {
		return errors.New("API_TOKEN must be set")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL must be set")
	}
	port := envOr("PORT", "8080")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = int32(envInt("DB_MAX_CONNS", 48))
	cfg.MinConns = int32(envInt("DB_MIN_CONNS", 16))
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 15 * time.Second

	pool, err := connect(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := migrations.Apply(ctx, pool); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	log.Info("schema ready")

	store := ledger.NewStore(pool, ledger.Options{
		StatementTimeout: time.Duration(envInt("WRITE_TIMEOUT_MS", 10_000)) * time.Millisecond,
		MaxAttempts:      envInt("MAX_ATTEMPTS", 5),
		MaxBatch:         envInt("MAX_BATCH", 128),
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           httpapi.New(store, token, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Graceful shutdown: stop accepting, let in-flight requests finish (each
	// one is a single short transaction), then close the pool.
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("stopped")
	return nil
}

// connect retries until the database accepts connections (it may still be
// starting when the API container comes up).
func connect(ctx context.Context, cfg *pgxpool.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = pool.Ping(pingCtx)
			cancel()
			if err == nil {
				return pool, nil
			}
			pool.Close()
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("connect to database: %w", err)
		}
		log.Info("waiting for database", "err", err)
		time.Sleep(time.Second)
	}
}

func healthcheck() int {
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+envOr("PORT", "8080")+"/health", nil)
	req.Header.Set("Authorization", "Bearer "+os.Getenv("API_TOKEN"))
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}
