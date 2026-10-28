package session

import (
	"fmt"
)

// monitor watches the transport for an unexpected subprocess exit until the
// session is torn down.
func (s *Session) monitor(done <-chan error) {
	var err error
	select {
	case <-s.lifetime.Done():
		return
	case err = <-done:
	}
	close(s.exited)
	s.fault(err)
}

// fault records an unexpected subprocess exit: it ends any active turn with a
// fatal subprocess_exit error frame followed by session_state "error", which
// closes every stream following the turn, and leaves the session in the error
// state so the caller can inspect or recreate it. Intentional teardown is not
// a fault.
func (s *Session) fault(exitErr error) {
	msg := "agent subprocess exited unexpectedly"
	if exitErr != nil {
		msg = fmt.Sprintf("%s: %v", msg, exitErr)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == StateClosed {
		return
	}
	s.appendLocked(StreamError(CodeSubprocessExit, msg, true), stateEvent(StateError))
	if t := s.turn; t != nil {
		s.releaseTurnLocked(t, StateError)
		return
	}
	s.state = StateError
	s.events.CancelDisconnectGrace()
}

// close tears the session down: it ends any active turn and its streams,
// cancels the disconnect grace timer and the session lifetime, terminates the
// transport subprocess, and purges the replay buffer. It returns
// ErrSessionClosed if the session was already closed.
func (s *Session) close() error {
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	if t := s.turn; t != nil {
		s.releaseTurnLocked(t, StateClosed)
	}
	s.state = StateClosed
	s.events.CancelDisconnectGrace()
	s.mu.Unlock()

	// Cancelling the lifetime first stops the fault monitor from reporting
	// the intentional exit below.
	s.cancelLifetime()
	err := s.transport.Close()
	s.events.Purge()
	if err != nil {
		return fmt.Errorf("session %s: close transport: %w", s.id, err)
	}
	return nil
}
