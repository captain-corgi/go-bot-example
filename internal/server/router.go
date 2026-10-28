package server

import (
	"net/http"

	"github.com/captain-corgi/go-bot-example/internal/problem"
)

// Router is the base HTTP router for the daemon. Session handlers register
// routes on it using net/http ServeMux patterns (e.g. "POST /sessions/{id}/prompts").
// Requests that match no registered pattern, including registered paths with
// an unsupported method, receive an RFC 7807 404 Not Found response.
type Router struct {
	mux *http.ServeMux
}

// NewRouter returns a Router with the RFC 7807 404 fallback installed.
func NewRouter() *Router {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		problem.NotFound(w, r, "No route matches "+r.Method+" "+r.URL.Path+".")
	})
	return &Router{mux: mux}
}

// Handle registers handler for the given ServeMux pattern.
func (rt *Router) Handle(pattern string, handler http.Handler) {
	rt.mux.Handle(pattern, handler)
}

// HandleFunc registers handler for the given ServeMux pattern.
func (rt *Router) HandleFunc(pattern string, handler http.HandlerFunc) {
	rt.mux.HandleFunc(pattern, handler)
}

// ServeHTTP dispatches the request to the matching registered handler.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.mux.ServeHTTP(w, r)
}
