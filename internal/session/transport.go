// Package session manages isolated agent sessions for the Grok Bot daemon: an
// in-memory registry mapping each session ID to a dedicated agent subprocess
// transport, a persisted session state store for resumption, per-session turn
// exclusivity, and turn execution feeding the session's SSE replay buffer.
package session

import (
	"context"

	"github.com/captain-corgi/go-bot-example/internal/sse"
)

// SessionState is the lifecycle state of a session.
type SessionState string

// Session lifecycle states.
const (
	StateIdle        SessionState = "idle"
	StateRunning     SessionState = "running"
	StateInterrupted SessionState = "interrupted"
	StateError       SessionState = "error"
)

// Message is one entry of a session's persisted conversation history.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Conversation roles recorded in session history.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// ToolOutput is the result of an in-process MCP tool invocation.
type ToolOutput struct {
	Stdout  string
	Stderr  string
	IsError bool
}

// ToolHandler executes an in-process MCP tool.
type ToolHandler func(ctx context.Context, args map[string]any) (ToolOutput, error)

// ToolRegistration registers an in-process MCP tool with the agent transport.
type ToolRegistration struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     ToolHandler
}

// PermissionDecision is the outcome of a CanUseTool pre-execution check.
type PermissionDecision string

// Permission decisions.
const (
	PermissionAllow  PermissionDecision = "allow"
	PermissionDeny   PermissionDecision = "deny"
	PermissionPrompt PermissionDecision = "prompt"
)

// ToolPermissionRequest describes a tool invocation awaiting permission.
type ToolPermissionRequest struct {
	SessionID string
	ToolName  string
	Arguments map[string]any
}

// CanUseToolFunc gates a tool invocation before it executes.
type CanUseToolFunc func(ctx context.Context, req ToolPermissionRequest) (PermissionDecision, error)

// SessionConfig is handed to Transport.Start when a session's dedicated agent
// subprocess is spawned (new session) or reattached (resumed session).
type SessionConfig struct {
	// SessionID is the daemon-assigned session identifier.
	SessionID string
	// InitialPrompt is the optional prompt supplied when the session was created.
	InitialPrompt string
	// Options carries client-supplied configuration (e.g. model selection).
	Options map[string]any
	// Resume is true when the transport must restore persisted conversation
	// state instead of starting a fresh conversation.
	Resume bool
	// History is the persisted conversation to restore when Resume is true.
	History []Message
	// Tools are in-process MCP tools registered with the agent.
	Tools []ToolRegistration
	// CanUseTool is the pre-execution permission callback for tool calls.
	CanUseTool CanUseToolFunc
}

// SDKEvent is one event emitted by the agent during a turn. Payload must be
// the sse payload type matching Type: sse.AssistantChunkPayload,
// sse.ToolCallPayload, sse.ToolResultPayload, sse.DonePayload or
// sse.ErrorPayload. Session state frames are owned by the session itself.
type SDKEvent struct {
	Type    sse.EventType
	Payload any
}

// Transport is a dedicated, bidirectional agent subprocess bound 1:1 to a
// session. Production implementations wrap the claude-agent-sdk-go client;
// tests use a deterministic fake.
type Transport interface {
	// Start spawns or reattaches the agent subprocess. ctx bounds the
	// subprocess lifetime and is cancelled when the session is torn down.
	Start(ctx context.Context, cfg SessionConfig) error
	// SendPrompt submits a prompt turn and returns the turn's event stream.
	// The transport closes the channel when the turn ends. Senders must select
	// on ctx.Done() so a cancelled turn never blocks the transport.
	SendPrompt(ctx context.Context, prompt string) (<-chan SDKEvent, error)
	// Close terminates the agent subprocess and releases its descriptors.
	Close() error
}

// TransportFactory creates a new Transport for each session.
type TransportFactory func() Transport

// AssistantChunk returns an assistant_chunk event.
func AssistantChunk(delta string) SDKEvent {
	return SDKEvent{Type: sse.EventAssistantChunk, Payload: sse.AssistantChunkPayload{Delta: delta}}
}

// ToolCall returns a tool_call event.
func ToolCall(id, name string, args map[string]any) SDKEvent {
	if args == nil {
		args = map[string]any{}
	}
	return SDKEvent{Type: sse.EventToolCall, Payload: sse.ToolCallPayload{ToolCallID: id, Name: name, Arguments: args}}
}

// ToolResult returns a tool_result event.
func ToolResult(id string, status sse.ToolStatus, stdout, stderr string) SDKEvent {
	return SDKEvent{Type: sse.EventToolResult, Payload: sse.ToolResultPayload{
		ToolCallID: id, Status: status, Stdout: stdout, Stderr: stderr,
	}}
}

// Done returns a done event with total tokens derived from input and output.
func Done(inputTokens, outputTokens int) SDKEvent {
	return SDKEvent{Type: sse.EventDone, Payload: sse.DonePayload{Usage: sse.Usage{
		InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens,
	}}}
}

// StreamError returns a stream-level error event.
func StreamError(code, message string, fatal bool) SDKEvent {
	return SDKEvent{Type: sse.EventError, Payload: sse.ErrorPayload{Code: code, Message: message, Fatal: fatal}}
}

// validEvent reports whether ev is a transport-emittable event whose payload
// matches its type.
func validEvent(ev SDKEvent) bool {
	var ok bool
	switch ev.Type {
	case sse.EventAssistantChunk:
		_, ok = ev.Payload.(sse.AssistantChunkPayload)
	case sse.EventToolCall:
		_, ok = ev.Payload.(sse.ToolCallPayload)
	case sse.EventToolResult:
		_, ok = ev.Payload.(sse.ToolResultPayload)
	case sse.EventDone:
		_, ok = ev.Payload.(sse.DonePayload)
	case sse.EventError:
		_, ok = ev.Payload.(sse.ErrorPayload)
	}
	return ok
}
