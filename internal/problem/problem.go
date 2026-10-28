// Package problem serializes HTTP error responses as RFC 7807 Problem Details
// (application/problem+json). It is the single source of truth for HTTP error
// payloads emitted by the Grok Bot daemon.
package problem

import (
	"encoding/json"
	"net/http"
)

// ContentType is the RFC 7807 media type for Problem Details JSON documents.
const ContentType = "application/problem+json"

// Canonical problem type URIs for the spec-mandated error classes.
const (
	TypeBadRequest     = "https://grokbot.local/problems/bad-request"
	TypeNotFound       = "https://grokbot.local/problems/not-found"
	TypeConflict       = "https://grokbot.local/problems/conflict"
	TypeInternalServer = "https://grokbot.local/problems/internal-server-error"
)

// Details is an RFC 7807 Problem Details document.
type Details struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
}

// WriteProblem writes an RFC 7807 Problem Details response with the given
// status code. The instance field is derived from the request URL path when
// r is non-nil.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, problemType, title, detail string) {
	body := Details{
		Type:     problemType,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: instanceOf(r),
	}

	h := w.Header()
	h.Set("Content-Type", ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	// The status line is already committed; an encoding failure here cannot be
	// reported to the client, so it is intentionally ignored.
	_ = json.NewEncoder(w).Encode(body)
}

// BadRequest writes a 400 Bad Request problem response.
func BadRequest(w http.ResponseWriter, r *http.Request, detail string) {
	WriteProblem(w, r, http.StatusBadRequest, TypeBadRequest, http.StatusText(http.StatusBadRequest), detail)
}

// NotFound writes a 404 Not Found problem response.
func NotFound(w http.ResponseWriter, r *http.Request, detail string) {
	WriteProblem(w, r, http.StatusNotFound, TypeNotFound, http.StatusText(http.StatusNotFound), detail)
}

// Conflict writes a 409 Conflict problem response.
func Conflict(w http.ResponseWriter, r *http.Request, detail string) {
	WriteProblem(w, r, http.StatusConflict, TypeConflict, http.StatusText(http.StatusConflict), detail)
}

// InternalServerError writes a 500 Internal Server Error problem response.
func InternalServerError(w http.ResponseWriter, r *http.Request, detail string) {
	WriteProblem(w, r, http.StatusInternalServerError, TypeInternalServer,
		http.StatusText(http.StatusInternalServerError), detail)
}

func instanceOf(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.Path
}
