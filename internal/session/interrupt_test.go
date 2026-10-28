package session_test

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/session"
	"github.com/captain-corgi/go-bot-example/internal/session/sessiontest"
)

// interruptBudget is the brief's interrupt latency requirement.
const interruptBudget = 500 * time.Millisecond

func withSignals(rec *sessiontest.SignalRecorder) func(*session.Options) {
	return func(o *session.Options) { o.SignalProcessGroup = rec.Signal }
}

func startGatedTurn(t *testing.T, s *session.Session, factory *sessiontest.MockFactory) (*session.Turn, *sessiontest.MockTurn) {
	t.Helper()
	turn, _, err := s.StartTurn("long task")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	return turn, factory.Last().NextTurn(t)
}

func timedInterrupt(t *testing.T, s *session.Session) time.Duration {
	t.Helper()
	start := time.Now()
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	return time.Since(start)
}

func TestInterruptHaltsActiveTurnAndKeepsSessionReusable(t *testing.T) {
	factory := sessiontest.NewMockFactory(sessiontest.WithPGID(4242))
	signals := &sessiontest.SignalRecorder{}
	s := newSession(t, factory, withSignals(signals))
	turn, mock := startGatedTurn(t, s, factory)
	mock.MustEmit(t, session.AssistantChunk("thinking"))

	if took := timedInterrupt(t, s); took >= interruptBudget {
		t.Fatalf("Interrupt took %v, want < %v", took, interruptBudget)
	}
	waitDone(t, turn)
	s.Detach(turn)

	assertSequence(t, s, "session_state:running", "assistant_chunk", "session_state:interrupted", "session_state:idle")
	if s.State() != session.StateIdle {
		t.Fatalf("state = %q, want idle", s.State())
	}
	if mock.Context().Err() == nil {
		t.Error("turn context was not cancelled")
	}
	transport := factory.Last()
	if transport.InterruptCount() != 1 || transport.CloseCount() != 0 {
		t.Errorf("interrupts = %d closes = %d, want 1 and 0", transport.InterruptCount(), transport.CloseCount())
	}
	if got := signals.Signals(); len(got) != 0 {
		t.Errorf("unexpected escalation %v for a turn that halted", got)
	}

	// The same subprocess serves the next turn.
	next, nextMock := startGatedTurn(t, s, factory)
	nextMock.MustEmit(t, session.Done(1, 1))
	waitDone(t, next)
	s.Detach(next)
	if len(factory.Transports()) != 1 || s.State() != session.StateIdle {
		t.Errorf("transports = %d state = %q; want the original subprocess, idle", len(factory.Transports()), s.State())
	}
}

func TestInterruptEscalatesSIGINTToProcessGroup(t *testing.T) {
	factory := sessiontest.NewMockFactory(sessiontest.WithPGID(4242), sessiontest.WithIgnoredInterrupts())
	signals := &sessiontest.SignalRecorder{}
	// SIGINT stops the lingering tool processes, which halts the turn.
	signals.OnSignal = func(sessiontest.GroupSignal) { factory.Last().FinishOpenTurns() }
	s := newSession(t, factory, withSignals(signals))
	turn, _ := startGatedTurn(t, s, factory)

	if took := timedInterrupt(t, s); took >= interruptBudget {
		t.Fatalf("Interrupt took %v, want < %v", took, interruptBudget)
	}
	waitDone(t, turn)
	s.Detach(turn)

	want := sessiontest.GroupSignal{PGID: 4242, Signal: syscall.SIGINT}
	if got := signals.Signals(); len(got) != 1 || got[0] != want {
		t.Fatalf("signals = %v, want [%v]", got, want)
	}
	if s.State() != session.StateIdle || factory.Last().CloseCount() != 0 {
		t.Errorf("state = %q closes = %d; want idle with the subprocess alive", s.State(), factory.Last().CloseCount())
	}
}

func TestInterruptEscalatesWhenSDKInterruptFails(t *testing.T) {
	factory := sessiontest.NewMockFactory(sessiontest.WithPGID(77), sessiontest.WithInterruptError(errors.New("pipe closed")))
	signals := &sessiontest.SignalRecorder{}
	s := newSession(t, factory, withSignals(signals), func(o *session.Options) {
		o.InterruptTimeout = 200 * time.Millisecond
		o.InterruptEscalation = 10 * time.Millisecond
	})
	turn, _ := startGatedTurn(t, s, factory)

	if took := timedInterrupt(t, s); took >= interruptBudget {
		t.Fatalf("Interrupt took %v, want < %v", took, interruptBudget)
	}
	waitDone(t, turn)
	s.Detach(turn)
	if got := signals.Signals(); len(got) != 1 || got[0].PGID != 77 {
		t.Fatalf("signals = %v, want one SIGINT to pgid 77", got)
	}
	if s.State() != session.StateIdle {
		t.Errorf("state = %q, want idle", s.State())
	}
}

func TestInterruptWithoutProcessGroupReturnsWithinTimeout(t *testing.T) {
	factory := sessiontest.NewMockFactory(sessiontest.WithIgnoredInterrupts())
	signals := &sessiontest.SignalRecorder{}
	s := newSession(t, factory, withSignals(signals), func(o *session.Options) {
		o.InterruptTimeout = 80 * time.Millisecond
		o.InterruptEscalation = time.Hour // capped at the timeout
	})
	turn, mock := startGatedTurn(t, s, factory)

	if took := timedInterrupt(t, s); took >= interruptBudget {
		t.Fatalf("Interrupt took %v, want < %v", took, interruptBudget)
	}
	waitDone(t, turn)
	s.Detach(turn)
	time.Sleep(50 * time.Millisecond)
	if got := signals.Signals(); len(got) != 0 {
		t.Errorf("signalled %v although the transport has no process group", got)
	}
	if s.State() != session.StateIdle {
		t.Errorf("state = %q, want idle", s.State())
	}
	mock.Finish()
}

func TestConcurrentInterruptsCoalesce(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	s := newSession(t, factory)
	turn, _ := startGatedTurn(t, s, factory)

	const callers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- s.Interrupt(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
	}
	waitDone(t, turn)
	s.Detach(turn)

	assertSequence(t, s, "session_state:running", "session_state:interrupted", "session_state:idle")
	if n := factory.Last().InterruptCount(); n != 1 {
		t.Errorf("transport interrupts = %d, want 1 (coalesced)", n)
	}
}

func TestInterruptIdleSessionIsNoOp(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	s := newSession(t, factory)
	for i := 0; i < 3; i++ {
		if err := s.Interrupt(context.Background()); err != nil {
			t.Fatalf("Interrupt #%d: %v", i, err)
		}
	}
	if s.State() != session.StateIdle || s.Events().LastSeq() != 0 || factory.Last().InterruptCount() != 0 {
		t.Errorf("state %q frames %d interrupts %d; want untouched idle session",
			s.State(), s.Events().LastSeq(), factory.Last().InterruptCount())
	}
}

func TestManagerInterruptErrors(t *testing.T) {
	m := newManager(t, sessiontest.NewMockFactory())
	if err := m.Interrupt(context.Background(), "sess_missing"); !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("unknown session err = %v, want ErrSessionNotFound", err)
	}
	if err := m.Interrupt(context.Background(), "bad id!"); !errors.Is(err, session.ErrInvalidSessionID) {
		t.Errorf("malformed id err = %v, want ErrInvalidSessionID", err)
	}
	s, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Interrupt(context.Background(), s.ID()); err != nil {
		t.Errorf("idle interrupt err = %v", err)
	}
	if err := m.CloseSession(context.Background(), s.ID()); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if err := s.Interrupt(context.Background()); !errors.Is(err, session.ErrSessionClosed) {
		t.Errorf("closed session Interrupt err = %v, want ErrSessionClosed", err)
	}
	if err := m.Interrupt(context.Background(), s.ID()); !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("closed session manager Interrupt err = %v, want ErrSessionNotFound", err)
	}
}
