package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type metrics struct {
	requests *prometheus.CounterVec
	errors   *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "api_http_requests_total",
			Help: "Total number of HTTP requests.",
		}, []string{"method", "route", "status"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "api_http_errors_total",
			Help: "Total number of HTTP responses with a 5xx status.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "api_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
	}
	reg.MustRegister(m.requests, m.errors, m.duration)
	return m
}

func newHandler(logger *slog.Logger, baseURL string) http.Handler {
	registry := prometheus.NewRegistry()
	m := newMetrics(registry)
	client := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   5 * time.Second,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) {
		err := fmt.Errorf("intentional failure")
		span := trace.SpanFromContext(r.Context())
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("api").Start(r.Context(), "slow-op")
		defer span.End()

		delay := time.Duration(1+rand.Intn(3)) * time.Second
		select {
		case <-time.After(delay):
			writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "delay": delay.String()})
		case <-ctx.Done():
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, ctx.Err().Error())
			http.Error(w, ctx.Err().Error(), http.StatusRequestTimeout)
		}
	})
	mux.HandleFunc("GET /load", func(w http.ResponseWriter, r *http.Request) {
		count := boundedCount(r.URL.Query().Get("count"), 50, 1, 500)
		failures := runLoad(r, client, baseURL, count)
		status := http.StatusOK
		if failures > 0 {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]int{"requests": count, "failures": failures})
	})
	mux.HandleFunc("POST /alerts", func(w http.ResponseWriter, r *http.Request) {
		var notification struct {
			Receiver string `json:"receiver"`
			Status   string `json:"status"`
			Alerts   []struct {
				Status      string            `json:"status"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				Fingerprint string            `json:"fingerprint"`
			} `json:"alerts"`
		}
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		if err := json.NewDecoder(body).Decode(&notification); err != nil {
			http.Error(w, "invalid alertmanager payload", http.StatusBadRequest)
			return
		}
		logger.Info("alertmanager notification",
			"receiver", notification.Receiver,
			"status", notification.Status,
			"alerts", notification.Alerts,
		)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	instrumented := requestMiddleware(logger, m, mux)
	return otelhttp.NewHandler(instrumented, "http.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + routeName(r.URL.Path)
		}),
	)
}

func requestMiddleware(logger *slog.Logger, m *metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		response := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(response, r)

		route := routeName(r.URL.Path)
		status := strconv.Itoa(response.status)
		elapsed := time.Since(started)
		m.requests.WithLabelValues(r.Method, route, status).Inc()
		m.duration.WithLabelValues(r.Method, route).Observe(elapsed.Seconds())
		if response.status >= http.StatusInternalServerError {
			m.errors.WithLabelValues(r.Method, route, status).Inc()
		}

		attrs := []any{
			"method", r.Method,
			"route", route,
			"status", response.status,
			"duration_ms", elapsed.Milliseconds(),
			"trace_id", traceID(r),
		}
		if response.status >= http.StatusInternalServerError {
			logger.Error("request completed", attrs...)
		} else {
			logger.Info("request completed", attrs...)
		}
	})
}

func runLoad(r *http.Request, client *http.Client, baseURL string, count int) int {
	var wg sync.WaitGroup
	results := make(chan bool, count)
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, baseURL+"/health", nil)
			if err != nil {
				results <- false
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				results <- false
				return
			}
			_ = resp.Body.Close()
			results <- resp.StatusCode < http.StatusInternalServerError
		}()
	}
	wg.Wait()
	close(results)

	failures := 0
	for ok := range results {
		if !ok {
			failures++
		}
	}
	return failures
}

func boundedCount(raw string, fallback, min, max int) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func routeName(path string) string {
	switch path {
	case "/health", "/fail", "/slow", "/load", "/alerts", "/metrics":
		return path
	default:
		return "unknown"
	}
}

func traceID(r *http.Request) string {
	spanContext := trace.SpanContextFromContext(r.Context())
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
