package api_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/captain-corgi/go-bot-example/internal/api"
	"github.com/captain-corgi/go-bot-example/internal/problem"
	"github.com/captain-corgi/go-bot-example/internal/session"
	"github.com/captain-corgi/go-bot-example/internal/session/sessiontest"
)

func promptPath(id string) string { return "/sessions/" + id + "/prompts" }

// wireIDs extracts the "id:" values from a raw SSE body in order.
func wireIDs(body string) []string {
	var ids []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "id: ") {
			ids = append(ids, strings.TrimPrefix(line, "id: "))
		}
	}
	return ids
}

func wireEvents(body string) []string {
	var events []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	return events
}

func assertStrings(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPromptStreamsSSEWithHeaders(t *testing.T) {
	h := newHarness(t, harnessConfig{transport: []sessiontest.Option{sessiontest.WithScript(sessiontest.FullTurn)}})
	id := h.createSession(t, "").SessionID

	rec := h.record(http.MethodPost, promptPath(id), `{"prompt":"world"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":  "text/event-stream",
		"Cache-Control": "no-cache",
		"Connection":    "keep-alive",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if !rec.Flushed {
		t.Error("response was never flushed")
	}
	body := rec.Body.String()
	assertStrings(t, wireIDs(body), "1", "2", "3", "4", "5", "6", "7")
	assertStrings(t, wireEvents(body), "session_state", "assistant_chunk", "assistant_chunk",
		"tool_call", "tool_result", "done", "session_state")
	if !strings.HasPrefix(body, "id: 1\nevent: session_state\ndata: {\"state\":\"running\"}\n\n") ||
		!strings.HasSuffix(body, "id: 7\nevent: session_state\ndata: {\"state\":\"idle\"}\n\n") {
		t.Errorf("unexpected stream framing:\n%s", body)
	}

	// The next turn continues the session's sequence.
	rec = h.record(http.MethodPost, promptPath(id), `{"prompt":"again"}`, nil)
	assertStrings(t, wireIDs(rec.Body.String()), "8", "9", "10", "11", "12", "13", "14")
}

func TestPromptReplayAfterCompletedTurn(t *testing.T) {
	h := newHarness(t, harnessConfig{transport: []sessiontest.Option{sessiontest.WithScript(sessiontest.FullTurn)}})
	id := h.createSession(t, "").SessionID
	h.record(http.MethodPost, promptPath(id), `{"prompt":"world"}`, nil)

	rec := h.record(http.MethodPost, promptPath(id), "", map[string]string{"Last-Event-ID": "3"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, wireIDs(rec.Body.String()), "4", "5", "6", "7")
	if got := h.factory.Last().Prompts(); len(got) != 1 {
		t.Errorf("replay started a new turn: prompts %v", got)
	}

	// A Last-Event-ID that is fully caught up is a fresh prompt submission.
	rec = h.record(http.MethodPost, promptPath(id), `{"prompt":"next"}`, map[string]string{"Last-Event-ID": "7"})
	assertStrings(t, wireIDs(rec.Body.String()), "8", "9", "10", "11", "12", "13", "14")
}

func TestPromptValidationErrors(t *testing.T) {
	h := newHarness(t, harnessConfig{transport: []sessiontest.Option{sessiontest.WithScript(sessiontest.FullTurn)}})
	id := h.createSession(t, "").SessionID

	tests := []struct {
		name   string
		path   string
		body   string
		header map[string]string
		status int
	}{
		{"unknown session", promptPath("sess_missing"), `{"prompt":"hi"}`, nil, http.StatusNotFound},
		{"empty body", promptPath(id), "", nil, http.StatusBadRequest},
		{"blank prompt", promptPath(id), `{"prompt":"   "}`, nil, http.StatusBadRequest},
		{"malformed json", promptPath(id), `{"prompt":`, nil, http.StatusBadRequest},
		{"unknown field", promptPath(id), `{"text":"hi"}`, nil, http.StatusBadRequest},
		{"non-numeric Last-Event-ID", promptPath(id), `{"prompt":"hi"}`, map[string]string{"Last-Event-ID": "abc"}, http.StatusBadRequest},
		{"negative Last-Event-ID", promptPath(id), `{"prompt":"hi"}`, map[string]string{"Last-Event-ID": "-1"}, http.StatusBadRequest},
		{"Last-Event-ID ahead of stream", promptPath(id), `{"prompt":"hi"}`, map[string]string{"Last-Event-ID": "99"}, http.StatusBadRequest},
		{"caught-up Last-Event-ID without prompt", promptPath(id), "", map[string]string{"Last-Event-ID": "0"}, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.record(http.MethodPost, tc.path, tc.body, tc.header)
			got := assertProblem(t, rec, tc.status)
			if got.Instance != tc.path {
				t.Errorf("instance = %q, want %q", got.Instance, tc.path)
			}
		})
	}
	if got := h.factory.Last().Prompts(); len(got) != 0 {
		t.Errorf("invalid requests reached the transport: %v", got)
	}
}

func TestPromptTransportSendFailure(t *testing.T) {
	h := newHarness(t, harnessConfig{transport: []sessiontest.Option{sessiontest.WithSendError(errors.New("broken pipe"))}})
	id := h.createSession(t, "").SessionID
	rec := h.record(http.MethodPost, promptPath(id), `{"prompt":"hi"}`, nil)
	got := assertProblem(t, rec, http.StatusInternalServerError)
	if strings.Contains(got.Detail, "broken pipe") {
		t.Errorf("internal error detail leaked: %q", got.Detail)
	}
	s, _ := h.manager.Get(id)
	if s.State() != session.StateIdle {
		t.Errorf("state = %q, want idle after failed send", s.State())
	}
}

// nonFlusher hides http.Flusher from the handler.
type nonFlusher struct{ http.ResponseWriter }

func TestPromptRequiresFlusher(t *testing.T) {
	h := newHarness(t, harnessConfig{transport: []sessiontest.Option{sessiontest.WithScript(sessiontest.FullTurn)}})
	id := h.createSession(t, "").SessionID
	req := httptest.NewRequest(http.MethodPost, promptPath(id), strings.NewReader(`{"prompt":"hi"}`))
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(nonFlusher{rec}, req)
	assertProblem(t, rec, http.StatusInternalServerError)
}

func TestPromptOnFaultedSessionConflicts(t *testing.T) {
	fatal := func(string) []session.SDKEvent {
		return []session.SDKEvent{session.StreamError("subprocess_exit", "agent exited", true)}
	}
	h := newHarness(t, harnessConfig{transport: []sessiontest.Option{sessiontest.WithScript(fatal)}})
	id := h.createSession(t, "").SessionID

	rec := h.record(http.MethodPost, promptPath(id), `{"prompt":"hi"}`, nil)
	assertStrings(t, wireEvents(rec.Body.String()), "session_state", "error", "session_state")
	if !strings.Contains(rec.Body.String(), `"fatal":true`) || !strings.HasSuffix(rec.Body.String(), `{"state":"error"}`+"\n\n") {
		t.Errorf("unexpected fault stream:\n%s", rec.Body.String())
	}

	rec = h.record(http.MethodPost, promptPath(id), `{"prompt":"again"}`, nil)
	got := assertProblem(t, rec, http.StatusConflict)
	if got.Type != problem.TypeConflict || got.Type == api.TypeSessionBusy {
		t.Errorf("type = %q, want %q", got.Type, problem.TypeConflict)
	}
}
