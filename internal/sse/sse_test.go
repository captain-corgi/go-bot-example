package sse

import (
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFrameWireFormatAllEventTypes(t *testing.T) {
	tests := []struct {
		event   EventType
		payload any
		data    string
	}{
		{EventAssistantChunk, AssistantChunkPayload{Delta: "Hel"}, `{"delta":"Hel"}`},
		{EventToolCall, ToolCallPayload{ToolCallID: "t1", Name: "ls", Arguments: map[string]any{"path": "."}},
			`{"tool_call_id":"t1","name":"ls","arguments":{"path":"."}}`},
		{EventToolResult, ToolResultPayload{ToolCallID: "t1", Status: ToolStatusSuccess, Stdout: "a\nb", Stderr: ""},
			`{"tool_call_id":"t1","status":"success","stdout":"a\nb","stderr":""}`},
		{EventSessionState, SessionStatePayload{State: "running"}, `{"state":"running"}`},
		{EventDone, DonePayload{Usage: Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7}},
			`{"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`},
		{EventError, ErrorPayload{Code: "boom", Message: "bad", Fatal: true}, `{"code":"boom","message":"bad","fatal":true}`},
	}
	buf := NewEventRingBuffer(16)
	for i, tc := range tests {
		t.Run(string(tc.event), func(t *testing.T) {
			f, err := buf.Append(tc.event, tc.payload)
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			wantID := uint64(i + 1)
			if f.ID != wantID {
				t.Fatalf("ID = %d, want %d", f.ID, wantID)
			}
			want := "id: " + strconv.FormatUint(wantID, 10) + "\nevent: " + string(tc.event) + "\ndata: " + tc.data + "\n\n"
			if got := string(f.Bytes()); got != want {
				t.Errorf("wire =\n%q\nwant\n%q", got, want)
			}
			if !json.Valid(f.Data) {
				t.Errorf("data is not valid JSON: %s", f.Data)
			}
		})
	}
}

func TestFrameOutOfBandOmitsID(t *testing.T) {
	f, err := NewFrame(0, EventError, ErrorPayload{Code: "c", Message: "m"})
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	want := "event: error\ndata: {\"code\":\"c\",\"message\":\"m\",\"fatal\":false}\n\n"
	if got := string(f.Bytes()); got != want {
		t.Errorf("wire = %q, want %q", got, want)
	}
}

func TestAppendRejectsInvalidInput(t *testing.T) {
	buf := NewEventRingBuffer(4)
	if _, err := buf.Append("bogus", struct{}{}); err == nil {
		t.Error("expected error for unknown event type")
	}
	if _, err := buf.Append(EventDone, make(chan int)); err == nil {
		t.Error("expected error for unencodable payload")
	}
	if buf.LastSeq() != 0 {
		t.Errorf("LastSeq = %d after rejected appends, want 0", buf.LastSeq())
	}
}

func appendN(t *testing.T, buf *EventRingBuffer, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := buf.Append(EventAssistantChunk, AssistantChunkPayload{Delta: strconv.Itoa(i)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

func ids(frames []Frame) []uint64 {
	out := make([]uint64, len(frames))
	for i, f := range frames {
		out[i] = f.ID
	}
	return out
}

func assertIDs(t *testing.T, got []Frame, want ...uint64) {
	t.Helper()
	g := ids(got)
	if len(g) != len(want) {
		t.Fatalf("ids = %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("ids = %v, want %v", g, want)
		}
	}
}

func TestReplayAfterOrderingAndWindow(t *testing.T) {
	buf := NewEventRingBuffer(4)
	if frames, ok := buf.ReplayAfter(0); len(frames) != 0 || !ok {
		t.Fatalf("empty ReplayAfter(0) = %v, %v", frames, ok)
	}
	appendN(t, buf, 3)

	frames, ok := buf.ReplayAfter(1)
	if !ok {
		t.Error("ReplayAfter(1) not in window")
	}
	assertIDs(t, frames, 2, 3)

	frames, ok = buf.ReplayAfter(3)
	if !ok || len(frames) != 0 {
		t.Errorf("ReplayAfter(latest) = %v, %v", ids(frames), ok)
	}
	if _, ok := buf.ReplayAfter(4); ok {
		t.Error("ReplayAfter(future) reported in window")
	}
}

func TestRingBufferWraparound(t *testing.T) {
	buf := NewEventRingBuffer(3)
	appendN(t, buf, 5) // retains 3, 4, 5

	frames, ok := buf.ReplayAfter(0)
	if ok {
		t.Error("ReplayAfter(0) after eviction reported in window")
	}
	assertIDs(t, frames, 3, 4, 5)

	frames, ok = buf.ReplayAfter(2)
	if !ok {
		t.Error("ReplayAfter(2) should be in window (next is oldest retained)")
	}
	assertIDs(t, frames, 3, 4, 5)

	frames, ok = buf.ReplayAfter(4)
	if !ok {
		t.Error("ReplayAfter(4) not in window")
	}
	assertIDs(t, frames, 5)
	if buf.LastSeq() != 5 {
		t.Errorf("LastSeq = %d, want 5", buf.LastSeq())
	}
}

func TestDefaultCapacity(t *testing.T) {
	if got := len(NewEventRingBuffer(0).frames); got != DefaultCapacity {
		t.Errorf("capacity = %d, want %d", got, DefaultCapacity)
	}
}

func TestWaitWakesOnAppend(t *testing.T) {
	buf := NewEventRingBuffer(4)
	wait := buf.Wait()
	select {
	case <-wait:
		t.Fatal("wait closed before append")
	default:
	}
	appendN(t, buf, 1)
	select {
	case <-wait:
	case <-time.After(time.Second):
		t.Fatal("wait not closed after append")
	}
}

func TestDisconnectGraceExpires(t *testing.T) {
	buf := NewEventRingBuffer(4)
	fired := make(chan struct{})
	buf.StartDisconnectGrace(10*time.Millisecond, func() { close(fired) })
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("grace callback did not fire")
	}
	if buf.CancelDisconnectGrace() {
		t.Error("CancelDisconnectGrace after expiry reported a pending timer")
	}
}

func TestDisconnectGraceCancelled(t *testing.T) {
	buf := NewEventRingBuffer(4)
	var fired atomic.Bool
	buf.StartDisconnectGrace(30*time.Millisecond, func() { fired.Store(true) })
	if !buf.CancelDisconnectGrace() {
		t.Fatal("CancelDisconnectGrace reported no pending timer")
	}
	time.Sleep(60 * time.Millisecond)
	if fired.Load() {
		t.Error("cancelled grace callback fired")
	}
	if buf.CancelDisconnectGrace() {
		t.Error("second CancelDisconnectGrace reported a pending timer")
	}
}

func TestDisconnectGraceRearmReplaces(t *testing.T) {
	buf := NewEventRingBuffer(4)
	var first atomic.Bool
	second := make(chan struct{})
	buf.StartDisconnectGrace(20*time.Millisecond, func() { first.Store(true) })
	buf.StartDisconnectGrace(40*time.Millisecond, func() { close(second) })
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement grace callback did not fire")
	}
	if first.Load() {
		t.Error("replaced grace callback fired")
	}
}

func TestConcurrentOperationsRaceFree(t *testing.T) {
	buf := NewEventRingBuffer(8)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_, _ = buf.Append(EventAssistantChunk, AssistantChunkPayload{Delta: "x"})
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				frames, _ := buf.ReplayAfter(buf.LastSeq() / 2)
				for j := 1; j < len(frames); j++ {
					if frames[j].ID <= frames[j-1].ID {
						t.Errorf("non-monotonic replay: %v", ids(frames))
						return
					}
				}
				<-buf.Wait()
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				buf.StartDisconnectGrace(time.Microsecond, func() {})
				buf.CancelDisconnectGrace()
			}
		}()
	}
	// Keep appending until all readers blocked on Wait are released.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-done:
			if buf.LastSeq() < 400 {
				t.Errorf("LastSeq = %d, want >= 400", buf.LastSeq())
			}
			return
		default:
			_, _ = buf.Append(EventAssistantChunk, AssistantChunkPayload{Delta: "tick"})
			time.Sleep(time.Millisecond)
		}
	}
}

func TestPurgeDropsFramesAndKeepsSequence(t *testing.T) {
	buf := NewEventRingBuffer(4)
	for i := 0; i < 3; i++ {
		if _, err := buf.Append(EventAssistantChunk, AssistantChunkPayload{Delta: strconv.Itoa(i)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	var fired atomic.Bool
	buf.StartDisconnectGrace(10*time.Millisecond, func() { fired.Store(true) })
	wait := buf.Wait()

	buf.Purge()

	select {
	case <-wait:
	default:
		t.Fatal("Purge did not wake waiters")
	}
	if buf.Len() != 0 {
		t.Fatalf("Len after Purge = %d, want 0", buf.Len())
	}
	if frames, ok := buf.ReplayAfter(0); len(frames) != 0 || !ok {
		t.Fatalf("ReplayAfter(0) = %d frames, inWindow %v", len(frames), ok)
	}
	if buf.LastSeq() != 3 {
		t.Fatalf("LastSeq after Purge = %d, want 3", buf.LastSeq())
	}
	f, err := buf.Append(EventDone, DonePayload{})
	if err != nil || f.ID != 4 {
		t.Fatalf("Append after Purge = id %d, err %v; want id 4", f.ID, err)
	}
	time.Sleep(30 * time.Millisecond)
	if fired.Load() {
		t.Fatal("grace timer fired after Purge")
	}
}
