package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/api"
	"github.com/captain-corgi/go-bot-example/internal/problem"
	"github.com/captain-corgi/go-bot-example/internal/server"
	"github.com/captain-corgi/go-bot-example/internal/session"
	"github.com/captain-corgi/go-bot-example/internal/session/sessiontest"
)

const frameTimeout = 5 * time.Second

type harnessConfig struct {
	transport []sessiontest.Option
	store     session.Store
	grace     time.Duration
}

type harness struct {
	factory *sessiontest.MockFactory
	manager *session.Manager
	handler http.Handler
	srv     *httptest.Server
}

// newHarness wires a Manager backed by a MockFactory into the daemon router
// behind the Host validation middleware. The live server is started lazily.
func newHarness(t *testing.T, cfg harnessConfig) *harness {
	t.Helper()
	factory := sessiontest.NewMockFactory(cfg.transport...)
	manager, err := session.NewManager(session.Options{
		NewTransport:    factory.New,
		Store:           cfg.store,
		DisconnectGrace: cfg.grace,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rt := server.NewRouter()
	api.NewHandler(manager).Register(rt)
	return &harness{factory: factory, manager: manager, handler: server.HostValidationMiddleware(0, rt)}
}

func (h *harness) server(t *testing.T) *httptest.Server {
	t.Helper()
	if h.srv == nil {
		h.srv = httptest.NewServer(h.handler)
		t.Cleanup(h.srv.Close)
	}
	return h.srv
}

// record serves a request in-process and returns the recorded response.
func (h *harness) record(method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1"
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

type createResponse struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

func (h *harness) createSession(t *testing.T, body string) createResponse {
	t.Helper()
	rec := h.record(http.MethodPost, "/sessions", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /sessions status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out createResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create response %q: %v", rec.Body.String(), err)
	}
	return out
}

func decodeProblem(t *testing.T, status int, contentType string, body []byte, wantStatus int) problem.Details {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", status, wantStatus, body)
	}
	if contentType != problem.ContentType {
		t.Fatalf("Content-Type = %q, want %q", contentType, problem.ContentType)
	}
	var d problem.Details
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("decode problem %s: %v", body, err)
	}
	if d.Status != wantStatus || d.Type == "" || d.Title == "" || d.Detail == "" || d.Instance == "" {
		t.Fatalf("incomplete problem details: %+v", d)
	}
	return d
}

func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) problem.Details {
	t.Helper()
	return decodeProblem(t, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes(), wantStatus)
}

// sseFrame is a parsed SSE frame as seen by a client.
type sseFrame struct {
	ID    string
	Event string
	Data  map[string]any
}

// label renders a frame as "event[:state|code]" for compact assertions.
func (f sseFrame) label() string {
	switch f.Event {
	case "session_state":
		return f.Event + ":" + asString(f.Data["state"])
	case "error":
		return f.Event + ":" + asString(f.Data["code"])
	default:
		return f.Event
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// sseStream is a client-side SSE connection read on a background goroutine.
type sseStream struct {
	resp   *http.Response
	frames chan sseFrame
	cancel context.CancelFunc
}

// openStream POSTs a prompt request and returns the open SSE stream. It fails
// t unless the response is a 200 event stream.
func (h *harness) openStream(t *testing.T, sessionID, body, lastEventID string) *sseStream {
	t.Helper()
	resp, cancel := h.postPrompt(t, sessionID, body, lastEventID)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		cancel()
		t.Fatalf("prompt status = %d, body %s", resp.StatusCode, b)
	}
	s := &sseStream{resp: resp, frames: make(chan sseFrame, 64), cancel: cancel}
	go s.read()
	t.Cleanup(s.close)
	return s
}

func (h *harness) postPrompt(t *testing.T, sessionID, body, lastEventID string) (*http.Response, context.CancelFunc) {
	t.Helper()
	srv := h.server(t)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/sessions/"+sessionID+"/prompts", strings.NewReader(body))
	if err != nil {
		cancel()
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("POST prompts: %v", err)
	}
	return resp, func() {
		cancel()
		_ = resp.Body.Close()
	}
}

func (s *sseStream) read() {
	defer close(s.frames)
	scanner := bufio.NewScanner(s.resp.Body)
	var cur sseFrame
	var data string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := json.Unmarshal([]byte(data), &cur.Data); err != nil {
				cur.Event = "invalid_json:" + cur.Event
			}
			s.frames <- cur
			cur, data = sseFrame{}, ""
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// next returns the next frame, failing t on timeout or end of stream.
func (s *sseStream) next(t *testing.T) sseFrame {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if !ok {
			t.Fatal("stream ended; expected another frame")
		}
		return f
	case <-time.After(frameTimeout):
		t.Fatal("timed out waiting for SSE frame")
		return sseFrame{}
	}
}

// expect reads one frame per label and asserts the sequence.
func (s *sseStream) expect(t *testing.T, labels ...string) []sseFrame {
	t.Helper()
	got := make([]sseFrame, 0, len(labels))
	for _, want := range labels {
		f := s.next(t)
		if f.label() != want {
			t.Fatalf("frame %d = %s (id %s), want %s", len(got), f.label(), f.ID, want)
		}
		got = append(got, f)
	}
	return got
}

// expectEnd asserts the server closes the stream with no further frames.
func (s *sseStream) expectEnd(t *testing.T) {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if ok {
			t.Fatalf("unexpected frame %s (id %s); expected end of stream", f.label(), f.ID)
		}
	case <-time.After(frameTimeout):
		t.Fatal("stream did not end")
	}
}

// close disconnects the client.
func (s *sseStream) close() {
	s.cancel()
}

// waitFor polls cond until it holds, failing t after frameTimeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(frameTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
