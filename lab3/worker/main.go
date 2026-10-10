package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type worker struct {
	db        *pgxpool.Pool
	processed atomic.Uint64
	failures  atomic.Uint64
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		logger.Error("create database pool", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := initialiseDatabase(startupCtx, db); err != nil {
		cancel()
		logger.Error("initialise database", "error", err)
		os.Exit(1)
	}
	cancel()

	w := &worker{db: db}
	server := &http.Server{
		Addr:              envOrDefault("HTTP_ADDR", ":8080"),
		Handler:           w.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go w.run(ctx, logger)
	go func() {
		logger.Info("worker health server started", "address", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "error", err)
	}
}

func initialiseDatabase(ctx context.Context, db *pgxpool.Pool) error {
	if err := db.Ping(ctx); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS orders (
			id BIGSERIAL PRIMARY KEY,
			payload JSONB NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			processed_at TIMESTAMPTZ
		)
	`)
	return err
}

func (w *worker) run(ctx context.Context, logger *slog.Logger) {
	interval, err := time.ParseDuration(envOrDefault("POLL_INTERVAL", "2s"))
	if err != nil || interval <= 0 {
		logger.Error("invalid POLL_INTERVAL", "value", os.Getenv("POLL_INTERVAL"))
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := w.processOne(ctx, logger); err != nil && !errors.Is(err, context.Canceled) {
			w.failures.Add(1)
			logger.Error("process order", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *worker) processOne(ctx context.Context, logger *slog.Logger) error {
	var id int64
	err := w.db.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id
			FROM orders
			WHERE status = 'pending'
			ORDER BY id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE orders
		SET status = 'processed', processed_at = now()
		FROM candidate
		WHERE orders.id = candidate.id
		RETURNING orders.id
	`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	w.processed.Add(1)
	logger.Info("order processed", "order_id", id)
	return nil
}

func (w *worker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(response http.ResponseWriter, request *http.Request) {
		if strings.EqualFold(os.Getenv("HEALTH_FAIL"), "true") {
			http.Error(response, "forced health failure", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		if err := w.db.Ping(ctx); err != nil {
			http.Error(response, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /metrics", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintf(response,
			"# TYPE shop_worker_processed_orders_total counter\nshop_worker_processed_orders_total %d\n"+
				"# TYPE shop_worker_failures_total counter\nshop_worker_failures_total %d\n",
			w.processed.Load(), w.failures.Load(),
		)
	})
	return mux
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
