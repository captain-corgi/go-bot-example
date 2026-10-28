package session

import (
	"context"
	"syscall"
	"time"
)

// Interrupt timing defaults.
const (
	// DefaultInterruptTimeout bounds how long Interrupt blocks before
	// returning, so interrupt requests answer within 500ms.
	DefaultInterruptTimeout = 500 * time.Millisecond
	// DefaultInterruptEscalation is how long an interrupted turn may stay
	// unhalted after context cancellation and the SDK interrupt before SIGINT
	// is sent to the agent's process group.
	DefaultInterruptEscalation = 250 * time.Millisecond
)

// GroupSignaler delivers sig to every process in the process group pgid.
type GroupSignaler func(pgid int, sig syscall.Signal) error

type interruptConfig struct {
	timeout    time.Duration
	escalation time.Duration
	signal     GroupSignaler
}

// Interrupt halts the session's in-flight turn, if any, while keeping the
// agent subprocess alive. It cancels the turn context, sends the SDK
// interrupt, and escalates SIGINT to the agent's process group when the
// transport has not halted the turn within the escalation window. The turn
// ends with session_state "interrupted" then "idle" and the turn lock is
// released.
//
// Interrupt is idempotent: concurrent calls coalesce onto a single
// cancellation, and calls on a session without an active turn return nil
// immediately. It returns within the configured timeout (500ms by default)
// even if the transport never acknowledges the halt.
func (s *Session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	state, t := s.state, s.turn
	s.mu.Unlock()
	if state == StateClosed {
		return ErrSessionClosed
	}
	if t == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.interrupt.timeout)
	defer cancel()
	t.interrupt.Do(func() { go s.haltTurn(t) })

	select {
	case <-t.done:
	case <-ctx.Done():
		return nil
	}
	select {
	case <-t.halted:
	case <-ctx.Done():
	}
	return nil
}

// haltTurn performs the single cancellation and escalation for t.
func (s *Session) haltTurn(t *Turn) {
	t.cancel()

	escalate := time.NewTimer(s.interrupt.escalation)
	defer escalate.Stop()
	ctx, cancel := context.WithTimeout(s.lifetime, s.interrupt.timeout)
	defer cancel()
	// A failed SDK interrupt is covered by the process group escalation below.
	_ = s.transport.Interrupt(ctx)

	select {
	case <-t.halted:
		return
	case <-s.lifetime.Done():
		return
	case <-escalate.C:
	}
	if pgid := s.transport.PGID(); pgid > 1 {
		// Best effort: the session is already idle; SIGINT only stops
		// lingering tool processes in the agent's process group.
		_ = s.interrupt.signal(pgid, syscall.SIGINT)
	}
}
