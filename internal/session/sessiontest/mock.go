// Package sessiontest provides a deterministic fake agent subprocess transport
// for exercising session management and HTTP/SSE streaming offline.
package sessiontest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/session"
	"github.com/captain-corgi/go-bot-example/internal/sse"
)

// waitTimeout bounds how long test helpers block waiting for the system
// under test.
const waitTimeout = 5 * time.Second

// Script returns the events a scripted turn emits for prompt, in order.
// A scripted turn closes its event channel after the last event.
type Script func(prompt string) []session.SDKEvent

// FullTurn is a Script emitting one event of every transport-emittable kind:
// two assistant chunks, a tool call, its result, and done.
func FullTurn(prompt string) []session.SDKEvent {
	return []session.SDKEvent{
		session.AssistantChunk("Hello"),
		session.AssistantChunk(", " + prompt),
		session.ToolCall("call_1", "list_files", map[string]any{"path": "."}),
		session.ToolResult("call_1", sse.ToolStatusSuccess, "main.go\n", ""),
		session.Done(12, 7),
	}
}

// Option configures transports created by a MockFactory.
type Option func(*MockTransport)

// WithScript makes every turn emit script's events automatically. Without a
// script, turns are gated: the test drives them through NextTurn.
func WithScript(script Script) Option {
	return func(m *MockTransport) { m.script = script }
}

// WithStartError makes Start fail with err.
func WithStartError(err error) Option {
	return func(m *MockTransport) { m.startErr = err }
}

// WithSendError makes SendPrompt fail with err.
func WithSendError(err error) Option {
	return func(m *MockTransport) { m.sendErr = err }
}

// WithPGID makes PGID report pgid, enabling process group escalation.
func WithPGID(pgid int) Option {
	return func(m *MockTransport) { m.pgid = pgid }
}

// WithIgnoredInterrupts makes Interrupt leave open turns running, simulating
// an agent that does not honor the SDK interrupt and must be escalated to
// with SIGINT.
func WithIgnoredInterrupts() Option {
	return func(m *MockTransport) { m.ignoreInterrupts = true }
}

// WithInterruptError makes Interrupt fail with err (open turns stay running).
func WithInterruptError(err error) Option {
	return func(m *MockTransport) { m.interruptErr = err }
}

// WithCloseError makes Close fail with err after terminating the transport.
func WithCloseError(err error) Option {
	return func(m *MockTransport) { m.closeErr = err }
}

// MockFactory creates MockTransports and records every one it creates.
type MockFactory struct {
	opts []Option

	mu         sync.Mutex
	transports []*MockTransport
}

// NewMockFactory returns a factory applying opts to each transport.
func NewMockFactory(opts ...Option) *MockFactory {
	return &MockFactory{opts: slices.Clone(opts)}
}

// New creates and records a MockTransport. It satisfies session.TransportFactory.
func (f *MockFactory) New() session.Transport {
	m := &MockTransport{turns: make(chan *MockTurn, 16), done: make(chan error, 1)}
	for _, opt := range f.opts {
		opt(m)
	}
	f.mu.Lock()
	f.transports = append(f.transports, m)
	f.mu.Unlock()
	return m
}

// Transports returns every transport created so far, oldest first.
func (f *MockFactory) Transports() []*MockTransport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.transports)
}

// Last returns the most recently created transport, or nil.
func (f *MockFactory) Last() *MockTransport {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.transports) == 0 {
		return nil
	}
	return f.transports[len(f.transports)-1]
}

// MockTransport is a deterministic in-memory session.Transport. It never
// spawns processes or contacts model endpoints.
type MockTransport struct {
	script           Script
	startErr         error
	sendErr          error
	interruptErr     error
	closeErr         error
	pgid             int
	ignoreInterrupts bool
	turns            chan *MockTurn
	done             chan error

	mu             sync.Mutex
	started        bool
	exited         bool
	startCfg       session.SessionConfig
	startCtx       context.Context
	prompts        []string
	open           []*MockTurn
	closeCount     int
	interruptCount int
}

var _ session.Transport = (*MockTransport)(nil)

// Start records cfg and ctx.
func (m *MockTransport) Start(ctx context.Context, cfg session.SessionConfig) error {
	if m.startErr != nil {
		return m.startErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = true
	m.startCfg = cfg
	m.startCtx = ctx
	return nil
}

// SendPrompt records prompt and opens a turn. Scripted turns emit
// automatically; gated turns are published for NextTurn.
func (m *MockTransport) SendPrompt(ctx context.Context, prompt string) (<-chan session.SDKEvent, error) {
	if m.sendErr != nil {
		return nil, m.sendErr
	}
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil, errors.New("sessiontest: transport not started")
	}
	m.prompts = append(m.prompts, prompt)
	m.mu.Unlock()

	turn := &MockTurn{Prompt: prompt, ctx: ctx, events: make(chan session.SDKEvent), stop: make(chan struct{})}
	m.mu.Lock()
	m.open = append(m.open, turn)
	m.mu.Unlock()
	if m.script != nil {
		go turn.play(m.script(prompt))
		return turn.events, nil
	}
	select {
	case m.turns <- turn:
	default:
		return nil, errors.New("sessiontest: too many unclaimed gated turns")
	}
	return turn.events, nil
}

// Interrupt records the interrupt and, unless interrupts are ignored or
// failing, ends every open turn as an agent honoring the interrupt would.
func (m *MockTransport) Interrupt(context.Context) error {
	m.mu.Lock()
	m.interruptCount++
	if m.interruptErr != nil || m.ignoreInterrupts {
		m.mu.Unlock()
		return m.interruptErr
	}
	m.mu.Unlock()
	m.FinishOpenTurns()
	return nil
}

// Done delivers the simulated subprocess exit.
func (m *MockTransport) Done() <-chan error {
	return m.done
}

// PGID returns the configured process group ID (0 by default).
func (m *MockTransport) PGID() int {
	return m.pgid
}

// Close records the close, ends every open turn, and reports a clean exit.
func (m *MockTransport) Close() error {
	m.mu.Lock()
	m.closeCount++
	m.mu.Unlock()
	m.exit(nil)
	m.FinishOpenTurns()
	return m.closeErr
}

// Crash simulates an unexpected subprocess exit with err: the exit is
// reported on Done, then every open turn's event stream ends.
func (m *MockTransport) Crash(err error) {
	m.exit(err)
	m.FinishOpenTurns()
}

// FinishOpenTurns ends every turn opened so far.
func (m *MockTransport) FinishOpenTurns() {
	m.mu.Lock()
	open := m.open
	m.open = nil
	m.mu.Unlock()
	for _, t := range open {
		t.Finish()
	}
}

func (m *MockTransport) exit(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exited {
		return
	}
	m.exited = true
	m.done <- err
	close(m.done)
}

// Exited reports whether the simulated subprocess has exited.
func (m *MockTransport) Exited() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exited
}

// InterruptCount returns how many times Interrupt was called.
func (m *MockTransport) InterruptCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.interruptCount
}

// StartConfig returns the config passed to Start and whether Start ran.
func (m *MockTransport) StartConfig() (session.SessionConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCfg, m.started
}

// StartContext returns the lifetime context passed to Start.
func (m *MockTransport) StartContext() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCtx
}

// Prompts returns every prompt submitted, oldest first.
func (m *MockTransport) Prompts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.prompts)
}

// CloseCount returns how many times Close was called.
func (m *MockTransport) CloseCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeCount
}

// NextTurn returns the next gated turn, failing t if none opens in time.
func (m *MockTransport) NextTurn(t testing.TB) *MockTurn {
	t.Helper()
	select {
	case turn := <-m.turns:
		return turn
	case <-time.After(waitTimeout):
		t.Fatal("sessiontest: no turn was started")
		return nil
	}
}

// ErrTurnFinished is returned by Emit once the turn has been finished.
var ErrTurnFinished = errors.New("sessiontest: turn finished")

// MockTurn is one in-flight turn of a MockTransport.
type MockTurn struct {
	Prompt string

	ctx      context.Context
	events   chan session.SDKEvent
	stop     chan struct{}
	stopOnce sync.Once

	sendMu   sync.Mutex // serializes sends with closing events
	finished bool
}

// Emit delivers ev to the session, blocking until it is consumed. It returns
// the context error if the turn was cancelled first, or ErrTurnFinished if
// the turn was finished.
func (t *MockTurn) Emit(ev session.SDKEvent) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	if t.finished {
		return ErrTurnFinished
	}
	select {
	case t.events <- ev:
		return nil
	case <-t.ctx.Done():
		return t.ctx.Err()
	case <-t.stop:
		return ErrTurnFinished
	}
}

// MustEmit emits each event in order, failing tb on cancellation.
func (t *MockTurn) MustEmit(tb testing.TB, events ...session.SDKEvent) {
	tb.Helper()
	for _, ev := range events {
		if err := t.Emit(ev); err != nil {
			tb.Fatalf("sessiontest: emit %s: %v", ev.Type, err)
		}
	}
}

// Finish closes the turn's event channel, simulating the end of the turn.
// It is safe to call concurrently with Emit and more than once.
func (t *MockTurn) Finish() {
	t.stopOnce.Do(func() { close(t.stop) })
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	if !t.finished {
		t.finished = true
		close(t.events)
	}
}

// Context returns the turn context handed to SendPrompt.
func (t *MockTurn) Context() context.Context {
	return t.ctx
}

// WaitCancelled fails tb unless the turn context is cancelled in time.
func (t *MockTurn) WaitCancelled(tb testing.TB) {
	tb.Helper()
	select {
	case <-t.ctx.Done():
	case <-time.After(waitTimeout):
		tb.Fatal("sessiontest: turn context was not cancelled")
	}
}

func (t *MockTurn) play(events []session.SDKEvent) {
	defer t.Finish()
	for _, ev := range events {
		if t.Emit(ev) != nil {
			return
		}
	}
}
