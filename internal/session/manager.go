package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/sse"
)

var (
	// ErrSessionNotFound is returned when a session ID exists in neither the
	// active registry nor the persisted store.
	ErrSessionNotFound = errors.New("session: not found")
	// ErrInvalidSessionID is returned for malformed client-supplied IDs.
	ErrInvalidSessionID = errors.New("session: invalid session id")
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// CreateSessionRequest is the body of POST /sessions.
type CreateSessionRequest struct {
	SessionID string         `json:"session_id,omitempty"`
	Prompt    string         `json:"prompt,omitempty"`
	Config    map[string]any `json:"config,omitempty"`
}

// SessionManager creates, resumes, looks up, interrupts, and tears down
// sessions.
type SessionManager interface {
	CreateOrResume(ctx context.Context, req CreateSessionRequest) (*Session, error)
	Get(sessionID string) (*Session, bool)
	Interrupt(ctx context.Context, sessionID string) error
	CloseSession(ctx context.Context, sessionID string) error
}

// Options configures a Manager.
type Options struct {
	// NewTransport creates the dedicated transport for each session. Required.
	NewTransport TransportFactory
	// Store persists session state for resumption. Defaults to a MemoryStore.
	Store Store
	// Tools are in-process MCP tools registered with every session transport.
	Tools []ToolRegistration
	// CanUseTool is the permission callback wired into every session transport.
	CanUseTool CanUseToolFunc
	// BufferCapacity is the per-session replay buffer size. Defaults to
	// sse.DefaultCapacity.
	BufferCapacity int
	// DisconnectGrace is how long a turn survives without SSE subscribers.
	// Defaults to sse.DefaultDisconnectGrace (60s).
	DisconnectGrace time.Duration
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// InterruptTimeout bounds how long Interrupt blocks. Defaults to
	// DefaultInterruptTimeout (500ms).
	InterruptTimeout time.Duration
	// InterruptEscalation is how long an interrupted turn may stay unhalted
	// before SIGINT is sent to the agent's process group. Defaults to
	// DefaultInterruptEscalation; it is capped at InterruptTimeout.
	InterruptEscalation time.Duration
	// SignalProcessGroup delivers interrupt escalation signals. Defaults to
	// SignalProcessGroup (kill(2) on the negative process group ID).
	SignalProcessGroup GroupSignaler
}

// Manager is the in-memory session registry. It is safe for concurrent use.
type Manager struct {
	opts     Options
	mu       sync.RWMutex
	sessions map[string]*Session
}

var _ SessionManager = (*Manager)(nil)

// NewManager returns a Manager configured by opts.
func NewManager(opts Options) (*Manager, error) {
	if opts.NewTransport == nil {
		return nil, errors.New("session: NewTransport is required")
	}
	if opts.Store == nil {
		opts.Store = NewMemoryStore()
	}
	if opts.DisconnectGrace <= 0 {
		opts.DisconnectGrace = sse.DefaultDisconnectGrace
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.InterruptTimeout <= 0 {
		opts.InterruptTimeout = DefaultInterruptTimeout
	}
	if opts.InterruptEscalation <= 0 {
		opts.InterruptEscalation = DefaultInterruptEscalation
	}
	opts.InterruptEscalation = min(opts.InterruptEscalation, opts.InterruptTimeout)
	if opts.SignalProcessGroup == nil {
		opts.SignalProcessGroup = SignalProcessGroup
	}
	opts.Tools = slices.Clone(opts.Tools)
	return &Manager{opts: opts, sessions: make(map[string]*Session)}, nil
}

// Get returns the active session with the given ID.
func (m *Manager) Get(sessionID string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[sessionID]
	return s, ok
}

// CreateOrResume provisions a new isolated session when req.SessionID is
// empty. Otherwise it returns the active session with that ID, or restores it
// from the persisted store on a fresh transport. Prompt and Config only apply
// to new sessions; a resumed session keeps its persisted configuration.
func (m *Manager) CreateOrResume(ctx context.Context, req CreateSessionRequest) (*Session, error) {
	if req.SessionID == "" {
		return m.create(ctx, req)
	}
	if !sessionIDPattern.MatchString(req.SessionID) {
		return nil, ErrInvalidSessionID
	}
	if s, ok := m.Get(req.SessionID); ok {
		return s, nil
	}
	return m.resume(ctx, req.SessionID)
}

func (m *Manager) create(ctx context.Context, req CreateSessionRequest) (*Session, error) {
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	now := m.opts.Now()
	rec := Record{
		ID:            id,
		InitialPrompt: req.Prompt,
		Options:       maps.Clone(req.Config),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	s, err := m.spawn(ctx, rec, false)
	if err != nil {
		return nil, err
	}
	if err := m.opts.Store.Save(rec); err != nil {
		s.discard()
		return nil, fmt.Errorf("session %s: persist: %w", id, err)
	}
	return m.register(s), nil
}

func (m *Manager) resume(ctx context.Context, id string) (*Session, error) {
	rec, ok, err := m.opts.Store.Load(id)
	if err != nil {
		return nil, fmt.Errorf("session %s: load: %w", id, err)
	}
	if !ok {
		return nil, ErrSessionNotFound
	}
	s, err := m.spawn(ctx, rec, true)
	if err != nil {
		return nil, err
	}
	return m.register(s), nil
}

// register adds s to the registry and starts its fault monitor unless a
// concurrent resume already registered the same ID, in which case s is
// discarded and the winner returned.
func (m *Manager) register(s *Session) *Session {
	m.mu.Lock()
	existing, ok := m.sessions[s.id]
	if !ok {
		m.sessions[s.id] = s
	}
	m.mu.Unlock()
	if ok {
		s.discard()
		return existing
	}
	if done := s.transport.Done(); done != nil {
		go s.monitor(done)
	}
	return s
}

// lookup returns the active session with a well-formed ID.
func (m *Manager) lookup(sessionID string) (*Session, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, ErrInvalidSessionID
	}
	s, ok := m.Get(sessionID)
	if !ok {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

// Interrupt halts the active turn of the session, if any, within the
// configured interrupt timeout. See Session.Interrupt.
func (m *Manager) Interrupt(ctx context.Context, sessionID string) error {
	s, err := m.lookup(sessionID)
	if err != nil {
		return err
	}
	if err := s.Interrupt(ctx); errors.Is(err, ErrSessionClosed) {
		return ErrSessionNotFound
	}
	return nil
}

// CloseSession removes the session from the registry and tears it down in
// any state: the active turn and grace timer are cancelled, the dedicated
// subprocess and its process group are terminated, and the replay buffer is
// purged. The persisted record is kept so the session can still be resumed.
func (m *Manager) CloseSession(_ context.Context, sessionID string) error {
	if !sessionIDPattern.MatchString(sessionID) {
		return ErrInvalidSessionID
	}
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	m.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}
	if err := s.close(); err != nil && !errors.Is(err, ErrSessionClosed) {
		return err
	}
	return nil
}

// Shutdown tears down every active session. It returns the joined errors of
// sessions whose transport failed to close.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	var errs []error
	for _, id := range ids {
		if err := m.CloseSession(ctx, id); err != nil && !errors.Is(err, ErrSessionNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// spawn starts a dedicated transport for rec and wraps it in an idle Session.
func (m *Manager) spawn(ctx context.Context, rec Record, resume bool) (*Session, error) {
	transport := m.opts.NewTransport()
	if transport == nil {
		return nil, errors.New("session: transport factory returned nil")
	}
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cfg := SessionConfig{
		SessionID:     rec.ID,
		InitialPrompt: rec.InitialPrompt,
		Options:       maps.Clone(rec.Options),
		Resume:        resume,
		History:       slices.Clone(rec.History),
		Tools:         slices.Clone(m.opts.Tools),
		CanUseTool:    m.opts.CanUseTool,
	}
	if err := transport.Start(lifetime, cfg); err != nil {
		cancel()
		return nil, fmt.Errorf("session %s: start transport: %w", rec.ID, err)
	}
	return &Session{
		id:        rec.ID,
		transport: transport,
		events:    sse.NewEventRingBuffer(m.opts.BufferCapacity),
		store:     m.opts.Store,
		graceTTL:  m.opts.DisconnectGrace,
		now:       m.opts.Now,
		interrupt: interruptConfig{
			timeout:    m.opts.InterruptTimeout,
			escalation: m.opts.InterruptEscalation,
			signal:     m.opts.SignalProcessGroup,
		},
		exited:         make(chan struct{}),
		lifetime:       lifetime,
		cancelLifetime: cancel,
		state:          StateIdle,
		record:         rec.clone(),
	}, nil
}

// discard releases a session that was never registered.
func (s *Session) discard() {
	s.cancelLifetime()
	_ = s.transport.Close()
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session: generate id: %w", err)
	}
	return "sess_" + hex.EncodeToString(b[:]), nil
}
