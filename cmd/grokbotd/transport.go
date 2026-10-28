package main

import (
	"context"
	"errors"

	"github.com/captain-corgi/go-bot-example/internal/session"
)

// errAgentTransportUnavailable is returned when a session is created before a
// claude-agent-sdk-go subprocess transport has been integrated.
var errAgentTransportUnavailable = errors.New("grokbotd: no agent subprocess transport is configured")

// unavailableTransport is the daemon's transport until the
// claude-agent-sdk-go subprocess transport lands. Start always fails, so
// POST /sessions answers with an RFC 7807 500 instead of a fake agent.
type unavailableTransport struct{}

var _ session.Transport = unavailableTransport{}

func newAgentTransport() session.Transport { return unavailableTransport{} }

func (unavailableTransport) Start(context.Context, session.SessionConfig) error {
	return errAgentTransportUnavailable
}

func (unavailableTransport) SendPrompt(context.Context, string) (<-chan session.SDKEvent, error) {
	return nil, errAgentTransportUnavailable
}

func (unavailableTransport) Interrupt(context.Context) error { return nil }
func (unavailableTransport) Done() <-chan error              { return nil }
func (unavailableTransport) PGID() int                       { return 0 }
func (unavailableTransport) Close() error                    { return nil }
