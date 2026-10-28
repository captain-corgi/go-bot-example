package api

import (
	"errors"
	"net/http"

	"github.com/captain-corgi/go-bot-example/internal/problem"
	"github.com/captain-corgi/go-bot-example/internal/session"
)

// Lifecycle response statuses.
const (
	StatusInterrupted = "interrupted"
	StatusClosed      = "closed"
)

type statusResponse struct {
	Status string `json:"status"`
}

// interruptSession handles POST /sessions/{id}/interrupt. It halts the
// active turn, if any, and answers within the manager's interrupt timeout.
// Interrupting an idle session is an idempotent no-op.
func (h *Handler) interruptSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := decodeJSONBody(w, r, &struct{}{}); err != nil {
		problem.BadRequest(w, r, "Request body must be empty or {}: "+err.Error())
		return
	}
	if !writeLifecycleError(w, r, id, h.manager.Interrupt(r.Context(), id), "interrupt the session turn") {
		writeJSON(w, http.StatusOK, statusResponse{Status: StatusInterrupted})
	}
}

// closeSession handles DELETE /sessions/{id}, tearing the session down in any
// state.
func (h *Handler) closeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := decodeJSONBody(w, r, &struct{}{}); err != nil {
		problem.BadRequest(w, r, "Request body must be empty: "+err.Error())
		return
	}
	if !writeLifecycleError(w, r, id, h.manager.CloseSession(r.Context(), id), "close the session") {
		writeJSON(w, http.StatusOK, statusResponse{Status: StatusClosed})
	}
}

// writeLifecycleError writes the RFC 7807 response for err and reports
// whether one was written.
func writeLifecycleError(w http.ResponseWriter, r *http.Request, id string, err error, action string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, session.ErrInvalidSessionID):
		problem.BadRequest(w, r, "Session ID must be 1-128 characters of letters, digits, '-' or '_'.")
	case errors.Is(err, session.ErrSessionNotFound), errors.Is(err, session.ErrSessionClosed):
		problem.NotFound(w, r, "Session "+id+" does not exist.")
	default:
		problem.InternalServerError(w, r, "Failed to "+action+".")
	}
	return true
}
