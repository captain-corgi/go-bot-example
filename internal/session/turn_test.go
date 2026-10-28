package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/session"
	"github.com/captain-corgi/go-bot-example/internal/session/sessiontest"
	"github.com/captain-corgi/go-bot-example/internal/sse"
)

type decodedFrame struct {
	ID    uint64
	Event sse.EventType
	Data  map[string]any
}

func bufferedFrames(t *testing.T, s *session.Session) []decodedFrame {
	t.Helper()
	frames, _ := s.Events().ReplayAfter(0)
	out := make([]decodedFrame, 0, len(frames))
	for _, f := range frames {
		var data map[string]any
		if err := json.Unmarshal(f.Data, &data); err != nil {
			t.Fatalf("frame %d data %s: %v", f.ID, f.Data, err)
		}
		out = append(out, decodedFrame{ID: f.ID, Event: f.Event, Data: data})
	}
	return out
}

// sequence renders frames as "event[:state|code]" for compact assertions.
func sequence(frames []decodedFrame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		label := string(f.Event)
		switch f.Event {
		case sse.EventSessionState:
			label += ":" + f.Data["state"].(string)
		case sse.EventError:
			label += ":" + f.Data["code"].(string)
		}
		out[i] = label
	}
	return out
}

func assertSequence(t *testing.T, s *session.Session, want ...string) []decodedFrame {
	t.Helper()
	frames := bufferedFrames(t, s)
	got := sequence(frames)
	if len(got) != len(want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("frames = %v, want %v", got, want)
		}
		if frames[i].ID != uint64(i+1) {
			t.Fatalf("frame %d has id %d, want %d", i, frames[i].ID, i+1)
		}
	}
	return frames
}

func newSession(t *testing.T, factory *sessiontest.MockFactory, mutate ...func(*session.Options)) *session.Session {
	t.Helper()
	s, err := newManager(t, factory, mutate...).CreateOrResume(context.Background(), session.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return s
}

func TestStartTurnStreamsFullTurn(t *testing.T) {
	s := newSession(t, sessiontest.NewMockFactory(sessiontest.WithScript(sessiontest.FullTurn)))
	turn, startSeq, err := s.StartTurn("there")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if startSeq != 0 {
		t.Errorf("startSeq = %d, want 0", startSeq)
	}
	waitDone(t, turn)
	s.Detach(turn)

	frames := assertSequence(t, s, "session_state:running", "assistant_chunk", "assistant_chunk",
		"tool_call", "tool_result", "done", "session_state:idle")
	usage := frames[5].Data["usage"].(map[string]any)
	if usage["input_tokens"] != 12.0 || usage["output_tokens"] != 7.0 || usage["total_tokens"] != 19.0 {
		t.Errorf("usage = %v", usage)
	}
	if turn.EndSeq() != 7 {
		t.Errorf("EndSeq = %d, want 7", turn.EndSeq())
	}
	if s.State() != session.StateIdle {
		t.Errorf("state = %q, want idle", s.State())
	}
	if h := s.History(); len(h) != 2 || h[1].Content != "Hello, there" {
		t.Errorf("history = %v", h)
	}
}

func TestStartTurnRejectsConcurrentTurn(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	s := newSession(t, factory)
	turn, _, err := s.StartTurn("one")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	mock := factory.Last().NextTurn(t)

	if _, _, err := s.StartTurn("two"); !errors.Is(err, session.ErrTurnInProgress) {
		t.Fatalf("second StartTurn err = %v, want ErrTurnInProgress", err)
	}
	mock.MustEmit(t, session.Done(1, 1))
	waitDone(t, turn)
	s.Detach(turn)
	if got := factory.Last().Prompts(); len(got) != 1 || got[0] != "one" {
		t.Errorf("prompts = %v, want [one]", got)
	}
}

func TestFatalErrorFaultsSession(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	s := newSession(t, factory)
	turn, _, err := s.StartTurn("go")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	mock := factory.Last().NextTurn(t)
	mock.MustEmit(t, session.StreamError("rate_limited", "slow down", false), session.StreamError("crashed", "boom", true))
	waitDone(t, turn)
	s.Detach(turn)

	frames := assertSequence(t, s, "session_state:running", "error:rate_limited", "error:crashed", "session_state:error")
	if frames[1].Data["fatal"] != false || frames[2].Data["fatal"] != true {
		t.Errorf("fatal flags = %v, %v", frames[1].Data["fatal"], frames[2].Data["fatal"])
	}
	if s.State() != session.StateError {
		t.Errorf("state = %q, want error", s.State())
	}
	if _, _, err := s.StartTurn("again"); !errors.Is(err, session.ErrSessionFaulted) {
		t.Errorf("StartTurn on faulted session err = %v, want ErrSessionFaulted", err)
	}
}

func TestTurnEndingWithoutDoneIsIncomplete(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	s := newSession(t, factory)
	turn, _, err := s.StartTurn("go")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	mock := factory.Last().NextTurn(t)
	mock.MustEmit(t, session.SDKEvent{Type: sse.EventSessionState, Payload: sse.SessionStatePayload{State: "idle"}},
		session.SDKEvent{Type: sse.EventAssistantChunk, Payload: "not a payload struct"},
		session.ToolCall("c1", "noop", nil))
	mock.Finish()
	waitDone(t, turn)
	s.Detach(turn)

	frames := assertSequence(t, s, "session_state:running", "error:invalid_event", "error:invalid_event",
		"tool_call", "error:turn_incomplete", "session_state:error")
	if args, ok := frames[3].Data["arguments"].(map[string]any); !ok || len(args) != 0 {
		t.Errorf("nil tool arguments must encode as {}, got %v", frames[3].Data["arguments"])
	}
}

func TestSendPromptFailureLeavesSessionIdle(t *testing.T) {
	s := newSession(t, sessiontest.NewMockFactory(sessiontest.WithSendError(errors.New("pipe closed"))))
	if _, _, err := s.StartTurn("go"); err == nil {
		t.Fatal("expected send error")
	}
	if s.State() != session.StateIdle || s.Events().LastSeq() != 0 {
		t.Errorf("state %q lastSeq %d, want idle with no frames", s.State(), s.Events().LastSeq())
	}
}

func TestDisconnectGraceCancelsTurn(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	s := newSession(t, factory, func(o *session.Options) { o.DisconnectGrace = 20 * time.Millisecond })
	turn, _, err := s.StartTurn("go")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	mock := factory.Last().NextTurn(t)
	mock.MustEmit(t, session.AssistantChunk("partial"))

	s.Detach(turn)
	mock.WaitCancelled(t)
	waitDone(t, turn)

	assertSequence(t, s, "session_state:running", "assistant_chunk", "session_state:interrupted", "session_state:idle")
	if s.State() != session.StateIdle {
		t.Errorf("state = %q, want idle", s.State())
	}
	if h := s.History(); len(h) != 2 || h[1].Content != "partial" {
		t.Errorf("history = %v", h)
	}
}

func TestReattachCancelsDisconnectGrace(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	s := newSession(t, factory, func(o *session.Options) { o.DisconnectGrace = 30 * time.Millisecond })
	turn, _, err := s.StartTurn("go")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	mock := factory.Last().NextTurn(t)

	s.Detach(turn)
	if again := s.Attach(); again != turn {
		t.Fatal("Attach did not return the active turn")
	}
	time.Sleep(90 * time.Millisecond)
	if mock.Context().Err() != nil {
		t.Fatal("turn cancelled despite re-attach within grace window")
	}
	mock.MustEmit(t, session.Done(0, 0))
	waitDone(t, turn)
	s.Detach(turn)
	if s.Attach() != nil {
		t.Error("Attach after completion returned a turn")
	}
}

// failingStore accepts the first allowed saves and fails every later one.
type failingStore struct {
	*session.MemoryStore
	allowed atomic.Int32
}

func (f *failingStore) Save(rec session.Record) error {
	if f.allowed.Add(-1) < 0 {
		return errors.New("disk full")
	}
	return f.MemoryStore.Save(rec)
}

func TestCreateFailsWhenStoreCannotPersist(t *testing.T) {
	store := &failingStore{MemoryStore: session.NewMemoryStore()}
	factory := sessiontest.NewMockFactory()
	m := newManager(t, factory, func(o *session.Options) { o.Store = store })
	if _, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{}); err == nil {
		t.Fatal("expected create to fail when the store cannot persist")
	}
	if factory.Last().CloseCount() != 1 {
		t.Error("transport of an unpersisted session was not closed")
	}
}

func TestPersistFailureEmitsNonFatalError(t *testing.T) {
	store := &failingStore{MemoryStore: session.NewMemoryStore()}
	store.allowed.Store(1)
	s := newSession(t, sessiontest.NewMockFactory(sessiontest.WithScript(sessiontest.FullTurn)),
		func(o *session.Options) { o.Store = store })
	turn, _, err := s.StartTurn("go")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	waitDone(t, turn)
	s.Detach(turn)

	assertSequence(t, s, "session_state:running", "assistant_chunk", "assistant_chunk", "tool_call",
		"tool_result", "error:persist_failed", "done", "session_state:idle")
	if s.State() != session.StateIdle {
		t.Errorf("state = %q, want idle", s.State())
	}
}
