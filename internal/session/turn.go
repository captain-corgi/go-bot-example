package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/sse"
)

var (
	// ErrTurnInProgress is returned when a prompt is submitted while a turn
	// is already running on the session.
	ErrTurnInProgress = errors.New("session: turn already running")
	// ErrSessionFaulted is returned when a prompt is submitted to a session
	// in the error state.
	ErrSessionFaulted = errors.New("session: session is in error state")
	// ErrSessionClosed is returned for operations on a torn-down session.
	ErrSessionClosed = errors.New("session: session is closed")
)

// exitProbeWindow is how long a turn whose event stream ended without
// completing waits for the fault monitor to attribute the end to a
// subprocess exit before reporting it as an incomplete turn.
const exitProbeWindow = 100 * time.Millisecond

// Stream-level error codes emitted by the session itself.
const (
	CodeInvalidEvent   = "invalid_event"
	CodeTurnIncomplete = "turn_incomplete"
	CodePersistFailed  = "persist_failed"
	CodeSubprocessExit = "subprocess_exit"
)

type turnOutcome int

const (
	outcomeDone turnOutcome = iota
	outcomeFatal
	outcomeCancelled
	outcomeIncomplete
)

// StartTurn acquires the turn lock, submits prompt to the transport, appends
// the session_state "running" frame, and runs the turn in the background on a
// context derived from the session lifetime (not from any HTTP request).
//
// The returned Turn is pre-attached for the caller, who must call Detach when
// it stops streaming. startSeq is the sequence ID immediately preceding the
// turn's first frame, suitable for EventRingBuffer.ReplayAfter.
func (s *Session) StartTurn(prompt string) (turn *Turn, startSeq uint64, err error) {
	turnCtx, _, ok := s.TryBeginTurn(s.lifetime)
	if !ok {
		switch s.State() {
		case StateError:
			return nil, 0, ErrSessionFaulted
		case StateClosed:
			return nil, 0, ErrSessionClosed
		}
		return nil, 0, ErrTurnInProgress
	}
	turn = s.Attach()

	events, err := s.transport.SendPrompt(turnCtx, prompt)
	if err != nil {
		s.CompleteTurn(StateIdle)
		return nil, 0, fmt.Errorf("session %s: send prompt: %w", s.id, err)
	}

	running := stateEvent(StateRunning)
	frame, err := s.events.Append(running.Type, running.Payload)
	if err != nil {
		s.CompleteTurn(StateIdle)
		return nil, 0, fmt.Errorf("session %s: append running frame: %w", s.id, err)
	}

	go s.runTurn(turnCtx, turn, events, prompt)
	return turn, frame.ID - 1, nil
}

// runTurn pumps transport events into the replay buffer until the turn ends,
// persists the exchange, and appends the terminal frames. A turn already
// ended by a subprocess fault or session teardown is left as is.
func (s *Session) runTurn(ctx context.Context, t *Turn, events <-chan SDKEvent, prompt string) {
	var reply strings.Builder
	outcome, terminal := s.consume(ctx, events, &reply)
	go s.awaitHalt(t, events)

	var final []SDKEvent
	if err := s.recordExchange(prompt, reply.String()); err != nil {
		final = append(final, StreamError(CodePersistFailed, "failed to persist session state", false))
	}

	switch outcome {
	case outcomeDone:
		s.finishTurn(t, StateIdle, append(final, terminal, stateEvent(StateIdle))...)
	case outcomeFatal:
		s.finishTurn(t, StateError, append(final, terminal, stateEvent(StateError))...)
	case outcomeCancelled:
		s.finishTurn(t, StateIdle, append(final, stateEvent(StateInterrupted), stateEvent(StateIdle))...)
	default:
		if s.waitExited(exitProbeWindow) {
			// The subprocess died; the fault monitor reports it and ends the turn.
			return
		}
		incomplete := StreamError(CodeTurnIncomplete, "agent transport ended the turn without completing it", true)
		s.finishTurn(t, StateError, append(final, incomplete, stateEvent(StateError))...)
	}
}

// awaitHalt drains events the session no longer consumes and marks t halted
// once the transport closes the channel, or when the session is torn down.
func (s *Session) awaitHalt(t *Turn, events <-chan SDKEvent) {
	defer t.markHalted()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-s.lifetime.Done():
			return
		}
	}
}

// waitExited reports whether the subprocess exits within d.
func (s *Session) waitExited(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-s.exited:
		return true
	case <-timer.C:
		return false
	}
}

// consume appends non-terminal events to the buffer and returns how the turn
// ended. Terminal events (done, fatal error) are returned unappended so they
// can be written atomically with the session state transition.
func (s *Session) consume(ctx context.Context, events <-chan SDKEvent, reply *strings.Builder) (turnOutcome, SDKEvent) {
	for {
		select {
		case <-ctx.Done():
			return outcomeCancelled, SDKEvent{}
		case ev, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return outcomeCancelled, SDKEvent{}
				}
				return outcomeIncomplete, SDKEvent{}
			}
			if !validEvent(ev) {
				s.appendInvalid(ev.Type)
				continue
			}
			switch p := ev.Payload.(type) {
			case sse.DonePayload:
				return outcomeDone, ev
			case sse.ErrorPayload:
				if p.Fatal {
					return outcomeFatal, ev
				}
			case sse.AssistantChunkPayload:
				reply.WriteString(p.Delta)
			}
			if _, err := s.events.Append(ev.Type, ev.Payload); err != nil {
				s.appendInvalid(ev.Type)
			}
		}
	}
}

func (s *Session) appendInvalid(eventType sse.EventType) {
	invalid := StreamError(CodeInvalidEvent, fmt.Sprintf("agent emitted an invalid %q event", eventType), false)
	_, _ = s.events.Append(invalid.Type, invalid.Payload)
}
