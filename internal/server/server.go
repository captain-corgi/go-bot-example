// Package server provides the loopback-only HTTP server scaffolding for the
// Grok Bot daemon: strict 127.0.0.1 binding, Host header DNS rebinding
// protection, and the base router with RFC 7807 error fallbacks.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
)

// Server is an HTTP server bound strictly to 127.0.0.1 that validates the
// Host header of every request before dispatching to its handler.
type Server struct {
	listener   net.Listener
	httpServer *http.Server
	port       int
}

// New validates addr, opens a 127.0.0.1 listener, and wraps handler with
// HostValidationMiddleware for the bound port. It fails before opening any
// socket when addr is not a 127.0.0.1 address.
func New(ctx context.Context, addr string, handler http.Handler) (*Server, error) {
	if handler == nil {
		return nil, errors.New("server: nil handler")
	}
	ln, err := ListenLoopback(ctx, addr)
	if err != nil {
		return nil, err
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("server: unexpected listener address type %T", ln.Addr())
	}

	// WriteTimeout is intentionally unset so long-lived SSE streams are not cut off.
	httpServer := &http.Server{
		Handler:           HostValidationMiddleware(tcpAddr.Port, handler),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	return &Server{listener: ln, httpServer: httpServer, port: tcpAddr.Port}, nil
}

// Addr returns the bound listener address.
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// Port returns the bound TCP port.
func (s *Server) Port() int {
	return s.port
}

// Serve accepts connections until the server is shut down. It returns nil
// after a graceful Shutdown.
func (s *Server) Serve() error {
	if err := s.httpServer.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server and releases the listener.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.httpServer.Shutdown(ctx)
	if cerr := s.listener.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) && err == nil {
		err = cerr
	}
	return err
}
