package main

import (
	"context"
	"encoding/json"
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

	"github.com/jackc/pgx/v5/pgxpool"
)

type application struct {
	db       *pgxpool.Pool
	requests atomic.Uint64
	errors   atomic.Uint64
}

type order struct {
	ID          int64           `json:"id"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	ProcessedAt *time.Time      `json:"processed_at,omitempty"`
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		logger.Error("create database pool", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := initialiseDatabase(startupCtx, db); err != nil {
		logger.Error("initialise database", "error", err)
		os.Exit(1)
	}

	app := &application{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", app.health)
	mux.HandleFunc("POST /order", app.createOrder)
	mux.HandleFunc("GET /orders", app.listOrders)
	mux.HandleFunc("GET /metrics", app.metrics)

	server := &http.Server{
		Addr:              envOrDefault("HTTP_ADDR", ":8080"),
		Handler:           app.countRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("api started", "address", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

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

func (app *application) health(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(os.Getenv("HEALTH_FAIL"), "true") {
		app.errors.Add(1)
		http.Error(w, "forced health failure", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := app.db.Ping(ctx); err != nil {
		app.errors.Add(1)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (app *application) createOrder(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var payload json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil || !json.Valid(payload) {
		app.errors.Add(1)
		http.Error(w, "request body must contain JSON", http.StatusBadRequest)
		return
	}

	created := order{Payload: payload}
	err := app.db.QueryRow(r.Context(), `
		INSERT INTO orders (payload)
		VALUES ($1::jsonb)
		RETURNING id, status, created_at
	`, string(payload)).Scan(&created.ID, &created.Status, &created.CreatedAt)
	if err != nil {
		app.errors.Add(1)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, created)
}

func (app *application) listOrders(w http.ResponseWriter, r *http.Request) {
	rows, err := app.db.Query(r.Context(), `
		SELECT id, payload, status, created_at, processed_at
		FROM orders
		ORDER BY id
	`)
	if err != nil {
		app.errors.Add(1)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	orders := make([]order, 0)
	for rows.Next() {
		var item order
		if err := rows.Scan(&item.ID, &item.Payload, &item.Status, &item.CreatedAt, &item.ProcessedAt); err != nil {
			app.errors.Add(1)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		orders = append(orders, item)
	}
	if err := rows.Err(); err != nil {
		app.errors.Add(1)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, orders)
}

func (app *application) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintf(w,
		"# TYPE shop_api_http_requests_total counter\nshop_api_http_requests_total %d\n"+
			"# TYPE shop_api_http_errors_total counter\nshop_api_http_errors_total %d\n",
		app.requests.Load(), app.errors.Load(),
	)
}

func (app *application) countRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app.requests.Add(1)
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
