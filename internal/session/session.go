package session

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/sse"
)

// Turn is a single in-flight prompt turn on a session.
type Turn struct {
	done        chan struct{}
	cancel      context.CancelFunc
	subscribers int    // guarded by the owning Session's mu
	endSeq      uint64 // written before done is closed
}

// Done is closed once the turn has completed and its final frames have been
// appended to the session's event buffer.
func (t *Turn) Done() <-chan struct{} {
	return t.done
}

// EndSeq returns the sequence ID of the turn's last frame. It is only
// meaningful after Done is closed.
func (t *Turn) EndSeq() uint64 {
	return t.endSeq
}

// Session is an isolated agent session bound 1:1 to a dedicated Transport.
type Session struct {
	id        string
	transport Transport
	events    *sse.EventRingBuffer
	store     Store
	graceTTL  time.Duration
	now       func() time.Time

	// lifetime is the session-scoped context; turns derive from it so they
	// are decoupled from any single HTTP request.
	lifetime       context.Context
	cancelLifetime context.CancelFunc

	mu     sync.Mutex
	state  SessionState
	turn   *Turn
	record Record
}

// ID returns the session identifier.
func (s *Session) ID() string {
	return s.id
}

// State returns the current lifecycle state.
func (s *Session) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// History returns a copy of the persisted conversation history.
func (s *Session) History() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.record.History)
}

// Events returns the session's SSE replay buffer.
func (s *Session) Events() *sse.EventRingBuffer {
	return s.events
}

// TryBeginTurn atomically transitions an idle (or interrupted) session to
// running and returns a turn context derived from ctx. Concurrent callers
// that lose the race, or callers on a running or faulted session, receive
// ok=false.
func (s *Session) TryBeginTurn(ctx context.Context) (turnCtx context.Context, cancel context.CancelFunc, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateIdle && s.state != StateInterrupted {
		return nil, nil, false
	}
	turnCtx, cancel = context.WithCancel(ctx)
	s.turn = &Turn{done: make(chan struct{}), cancel: cancel}
	s.state = StateRunning
	return turnCtx, cancel, true
}

// CompleteTurn ends the active turn, if any, and transitions the session to
// nextState.
func (s *Session) CompleteTurn(nextState SessionState) {
	s.finishTurn(nextState)
}

// finishTurn appends the final frames, transitions to nextState, and releases
// the active turn in one critical section so a client that observes the final
// frame can immediately start the next turn.
func (s *Session) finishTurn(nextState SessionState, final ...SDKEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range final {
		// Final frames are built internally from well-formed payloads.
		_, _ = s.events.Append(ev.Type, ev.Payload)
	}
	s.state = nextState
	if t := s.turn; t != nil {
		s.turn = nil
		t.endSeq = s.events.LastSeq()
		t.cancel()
		close(t.done)
	}
	s.events.CancelDisconnectGrace()
}

// Attach registers an SSE subscriber on the active turn and cancels any
// pending disconnect grace timer. It returns nil when no turn is active.
func (s *Session) Attach() *Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.turn
	if t != nil {
		t.subscribers++
		s.events.CancelDisconnectGrace()
	}
	return t
}

// Detach unregisters an SSE subscriber from t. When the last subscriber of a
// still-active turn detaches, the disconnect grace timer is armed and the
// turn is cancelled if nobody re-attaches before it expires.
func (s *Session) Detach(t *Turn) {
	if t == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.subscribers--
	if t.subscribers <= 0 && s.turn == t {
		s.events.StartDisconnectGrace(s.graceTTL, t.cancel)
	}
}

// recordExchange appends a completed exchange to the session history and
// persists it.
func (s *Session) recordExchange(prompt, reply string) error {
	s.mu.Lock()
	history := append(slices.Clone(s.record.History), Message{Role: RoleUser, Content: prompt})
	if reply != "" {
		history = append(history, Message{Role: RoleAssistant, Content: reply})
	}
	rec := s.record.clone()
	rec.History = history
	rec.UpdatedAt = s.now()
	s.record = rec
	s.mu.Unlock()
	return s.store.Save(rec)
}

func stateEvent(state SessionState) SDKEvent {
	return SDKEvent{Type: sse.EventSessionState, Payload: sse.SessionStatePayload{State: string(state)}}
}
