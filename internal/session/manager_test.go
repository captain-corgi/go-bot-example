package session_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/session"
	"github.com/captain-corgi/go-bot-example/internal/session/sessiontest"
)

func newManager(t *testing.T, factory *sessiontest.MockFactory, mutate ...func(*session.Options)) *session.Manager {
	t.Helper()
	opts := session.Options{NewTransport: factory.New}
	for _, fn := range mutate {
		fn(&opts)
	}
	m, err := session.NewManager(opts)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestNewManagerRequiresTransportFactory(t *testing.T) {
	if _, err := session.NewManager(session.Options{}); err == nil {
		t.Fatal("expected error without NewTransport")
	}
}

func TestCreateSpawnsIsolatedIdleSession(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	var permissionCalls atomic.Int32
	canUse := func(context.Context, session.ToolPermissionRequest) (session.PermissionDecision, error) {
		permissionCalls.Add(1)
		return session.PermissionAllow, nil
	}
	tool := session.ToolRegistration{Name: "echo", Handler: func(context.Context, map[string]any) (session.ToolOutput, error) {
		return session.ToolOutput{Stdout: "ok"}, nil
	}}
	m := newManager(t, factory, func(o *session.Options) {
		o.Tools = []session.ToolRegistration{tool}
		o.CanUseTool = canUse
	})

	s1, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{
		Prompt: "be terse", Config: map[string]any{"model": "grok"},
	})
	if err != nil {
		t.Fatalf("CreateOrResume: %v", err)
	}
	s2, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("CreateOrResume: %v", err)
	}

	if !strings.HasPrefix(s1.ID(), "sess_") || s1.ID() == s2.ID() {
		t.Fatalf("ids %q, %q: want distinct sess_ ids", s1.ID(), s2.ID())
	}
	if s1.State() != session.StateIdle {
		t.Errorf("state = %q, want idle", s1.State())
	}
	if got, ok := m.Get(s1.ID()); !ok || got != s1 {
		t.Error("session not registered")
	}
	transports := factory.Transports()
	if len(transports) != 2 {
		t.Fatalf("transports = %d, want 2 (one per session)", len(transports))
	}

	cfg, started := transports[0].StartConfig()
	if !started {
		t.Fatal("transport not started")
	}
	if cfg.SessionID != s1.ID() || cfg.InitialPrompt != "be terse" || cfg.Options["model"] != "grok" || cfg.Resume {
		t.Errorf("unexpected start config: %+v", cfg)
	}
	if len(cfg.Tools) != 1 || cfg.Tools[0].Name != "echo" || cfg.CanUseTool == nil {
		t.Fatalf("MCP tools / CanUseTool not wired: %+v", cfg)
	}
	if _, err := cfg.CanUseTool(context.Background(), session.ToolPermissionRequest{ToolName: "echo"}); err != nil ||
		permissionCalls.Load() != 1 {
		t.Error("CanUseTool hook did not reach the configured callback")
	}
}

func TestResumeActiveSessionReturnsSameSession(t *testing.T) {
	factory := sessiontest.NewMockFactory()
	m := newManager(t, factory)
	s, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	again, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{SessionID: s.ID()})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if again != s || len(factory.Transports()) != 1 {
		t.Error("resuming an active session must reattach without spawning a transport")
	}
}

func TestResumeRestoresPersistedState(t *testing.T) {
	store := session.NewMemoryStore()
	factory := sessiontest.NewMockFactory(sessiontest.WithScript(sessiontest.FullTurn))
	withStore := func(o *session.Options) { o.Store = store }

	first := newManager(t, factory, withStore)
	s, err := first.CreateOrResume(context.Background(), session.CreateSessionRequest{Prompt: "init"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	turn, _, err := s.StartTurn("world")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	waitDone(t, turn)
	s.Detach(turn)

	// A fresh registry sharing the store must restore the session.
	second := newManager(t, factory, withStore)
	restored, err := second.CreateOrResume(context.Background(), session.CreateSessionRequest{SessionID: s.ID()})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if restored.ID() != s.ID() || restored.State() != session.StateIdle {
		t.Fatalf("restored %q in %q", restored.ID(), restored.State())
	}
	cfg, _ := factory.Last().StartConfig()
	want := []session.Message{{Role: session.RoleUser, Content: "world"}, {Role: session.RoleAssistant, Content: "Hello, world"}}
	if !cfg.Resume || cfg.InitialPrompt != "init" || !equalHistory(cfg.History, want) {
		t.Errorf("resume config = %+v, want Resume with history %v", cfg, want)
	}
	if !equalHistory(restored.History(), want) {
		t.Errorf("restored history = %v, want %v", restored.History(), want)
	}
}

func TestCreateOrResumeErrors(t *testing.T) {
	m := newManager(t, sessiontest.NewMockFactory())
	if _, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{SessionID: "sess_missing"}); !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("unknown id err = %v, want ErrSessionNotFound", err)
	}
	if _, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{SessionID: "../etc"}); !errors.Is(err, session.ErrInvalidSessionID) {
		t.Errorf("malformed id err = %v, want ErrInvalidSessionID", err)
	}

	failing := newManager(t, sessiontest.NewMockFactory(sessiontest.WithStartError(errors.New("spawn failed"))))
	if _, err := failing.CreateOrResume(context.Background(), session.CreateSessionRequest{}); err == nil {
		t.Error("expected transport start error")
	}
}

func TestTryBeginTurnMutualExclusion(t *testing.T) {
	m := newManager(t, sessiontest.NewMockFactory())
	s, err := m.CreateOrResume(context.Background(), session.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const callers = 64
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ctx, cancel, ok := s.TryBeginTurn(context.Background()); ok {
				if ctx == nil || cancel == nil {
					t.Error("winner received nil turn context")
				}
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins.Load() != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins.Load())
	}
	if s.State() != session.StateRunning {
		t.Errorf("state = %q, want running", s.State())
	}
	s.CompleteTurn(session.StateIdle)
	if _, cancel, ok := s.TryBeginTurn(context.Background()); !ok {
		t.Error("TryBeginTurn after CompleteTurn failed")
	} else {
		cancel()
	}
}

func waitDone(t *testing.T, turn *session.Turn) {
	t.Helper()
	select {
	case <-turn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not complete")
	}
}

func equalHistory(got, want []session.Message) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
