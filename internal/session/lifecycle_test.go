package session_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/session"
	"github.com/captain-corgi/go-bot-example/internal/session/sessiontest"
)

func waitState(t *testing.T, s *session.Session, want session.SessionState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.State() != want {
		if time.Now().After(deadline) {
			t.Fatalf("state = %q, want %q", s.State(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSubprocessCrashMidTurnFaultsSession(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	s := newSession(t, factory)
	turn, mock := startGatedTurn(t, s, factory)
	mock.MustEmit(t, session.AssistantChunk("half"))

	factory.Last().Crash(errors.New("signal: killed"))
	waitDone(t, turn)
	s.Detach(turn)

	frames := assertSequence(t, s, "session_state:running", "assistant_chunk", "error:subprocess_exit", "session_state:error")
	if frames[2].Data["fatal"] != true || !strings.Contains(frames[2].Data["message"].(string), "signal: killed") {
		t.Errorf("fault frame = %v", frames[2].Data)
	}
	if s.State() != session.StateError {
		t.Fatalf("state = %q, want error", s.State())
	}
	if mock.Context().Err() == nil {
		t.Error("turn context not cancelled by the fault")
	}
	if _, _, err := s.StartTurn("again"); !errors.Is(err, session.ErrSessionFaulted) {
		t.Errorf("StartTurn after crash err = %v, want ErrSessionFaulted", err)
	}
}

func TestTurnStreamEndingBeforeExitIsAttributedToCrash(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	s := newSession(t, factory)
	turn, mock := startGatedTurn(t, s, factory)

	// A dying subprocess usually closes its pipes before it is reaped.
	mock.Finish()
	time.Sleep(10 * time.Millisecond)
	factory.Last().Crash(nil)
	waitDone(t, turn)
	s.Detach(turn)

	frames := assertSequence(t, s, "session_state:running", "error:subprocess_exit", "session_state:error")
	if frames[1].Data["message"] != "agent subprocess exited unexpectedly" {
		t.Errorf("message = %v", frames[1].Data["message"])
	}
}

func TestSubprocessExitWhileIdleFaultsSession(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	s := newSession(t, factory)
	factory.Last().Crash(errors.New("exit status 2"))
	waitState(t, s, session.StateError)
	assertSequence(t, s, "error:subprocess_exit", "session_state:error")
}

func TestCloseSessionInEveryState(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, s *session.Session, f *sessiontest.MockFactory) *session.Turn
	}{
		{"idle", func(*testing.T, *session.Session, *sessiontest.MockFactory) *session.Turn { return nil }},
		{"running", func(t *testing.T, s *session.Session, f *sessiontest.MockFactory) *session.Turn {
			turn, mock := startGatedTurn(t, s, f)
			mock.MustEmit(t, session.AssistantChunk("partial"))
			return turn
		}},
		{"interrupted", func(t *testing.T, s *session.Session, f *sessiontest.MockFactory) *session.Turn {
			turn, _ := startGatedTurn(t, s, f)
			if err := s.Interrupt(context.Background()); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			waitDone(t, turn)
			return nil
		}},
		{"error", func(t *testing.T, s *session.Session, f *sessiontest.MockFactory) *session.Turn {
			f.Last().Crash(errors.New("boom"))
			waitState(t, s, session.StateError)
			return nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			factory := sessiontest.NewMockFactory()
			m := newManager(t, factory, func(o *session.Options) { o.DisconnectGrace = time.Hour })
			s, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			turn := tc.setup(t, s, factory)
			if turn != nil {
				s.Detach(turn) // arms the disconnect grace timer
			}
			lifetime := factory.Last().StartContext()

			if err := m.CloseSession(context.Background(), s.ID()); err != nil {
				t.Fatalf("CloseSession: %v", err)
			}
			if turn != nil {
				waitDone(t, turn)
			}
			if _, ok := m.Get(s.ID()); ok {
				t.Error("session still registered")
			}
			if s.State() != session.StateClosed || s.Events().Len() != 0 {
				t.Errorf("state %q buffered %d; want closed and purged", s.State(), s.Events().Len())
			}
			if factory.Last().CloseCount() != 1 || lifetime.Err() == nil {
				t.Errorf("closes = %d lifetime err = %v; want subprocess terminated", factory.Last().CloseCount(), lifetime.Err())
			}
			// The intentional exit is not reported as a fault.
			time.Sleep(20 * time.Millisecond)
			if s.State() != session.StateClosed || s.Events().Len() != 0 {
				t.Errorf("after teardown: state %q buffered %d", s.State(), s.Events().Len())
			}
			if err := m.CloseSession(context.Background(), s.ID()); !errors.Is(err, session.ErrSessionNotFound) {
				t.Errorf("second CloseSession err = %v, want ErrSessionNotFound", err)
			}
			if _, _, err := s.StartTurn("late"); !errors.Is(err, session.ErrSessionClosed) {
				t.Errorf("StartTurn on closed session err = %v, want ErrSessionClosed", err)
			}
		})
	}
}

func TestCloseSessionErrors(t *testing.T) {
	m := newManager(t, sessiontest.NewMockFactory(sessiontest.WithCloseError(errors.New("wait: no child"))))
	if err := m.CloseSession(context.Background(), "nope!"); !errors.Is(err, session.ErrInvalidSessionID) {
		t.Errorf("malformed id err = %v", err)
	}
	if err := m.CloseSession(context.Background(), "sess_unknown"); !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("unknown id err = %v", err)
	}
	s, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.CloseSession(context.Background(), s.ID()); err == nil {
		t.Error("expected transport close error")
	}
	if _, ok := m.Get(s.ID()); ok {
		t.Error("session still registered after failed transport close")
	}
}

func TestClosedSessionCanBeResumedFromStore(t *testing.T) {
	factory := sessiontest.NewMockFactory(sessiontest.WithScript(sessiontest.FullTurn))
	m := newManager(t, factory)
	s, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	turn, _, err := s.StartTurn("hi")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	waitDone(t, turn)
	s.Detach(turn)
	if err := m.CloseSession(context.Background(), s.ID()); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	resumed, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{SessionID: s.ID()})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed == s || len(resumed.History()) != 2 || len(factory.Transports()) != 2 {
		t.Errorf("resumed %p (old %p) history %v transports %d", resumed, s, resumed.History(), len(factory.Transports()))
	}
}

func TestShutdownClosesEverySession(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	m := newManager(t, factory)
	ids := make([]string, 3)
	for i := range ids {
		s, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids[i] = s.ID()
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for _, id := range ids {
		if _, ok := m.Get(id); ok {
			t.Errorf("session %s still registered", id)
		}
	}
	for _, tr := range factory.Transports() {
		if tr.CloseCount() != 1 {
			t.Errorf("transport closed %d times, want 1", tr.CloseCount())
		}
	}

	failing := newManager(t, sessiontest.NewMockFactory(sessiontest.WithCloseError(errors.New("stuck"))))
	if _, err := failing.CreateOrResume(context.Background(), session.CreateSessionRequest{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := failing.Shutdown(context.Background()); err == nil {
		t.Error("expected Shutdown to report the transport close error")
	}
}
