// Package api implements the Grok Bot daemon's session HTTP endpoints:
// session creation/resumption, SSE prompt streaming, turn interruption, and
// session teardown.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/captain-corgi/go-bot-example/internal/problem"
	"github.com/captain-corgi/go-bot-example/internal/session"
)

// maxBodyBytes bounds request bodies accepted by the session endpoints.
const maxBodyBytes = 1 << 20

// Registrar registers HTTP handlers on ServeMux-style patterns. It is
// satisfied by *server.Router.
type Registrar interface {
	HandleFunc(pattern string, handler http.HandlerFunc)
}

// Handler serves the session HTTP API.
type Handler struct {
	manager session.SessionManager
}

// NewHandler returns a Handler backed by manager.
func NewHandler(manager session.SessionManager) *Handler {
	return &Handler{manager: manager}
}

// Register installs the session routes on r.
func (h *Handler) Register(r Registrar) {
	r.HandleFunc("POST /sessions", h.createSession)
	r.HandleFunc("POST /sessions/{id}/prompts", h.submitPrompt)
	r.HandleFunc("POST /sessions/{id}/interrupt", h.interruptSession)
	r.HandleFunc("DELETE /sessions/{id}", h.closeSession)
}

type createSessionResponse struct {
	SessionID string               `json:"session_id"`
	Status    session.SessionState `json:"status"`
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	var req session.CreateSessionRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		problem.BadRequest(w, r, "Request body must be a JSON object {session_id?, prompt?, config?}: "+err.Error())
		return
	}

	s, err := h.manager.CreateOrResume(r.Context(), req)
	switch {
	case errors.Is(err, session.ErrInvalidSessionID):
		problem.BadRequest(w, r, "session_id must be 1-128 characters of letters, digits, '-' or '_'.")
		return
	case errors.Is(err, session.ErrSessionNotFound):
		problem.NotFound(w, r, "Session "+req.SessionID+" does not exist.")
		return
	case err != nil:
		problem.InternalServerError(w, r, "Failed to create or resume the agent session.")
		return
	}

	writeJSON(w, http.StatusOK, createSessionResponse{SessionID: s.ID(), Status: s.State()})
}

// decodeJSONBody decodes a single JSON object from the request body into dst,
// rejecting unknown fields and trailing data. An empty body leaves dst as is.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is committed; an encoding failure cannot be reported.
	_ = json.NewEncoder(w).Encode(body)
}
