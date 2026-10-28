package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/api"
	"github.com/captain-corgi/go-bot-example/internal/session"
	"github.com/captain-corgi/go-bot-example/internal/session/sessiontest"
	"github.com/captain-corgi/go-bot-example/internal/sse"
)

// createLive calls POST /sessions over the live loopback test server.
func (h *harness) createLive(t *testing.T, body string) createResponse {
	t.Helper()
	srv := h.server(t)
	resp, err := srv.Client().Post(srv.URL+"/sessions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /sessions status = %d, body %s", resp.StatusCode, b)
	}
	var out createResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return out
}

func assertIDs(t *testing.T, frames []sseFrame, first uint64) {
	t.Helper()
	for i, f := range frames {
		if want := strconv.FormatUint(first+uint64(i), 10); f.ID != want {
			t.Fatalf("frame %d (%s) id = %q, want %q", i, f.label(), f.ID, want)
		}
	}
}

func TestScenarioACreateAndStreamLiveTurn(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	created := h.createLive(t, `{"prompt":"you are helpful","config":{"model":"grok"}}`)
	if created.SessionID == "" || created.Status != "idle" {
		t.Fatalf("create response = %+v", created)
	}

	stream := h.openStream(t, created.SessionID, `{"prompt":"list files"}`, "")
	for header, want := range map[string]string{
		"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "Connection": "keep-alive",
	} {
		if got := stream.resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	frames := stream.expect(t, "session_state:running")
	mock := h.factory.Last().NextTurn(t)
	if mock.Prompt != "list files" {
		t.Errorf("transport prompt = %q", mock.Prompt)
	}

	// Each event is flushed live, before the turn completes.
	mock.MustEmit(t, session.AssistantChunk("Let me look."))
	chunk := stream.expect(t, "assistant_chunk")[0]
	if chunk.Data["delta"] != "Let me look." {
		t.Errorf("delta = %v", chunk.Data["delta"])
	}
	mock.MustEmit(t, session.ToolCall("call_7", "list_files", map[string]any{"path": "/tmp"}))
	call := stream.expect(t, "tool_call")[0]
	if call.Data["tool_call_id"] != "call_7" || call.Data["name"] != "list_files" ||
		call.Data["arguments"].(map[string]any)["path"] != "/tmp" {
		t.Errorf("tool_call = %v", call.Data)
	}
	mock.MustEmit(t, session.ToolResult("call_7", sse.ToolStatusError, "", "permission denied"))
	result := stream.expect(t, "tool_result")[0]
	if result.Data["status"] != "error" || result.Data["stderr"] != "permission denied" || result.Data["stdout"] != "" {
		t.Errorf("tool_result = %v", result.Data)
	}
	mock.MustEmit(t, session.Done(30, 12))
	tail := stream.expect(t, "done", "session_state:idle")
	usage := tail[0].Data["usage"].(map[string]any)
	if usage["input_tokens"] != 30.0 || usage["output_tokens"] != 12.0 || usage["total_tokens"] != 42.0 {
		t.Errorf("usage = %v", usage)
	}
	stream.expectEnd(t)

	all := append(append(append(append(frames, chunk), call), result), tail...)
	assertIDs(t, all, 1)
	s, _ := h.manager.Get(created.SessionID)
	if s.State() != session.StateIdle {
		t.Errorf("state = %q, want idle", s.State())
	}
}

func TestScenarioBResumePersistedSession(t *testing.T) {
	store := session.NewMemoryStore()
	scripted := []sessiontest.Option{sessiontest.WithScript(sessiontest.FullTurn)}
	first := newHarness(t, harnessConfig{store: store, transport: scripted})
	id := first.createLive(t, `{"prompt":"init"}`).SessionID

	stream := first.openStream(t, id, `{"prompt":"one"}`, "")
	stream.expect(t, "session_state:running", "assistant_chunk", "assistant_chunk", "tool_call", "tool_result",
		"done", "session_state:idle")
	stream.expectEnd(t)

	// Resuming while the session is active reattaches to it.
	if again := first.createLive(t, `{"session_id":"`+id+`"}`); again.SessionID != id || again.Status != "idle" {
		t.Fatalf("resume active = %+v", again)
	}
	if n := len(first.factory.Transports()); n != 1 {
		t.Errorf("transports = %d, want 1", n)
	}

	// After a daemon restart the session is restored from persisted state.
	second := newHarness(t, harnessConfig{store: store, transport: scripted})
	if restored := second.createLive(t, `{"session_id":"`+id+`"}`); restored.SessionID != id || restored.Status != "idle" {
		t.Fatalf("resume persisted = %+v", restored)
	}
	cfg, _ := second.factory.Last().StartConfig()
	if !cfg.Resume || cfg.SessionID != id || cfg.InitialPrompt != "init" || len(cfg.History) != 2 ||
		cfg.History[0] != (session.Message{Role: session.RoleUser, Content: "one"}) ||
		cfg.History[1] != (session.Message{Role: session.RoleAssistant, Content: "Hello, one"}) {
		t.Fatalf("restored transport config = %+v", cfg)
	}

	stream = second.openStream(t, id, `{"prompt":"two"}`, "")
	stream.expect(t, "session_state:running", "assistant_chunk", "assistant_chunk", "tool_call", "tool_result",
		"done", "session_state:idle")
	stream.expectEnd(t)

	s, _ := second.manager.Get(id)
	if h := s.History(); len(h) != 4 || h[2].Content != "two" || h[3].Content != "Hello, two" {
		t.Errorf("history after resumed turn = %v", h)
	}
	if got := second.factory.Last().Prompts(); len(got) != 1 || got[0] != "two" {
		t.Errorf("resumed transport prompts = %v", got)
	}
}

func TestConcurrentPromptRejectedWithConflict(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	id := h.createLive(t, "").SessionID

	stream := h.openStream(t, id, `{"prompt":"first"}`, "")
	frames := stream.expect(t, "session_state:running")
	mock := h.factory.Last().NextTurn(t)
	mock.MustEmit(t, session.AssistantChunk("a"))
	frames = append(frames, stream.expect(t, "assistant_chunk")...)

	resp, cancel := h.postPrompt(t, id, `{"prompt":"second"}`, "")
	body, _ := io.ReadAll(resp.Body)
	cancel()
	got := decodeProblem(t, resp.StatusCode, resp.Header.Get("Content-Type"), body, http.StatusConflict)
	if got.Type != api.TypeSessionBusy || got.Title != api.TitleSessionBusy ||
		!strings.Contains(got.Detail, id) || got.Instance != promptPath(id) {
		t.Errorf("409 problem = %+v", got)
	}

	// The active stream is undisturbed and completes normally.
	mock.MustEmit(t, session.AssistantChunk("b"), session.Done(1, 2))
	frames = append(frames, stream.expect(t, "assistant_chunk", "done", "session_state:idle")...)
	stream.expectEnd(t)
	assertIDs(t, frames, 1)
	if frames[2].Data["delta"] != "b" {
		t.Errorf("second chunk = %v", frames[2].Data)
	}
	if prompts := h.factory.Last().Prompts(); len(prompts) != 1 || prompts[0] != "first" {
		t.Errorf("transport prompts = %v, want only the first", prompts)
	}
}

func TestDisconnectReconnectReplaysMissedEvents(t *testing.T) {
	h := newHarness(t, harnessConfig{})
	id := h.createLive(t, "").SessionID

	stream := h.openStream(t, id, `{"prompt":"go"}`, "")
	stream.expect(t, "session_state:running")
	mock := h.factory.Last().NextTurn(t)
	mock.MustEmit(t, session.AssistantChunk("seen"))
	seen := stream.expect(t, "assistant_chunk")[0]
	stream.close()

	// Out-of-band events keep buffering while no client is attached.
	mock.MustEmit(t,
		session.AssistantChunk("missed"),
		session.ToolCall("c1", "grep", map[string]any{"q": "x"}),
		session.ToolResult("c1", sse.ToolStatusSuccess, "hit", ""),
	)

	resumed := h.openStream(t, id, "", seen.ID)
	replayed := resumed.expect(t, "assistant_chunk", "tool_call", "tool_result")
	assertIDs(t, replayed, 3)
	if replayed[0].Data["delta"] != "missed" {
		t.Errorf("replayed chunk = %v", replayed[0].Data)
	}
	if mock.Context().Err() != nil {
		t.Fatal("turn cancelled although the client reconnected within the grace window")
	}

	// Live streaming resumes on the reconnected stream until done.
	mock.MustEmit(t, session.AssistantChunk("live"), session.Done(5, 5))
	live := resumed.expect(t, "assistant_chunk", "done", "session_state:idle")
	assertIDs(t, live, 6)
	resumed.expectEnd(t)
	if prompts := h.factory.Last().Prompts(); len(prompts) != 1 {
		t.Errorf("reconnect submitted a new turn: prompts %v", prompts)
	}
}

func TestDisconnectGraceExpiryCancelsTurn(t *testing.T) {
	h := newHarness(t, harnessConfig{grace: 50 * time.Millisecond})
	id := h.createLive(t, "").SessionID
	s, _ := h.manager.Get(id)

	stream := h.openStream(t, id, `{"prompt":"go"}`, "")
	stream.expect(t, "session_state:running")
	mock := h.factory.Last().NextTurn(t)
	mock.MustEmit(t, session.AssistantChunk("partial"))
	stream.expect(t, "assistant_chunk")
	stream.close()

	mock.WaitCancelled(t)
	waitFor(t, "session to leave running", func() bool { return s.State() == session.StateIdle })

	// A late reconnect still sees how the turn ended.
	late := h.openStream(t, id, "", "2")
	assertIDs(t, late.expect(t, "session_state:interrupted", "session_state:idle"), 3)
	late.expectEnd(t)

	// The session remains usable for the next turn.
	next := h.openStream(t, id, `{"prompt":"again"}`, "")
	next.expect(t, "session_state:running")
	h.factory.Last().NextTurn(t).MustEmit(t, session.Done(1, 1))
	next.expect(t, "done", "session_state:idle")
	next.expectEnd(t)
}
