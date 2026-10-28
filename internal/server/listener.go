package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
)

// LoopbackHost is the only host the daemon is permitted to bind to.
const LoopbackHost = "127.0.0.1"

// ErrNonLoopbackAddr is returned when a bind address targets anything other
// than the IPv4 loopback address 127.0.0.1.
var ErrNonLoopbackAddr = errors.New("bind address must be 127.0.0.1")

// ValidateLoopbackAddr parses addr ("host:port") and verifies that the host is
// literally 127.0.0.1 and the port is within [0, 65535]. Port 0 is allowed so
// callers can request an ephemeral loopback port.
func ValidateLoopbackAddr(addr string) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid bind address %q: %w", addr, err)
	}
	if h == "" {
		return "", 0, fmt.Errorf("invalid bind address %q: wildcard host refused: %w", addr, ErrNonLoopbackAddr)
	}
	ip := net.ParseIP(h)
	if h != LoopbackHost || ip == nil || !ip.Equal(net.IPv4(127, 0, 0, 1)) {
		return "", 0, fmt.Errorf("invalid bind address %q: host %q refused: %w", addr, h, ErrNonLoopbackAddr)
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 || n > 65535 {
		return "", 0, fmt.Errorf("invalid bind address %q: port %q must be an integer in [0, 65535]", addr, p)
	}
	return h, n, nil
}

// ListenLoopback validates addr and opens an IPv4 TCP listener bound strictly
// to 127.0.0.1. No socket is opened when validation fails.
func ListenLoopback(ctx context.Context, addr string) (net.Listener, error) {
	_, port, err := ValidateLoopbackAddr(addr)
	if err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp4", net.JoinHostPort(LoopbackHost, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}
