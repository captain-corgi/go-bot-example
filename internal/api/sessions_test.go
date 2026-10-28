package api_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/captain-corgi/go-bot-example/internal/problem"
	"github.com/captain-corgi/go-bot-example/internal/session/sessiontest"
)

func TestCreateSessionEmptyBody(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	rec := h.record(http.MethodPost, "/sessions", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	out := h.createSession(t, "{}")
	if out.SessionID == "" || out.Status != "idle" {
		t.Errorf("response = %+v, want non-empty id and idle", out)
	}
	if n := len(h.factory.Transports()); n != 2 {
		t.Errorf("transports = %d, want one per created session", n)
	}
}

func TestCreateSessionWithPromptAndConfig(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	out := h.createSession(t, `{"prompt":"be terse","config":{"model":"grok","max_turns":3}}`)
	if out.Status != "idle" {
		t.Errorf("status = %q, want idle", out.Status)
	}
	s, ok := h.manager.Get(out.SessionID)
	if !ok {
		t.Fatal("created session not registered")
	}
	cfg, _ := h.factory.Last().StartConfig()
	if cfg.SessionID != s.ID() || cfg.InitialPrompt != "be terse" || cfg.Options["model"] != "grok" {
		t.Errorf("start config = %+v", cfg)
	}
}

func TestCreateSessionResumesExisting(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	created := h.createSession(t, "")
	resumed := h.createSession(t, `{"session_id":"`+created.SessionID+`"}`)
	if resumed.SessionID != created.SessionID || resumed.Status != "idle" {
		t.Errorf("resumed = %+v, want %+v", resumed, created)
	}
	if n := len(h.factory.Transports()); n != 1 {
		t.Errorf("transports = %d, want 1 (resume reattaches)", n)
	}
}

func TestCreateSessionErrors(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		ptype  string
	}{
		{"unknown session", `{"session_id":"sess_does_not_exist"}`, http.StatusNotFound, problem.TypeNotFound},
		{"malformed json", `{"prompt":`, http.StatusBadRequest, problem.TypeBadRequest},
		{"not an object", `["prompt"]`, http.StatusBadRequest, problem.TypeBadRequest},
		{"unknown field", `{"sessionId":"x"}`, http.StatusBadRequest, problem.TypeBadRequest},
		{"config not object", `{"config":"fast"}`, http.StatusBadRequest, problem.TypeBadRequest},
		{"trailing data", `{} {}`, http.StatusBadRequest, problem.TypeBadRequest},
		{"invalid session id", `{"session_id":"../../etc/passwd"}`, http.StatusBadRequest, problem.TypeBadRequest},
		{"oversized body", `{"prompt":"` + strings.Repeat("a", 1<<20) + `"}`, http.StatusBadRequest, problem.TypeBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessConfig{})
			rec := h.record(http.MethodPost, "/sessions", tc.body, nil)
			got := assertProblem(t, rec, tc.status)
			if got.Type != tc.ptype || got.Instance != "/sessions" {
				t.Errorf("problem = %+v", got)
			}
		})
	}
}

func TestCreateSessionTransportFailure(t *testing.T) {
	h := newHarness(t, harnessConfig{transport: []sessiontest.Option{
		sessiontest.WithStartError(errors.New("claude binary not found")),
	}})
	rec := h.record(http.MethodPost, "/sessions", "", nil)
	got := assertProblem(t, rec, http.StatusInternalServerError)
	if strings.Contains(got.Detail, "claude binary") {
		t.Errorf("internal error detail leaked: %q", got.Detail)
	}
}
