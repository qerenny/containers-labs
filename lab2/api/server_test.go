package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCoreEndpointsAndMetrics(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	server := httptest.NewServer(newHandler(logger, ""))
	t.Cleanup(server.Close)

	assertResponse(t, server.URL+"/health", http.StatusOK, "ok")
	assertResponse(t, server.URL+"/fail", http.StatusInternalServerError, "intentional failure")

	resp, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	metrics := string(body)
	for _, name := range []string{
		"api_http_requests_total",
		"api_http_errors_total",
		"api_http_request_duration_seconds_bucket",
	} {
		if !strings.Contains(metrics, name) {
			t.Errorf("metrics output does not contain %q", name)
		}
	}
}

func TestLoadEndpoint(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	var handler http.Handler
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	handler = newHandler(logger, server.URL)

	assertResponse(t, server.URL+"/load?count=5", http.StatusOK, `"requests":5`)
}

func TestAlertmanagerWebhook(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	server := httptest.NewServer(newHandler(logger, ""))
	t.Cleanup(server.Close)

	payload := `{"receiver":"api-webhook","status":"firing","alerts":[{"status":"firing","labels":{"alertname":"ApiDown"},"fingerprint":"abc123"}]}`
	resp, err := http.Post(server.URL+"/alerts", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /alerts status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if !strings.Contains(logs.String(), `"msg":"alertmanager notification"`) ||
		!strings.Contains(logs.String(), `"alertname":"ApiDown"`) {
		t.Fatalf("webhook log does not contain notification: %s", logs.String())
	}
}

func assertResponse(t *testing.T, url string, wantStatus int, wantBody string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s status = %d, want %d", url, resp.StatusCode, wantStatus)
	}
	if !strings.Contains(string(body), wantBody) {
		t.Fatalf("GET %s body = %q, want substring %q", url, body, wantBody)
	}
}
