package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/captain-corgi/go-bot-example/internal/problem"
	"github.com/captain-corgi/go-bot-example/internal/session"
	"github.com/captain-corgi/go-bot-example/internal/sse"
)

// Problem type and title for prompts submitted while a turn is running.
const (
	TypeSessionBusy  = "https://grokbot.local/problems/session-busy"
	TitleSessionBusy = "Session Turn Already Running"
)

// CodeReplayGap is the non-fatal error code sent when frames after the
// client's Last-Event-ID were evicted from the replay buffer.
const CodeReplayGap = "replay_gap"

type promptRequest struct {
	Prompt string `json:"prompt"`
}

func (h *Handler) submitPrompt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s, ok := h.manager.Get(id)
	if !ok {
		problem.NotFound(w, r, "Session "+id+" does not exist.")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		problem.InternalServerError(w, r, "Streaming is not supported by this connection.")
		return
	}
	lastID, hasLastID, err := parseLastEventID(r.Header.Get("Last-Event-ID"))
	if err != nil {
		problem.BadRequest(w, r, "Last-Event-ID must be a non-negative integer event ID.")
		return
	}
	var req promptRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		problem.BadRequest(w, r, `Request body must be a JSON object {"prompt": string}: `+err.Error())
		return
	}

	if hasLastID {
		if lastID > s.Events().LastSeq() {
			problem.BadRequest(w, r, "Last-Event-ID is ahead of the session's event stream.")
			return
		}
		// Reconnect: resume an active turn, or replay frames the client missed.
		if turn := s.Attach(); turn != nil || lastID < s.Events().LastSeq() {
			streamFrames(w, flusher, r, s, turn, lastID)
			return
		}
	}

	h.startTurn(w, flusher, r, s, req.Prompt)
}

func (h *Handler) startTurn(w http.ResponseWriter, flusher http.Flusher, r *http.Request, s *session.Session, prompt string) {
	if strings.TrimSpace(prompt) == "" {
		problem.BadRequest(w, r, "prompt is required.")
		return
	}
	turn, startSeq, err := s.StartTurn(prompt)
	switch {
	case errors.Is(err, session.ErrTurnInProgress):
		problem.WriteProblem(w, r, http.StatusConflict, TypeSessionBusy, TitleSessionBusy,
			"Session "+s.ID()+" is currently executing an active turn.")
		return
	case errors.Is(err, session.ErrSessionClosed):
		problem.NotFound(w, r, "Session "+s.ID()+" does not exist.")
		return
	case errors.Is(err, session.ErrSessionFaulted):
		problem.Conflict(w, r, "Session "+s.ID()+" is in the error state; create a new session.")
		return
	case err != nil:
		problem.InternalServerError(w, r, "Failed to submit the prompt to the agent.")
		return
	}
	streamFrames(w, flusher, r, s, turn, startSeq)
}

// streamFrames writes SSE headers, then flushes every buffered frame after
// lastSeq and follows the live turn until it completes or the client goes
// away. A nil turn replays the frames buffered so far and returns. The
// subscriber is detached on return, arming the disconnect grace timer when
// it was the turn's last subscriber.
func streamFrames(w http.ResponseWriter, flusher http.Flusher, r *http.Request, s *session.Session, turn *session.Turn, lastSeq uint64) {
	defer s.Detach(turn)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	buf := s.Events()
	snapshot := buf.LastSeq()
	for {
		wait := buf.Wait()
		finished, limit := streamLimit(turn, snapshot)
		frames, inWindow := buf.ReplayAfter(lastSeq)
		if !inWindow {
			if !writeReplayGap(w) {
				return
			}
		}
		for _, f := range frames {
			if f.ID > limit {
				break
			}
			if _, err := f.WriteTo(w); err != nil {
				return
			}
			lastSeq = f.ID
		}
		flusher.Flush()
		// Every frame up to limit is already buffered once the turn finished.
		if finished {
			return
		}
		select {
		case <-wait:
		case <-turn.Done():
		case <-r.Context().Done():
			return
		}
	}
}

// streamLimit reports whether no further frames belong to this stream and the
// highest sequence ID it may deliver.
func streamLimit(turn *session.Turn, snapshot uint64) (finished bool, limit uint64) {
	if turn == nil {
		return true, snapshot
	}
	select {
	case <-turn.Done():
		return true, turn.EndSeq()
	default:
		return false, ^uint64(0)
	}
}

func writeReplayGap(w http.ResponseWriter) bool {
	gap, err := sse.NewFrame(0, sse.EventError, sse.ErrorPayload{
		Code:    CodeReplayGap,
		Message: "Some events were evicted from the replay buffer before they could be delivered.",
		Fatal:   false,
	})
	if err != nil {
		return false
	}
	_, err = gap.WriteTo(w)
	return err == nil
}

func parseLastEventID(v string) (id uint64, present bool, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false, nil
	}
	id, err = strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}
