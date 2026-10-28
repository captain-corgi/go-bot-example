package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/captain-corgi/go-bot-example/internal/problem"
)

const testBoundPort = 8787

func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, status int) problem.Details {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, problem.ContentType)
	}
	var got problem.Details
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode problem body %q: %v", rec.Body.String(), err)
	}
	if got.Status != status || got.Type == "" || got.Title == "" || got.Detail == "" || got.Instance == "" {
		t.Errorf("incomplete problem details: %+v", got)
	}
	return got
}

func TestHostValidationMiddlewareAllows(t *testing.T) {
	port := strconv.Itoa(testBoundPort)
	for _, host := range []string{"127.0.0.1", "localhost", "LOCALHOST", "127.0.0.1:" + port, "localhost:" + port} {
		t.Run(host, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
			req.Host = host
			rec := httptest.NewRecorder()

			HostValidationMiddleware(testBoundPort, next).ServeHTTP(rec, req)

			if !called || rec.Code != http.StatusNoContent {
				t.Errorf("request with Host %q was not passed through (status %d)", host, rec.Code)
			}
		})
	}
}

func TestHostValidationMiddlewareRejects(t *testing.T) {
	port := strconv.Itoa(testBoundPort)
	hosts := []string{
		"",
		"evil.com",
		"evil.com:" + port,
		"localhost.evil.com",
		"127.0.0.1.evil.com",
		"127.0.0.1.attacker.com:" + port,
		"0.0.0.0:" + port,
		"0.0.0.0",
		"192.168.1.10:" + port,
		"127.0.0.2",
		"127.0.0.1:9999",
		"localhost:9999",
		"[::1]:" + port,
		"[::1]",
		"::1",
		"localhost:",
		"localhost:abc",
		"localhost.",
	}
	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Errorf("next handler invoked for Host %q", host)
			})
			req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
			req.Host = host
			rec := httptest.NewRecorder()

			HostValidationMiddleware(testBoundPort, next).ServeHTTP(rec, req)

			got := assertProblem(t, rec, http.StatusBadRequest)
			if got.Type != problem.TypeBadRequest {
				t.Errorf("type = %q, want %q", got.Type, problem.TypeBadRequest)
			}
		})
	}
}

func TestHostValidationMiddlewareUnboundPortAcceptsAnyValidPort(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:1234"
	rec := httptest.NewRecorder()

	HostValidationMiddleware(0, next).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestRouterFallbackNotFound(t *testing.T) {
	rt := NewRouter()
	rt.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	rt.Handle("GET /health", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	tests := []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/sessions", http.StatusOK},
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodGet, "/sessions", http.StatusNotFound},
		{http.MethodGet, "/unknown", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if tc.status == http.StatusNotFound {
				got := assertProblem(t, rec, http.StatusNotFound)
				if got.Instance != tc.path {
					t.Errorf("instance = %q, want %q", got.Instance, tc.path)
				}
				return
			}
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
}
