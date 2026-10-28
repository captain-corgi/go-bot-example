// Package sse implements the Server-Sent Events wire protocol used by the
// Grok Bot daemon: the six canonical event payloads, frame encoding with
// monotonically increasing sequence IDs, and the per-session in-memory replay
// ring buffer with its disconnect grace timer.
package sse

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// EventType is the SSE "event:" name of a frame.
type EventType string

// Canonical SSE event types.
const (
	EventAssistantChunk EventType = "assistant_chunk"
	EventToolCall       EventType = "tool_call"
	EventToolResult     EventType = "tool_result"
	EventSessionState   EventType = "session_state"
	EventDone           EventType = "done"
	EventError          EventType = "error"
)

// Valid reports whether t is one of the six canonical event types.
func (t EventType) Valid() bool {
	switch t {
	case EventAssistantChunk, EventToolCall, EventToolResult, EventSessionState, EventDone, EventError:
		return true
	default:
		return false
	}
}

// ToolStatus is the execution status carried by a tool_result event.
type ToolStatus string

// Tool result statuses.
const (
	ToolStatusSuccess ToolStatus = "success"
	ToolStatusError   ToolStatus = "error"
)

// AssistantChunkPayload is the data of an assistant_chunk event.
type AssistantChunkPayload struct {
	Delta string `json:"delta"`
}

// ToolCallPayload is the data of a tool_call event.
type ToolCallPayload struct {
	ToolCallID string         `json:"tool_call_id"`
	Name       string         `json:"name"`
	Arguments  map[string]any `json:"arguments"`
}

// ToolResultPayload is the data of a tool_result event.
type ToolResultPayload struct {
	ToolCallID string     `json:"tool_call_id"`
	Status     ToolStatus `json:"status"`
	Stdout     string     `json:"stdout"`
	Stderr     string     `json:"stderr"`
}

// SessionStatePayload is the data of a session_state event.
type SessionStatePayload struct {
	State string `json:"state"`
}

// Usage reports token accounting for a completed turn.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// DonePayload is the data of a done event.
type DonePayload struct {
	Usage Usage `json:"usage"`
}

// ErrorPayload is the data of a stream-level error event.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

// Frame is a single encoded SSE frame. ID is the monotonically increasing
// sequence number assigned by the session's EventRingBuffer; a zero ID marks
// an out-of-band frame that is not retained for replay.
type Frame struct {
	ID    uint64
	Event EventType
	Data  []byte
}

// NewFrame validates eventType and JSON-encodes payload into a frame.
func NewFrame(id uint64, eventType EventType, payload any) (Frame, error) {
	if !eventType.Valid() {
		return Frame{}, fmt.Errorf("sse: unknown event type %q", eventType)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Frame{}, fmt.Errorf("sse: encode %s payload: %w", eventType, err)
	}
	return Frame{ID: id, Event: eventType, Data: data}, nil
}

// Bytes returns the SSE wire encoding of the frame:
// "id: <seq>\nevent: <type>\ndata: <json>\n\n". The id line is omitted for
// out-of-band frames (ID == 0). encoding/json never emits raw newlines, so the
// payload always fits on a single data line.
func (f Frame) Bytes() []byte {
	buf := make([]byte, 0, len(f.Data)+len(f.Event)+40)
	if f.ID > 0 {
		buf = append(buf, "id: "...)
		buf = strconv.AppendUint(buf, f.ID, 10)
		buf = append(buf, '\n')
	}
	buf = append(buf, "event: "...)
	buf = append(buf, f.Event...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, f.Data...)
	buf = append(buf, "\n\n"...)
	return buf
}

// WriteTo writes the wire encoding of the frame to w.
func (f Frame) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(f.Bytes())
	return int64(n), err
}
