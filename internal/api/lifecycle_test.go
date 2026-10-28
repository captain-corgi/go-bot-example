package api_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/api"
	"github.com/captain-corgi/go-bot-example/internal/session"
	"github.com/captain-corgi/go-bot-example/internal/session/sessiontest"
)

// interruptBudget is the brief's interrupt latency requirement.
const interruptBudget = 500 * time.Millisecond

type liveResponse struct {
	status      int
	contentType string
	body        []byte
}

// doLive sends a request to the live loopback test server.
func (h *harness) doLive(t *testing.T, method, path, body string) liveResponse {
	t.Helper()
	resp, err := fetch(h.server(t), method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// fetch performs a live request without touching testing.T, so it is safe
// to call from spawned goroutines.
func fetch(srv *httptest.Server, method, path, body string) (liveResponse, error) {
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		return liveResponse{}, err
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		return liveResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return liveResponse{}, err
	}
	return liveResponse{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: b}, nil
}

func (r liveResponse) assertStatus(t *testing.T, want string) {
	t.Helper()
	if r.status != http.StatusOK || r.contentType != "application/json" {
		t.Fatalf("status %d Content-Type %q body %s; want 200 JSON", r.status, r.contentType, r.body)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil || out.Status != want {
		t.Fatalf("body %s; want {\"status\": %q}", r.body, want)
	}
}

func (r liveResponse) assertProblem(t *testing.T, wantStatus int) {
	t.Helper()
	decodeProblem(t, r.status, r.contentType, r.body, wantStatus)
}

func interruptPath(id string) string { return "/sessions/" + id + "/interrupt" }
func sessionPath(id string) string   { return "/sessions/" + id }

// timedInterrupt interrupts over HTTP and asserts the 500ms budget.
func (h *harness) timedInterrupt(t *testing.T, id string) {
	t.Helper()
	start := time.Now()
	resp := h.doLive(t, http.MethodPost, interruptPath(id), "")
	if took := time.Since(start); took >= interruptBudget {
		t.Fatalf("interrupt took %v, want < %v", took, interruptBudget)
	}
	resp.assertStatus(t, api.StatusInterrupted)
}

func TestScenarioCInterruptReuseAndTeardown(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	id := h.createLive(t, "").SessionID
	s, _ := h.manager.Get(id)

	stream := h.openStream(t, id, `{"prompt":"refactor everything"}`, "")
	stream.expect(t, "session_state:running")
	mock := h.factory.Last().NextTurn(t)
	mock.MustEmit(t, session.AssistantChunk("Starting"))
	stream.expect(t, "assistant_chunk")

	h.timedInterrupt(t, id)
	stream.expect(t, "session_state:interrupted", "session_state:idle")
	stream.expectEnd(t)
	if s.State() != session.StateIdle || mock.Context().Err() == nil {
		t.Fatalf("state %q turn ctx err %v; want idle with the turn cancelled", s.State(), mock.Context().Err())
	}

	// The same session and subprocess accept the follow-up prompt.
	next := h.openStream(t, id, `{"prompt":"just the tests"}`, "")
	next.expect(t, "session_state:running")
	h.factory.Last().NextTurn(t).MustEmit(t, session.AssistantChunk("ok"), session.Done(3, 1))
	next.expect(t, "assistant_chunk", "done", "session_state:idle")
	next.expectEnd(t)
	transport := h.factory.Last()
	if len(h.factory.Transports()) != 1 || transport.CloseCount() != 0 {
		t.Fatalf("transports %d closes %d; interrupt must keep the subprocess alive",
			len(h.factory.Transports()), transport.CloseCount())
	}

	h.doLive(t, http.MethodDelete, sessionPath(id), "").assertStatus(t, api.StatusClosed)
	if transport.CloseCount() != 1 || !transport.Exited() || s.Events().Len() != 0 {
		t.Fatalf("closes %d exited %v buffered %d; want terminated subprocess and purged buffer",
			transport.CloseCount(), transport.Exited(), s.Events().Len())
	}

	h.doLive(t, http.MethodDelete, sessionPath(id), "").assertProblem(t, http.StatusNotFound)
	h.doLive(t, http.MethodPost, interruptPath(id), "").assertProblem(t, http.StatusNotFound)
	h.doLive(t, http.MethodPost, promptPath(id), `{"prompt":"again"}`).assertProblem(t, http.StatusNotFound)
}

func TestInterruptEscalatesToProcessGroupOverHTTP(t *testing.T) {
	signals := &sessiontest.SignalRecorder{}
	h := newHarness(t, harnessConfig{
		transport: []sessiontest.Option{sessiontest.WithPGID(31337), sessiontest.WithIgnoredInterrupts()},
		options:   func(o *session.Options) { o.SignalProcessGroup = signals.Signal },
	})
	signals.OnSignal = func(sessiontest.GroupSignal) { h.factory.Last().FinishOpenTurns() }
	id := h.createLive(t, "").SessionID

	stream := h.openStream(t, id, `{"prompt":"run a slow tool"}`, "")
	stream.expect(t, "session_state:running")
	h.factory.Last().NextTurn(t)

	h.timedInterrupt(t, id)
	stream.expect(t, "session_state:interrupted", "session_state:idle")
	stream.expectEnd(t)
	want := sessiontest.GroupSignal{PGID: 31337, Signal: syscall.SIGINT}
	if got := signals.Signals(); len(got) != 1 || got[0] != want {
		t.Fatalf("signals = %v, want [%v]", got, want)
	}
}

func TestRapidAndIdleInterruptsAreIdempotent(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	id := h.createLive(t, "").SessionID

	// Interrupting an idle session is a safe no-op.
	h.timedInterrupt(t, id)

	stream := h.openStream(t, id, `{"prompt":"go"}`, "")
	stream.expect(t, "session_state:running")
	h.factory.Last().NextTurn(t)

	const callers = 16
	srv := h.server(t)
	var wg sync.WaitGroup
	results := make(chan liveResponse, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := fetch(srv, http.MethodPost, interruptPath(id), "{}")
			if err != nil {
				errs <- err
				return
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent interrupt: %v", err)
	}
	for r := range results {
		r.assertStatus(t, api.StatusInterrupted)
	}
	stream.expect(t, "session_state:interrupted", "session_state:idle")
	stream.expectEnd(t)
	if n := h.factory.Last().InterruptCount(); n != 1 {
		t.Errorf("transport interrupts = %d, want 1", n)
	}

	// Late repeats after the turn ended stay no-ops.
	for i := 0; i < 3; i++ {
		h.timedInterrupt(t, id)
	}
	if s, _ := h.manager.Get(id); s.State() != session.StateIdle {
		t.Errorf("state = %q, want idle", s.State())
	}
}

func TestSubprocessCrashStreamsFatalErrorAndRecovers(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	id := h.createLive(t, "").SessionID

	stream := h.openStream(t, id, `{"prompt":"go"}`, "")
	stream.expect(t, "session_state:running")
	h.factory.Last().NextTurn(t).MustEmit(t, session.AssistantChunk("partial"))
	stream.expect(t, "assistant_chunk")

	h.factory.Last().Crash(errors.New("signal: segmentation fault"))
	frames := stream.expect(t, "error:subprocess_exit", "session_state:error")
	stream.expectEnd(t)
	if frames[0].Data["fatal"] != true || !strings.Contains(asString(frames[0].Data["message"]), "segmentation fault") {
		t.Errorf("fault frame = %v", frames[0].Data)
	}

	// The faulted session can be inspected, then torn down and recreated.
	h.doLive(t, http.MethodPost, promptPath(id), `{"prompt":"again"}`).assertProblem(t, http.StatusConflict)
	h.timedInterrupt(t, id)
	h.doLive(t, http.MethodDelete, sessionPath(id), "").assertStatus(t, api.StatusClosed)
	resumed := h.createLive(t, `{"session_id":"`+id+`"}`)
	if resumed.SessionID != id || resumed.Status != "idle" {
		t.Errorf("recreated session = %+v", resumed)
	}
}

func TestDeleteRunningSessionEndsStream(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	id := h.createLive(t, "").SessionID
	s, _ := h.manager.Get(id)

	stream := h.openStream(t, id, `{"prompt":"go"}`, "")
	stream.expect(t, "session_state:running")
	mock := h.factory.Last().NextTurn(t)
	mock.MustEmit(t, session.AssistantChunk("partial"))
	stream.expect(t, "assistant_chunk")

	h.doLive(t, http.MethodDelete, sessionPath(id), "").assertStatus(t, api.StatusClosed)
	stream.expectEnd(t)
	mock.WaitCancelled(t)
	if s.State() != session.StateClosed || s.Events().Len() != 0 || h.factory.Last().CloseCount() != 1 {
		t.Errorf("state %q buffered %d closes %d", s.State(), s.Events().Len(), h.factory.Last().CloseCount())
	}
}

func TestLifecycleEndpointErrors(t *testing.T) {
	h := newHarness(t, harnessConfig{transport: []sessiontest.Option{
		sessiontest.WithCloseError(errors.New("wait: no child processes")),
	}})
	id := h.createSession(t, "").SessionID

	tests := []struct {
		name, method, path, body string
		want                     int
	}{
		{"interrupt unknown", http.MethodPost, interruptPath("sess_missing"), "", http.StatusNotFound},
		{"interrupt malformed id", http.MethodPost, interruptPath("bad.id"), "", http.StatusBadRequest},
		{"interrupt invalid body", http.MethodPost, interruptPath(id), `{"force":true}`, http.StatusBadRequest},
		{"interrupt non-JSON body", http.MethodPost, interruptPath(id), `nope`, http.StatusBadRequest},
		{"delete unknown", http.MethodDelete, sessionPath("sess_missing"), "", http.StatusNotFound},
		{"delete malformed id", http.MethodDelete, sessionPath("bad.id"), "", http.StatusBadRequest},
		{"delete with body", http.MethodDelete, sessionPath(id), `{"x":1}`, http.StatusBadRequest},
		{"delete transport failure", http.MethodDelete, sessionPath(id), "", http.StatusInternalServerError},
		{"delete after failure", http.MethodDelete, sessionPath(id), "", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertProblem(t, h.record(tc.method, tc.path, tc.body, nil), tc.want)
		})
	}
}
