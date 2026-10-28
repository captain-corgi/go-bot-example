package sse

import (
	"sync"
	"time"
)

// DefaultCapacity is the default number of frames retained per session.
const DefaultCapacity = 1024

// DefaultDisconnectGrace is how long an in-flight turn keeps running after its
// last SSE client disconnects before it is cancelled.
const DefaultDisconnectGrace = 60 * time.Second

// EventRingBuffer is a thread-safe, fixed-capacity circular buffer of SSE
// frames for a single session. It assigns strictly increasing sequence IDs
// starting at 1, overwrites the oldest frame only once capacity is exceeded,
// and owns the session's disconnect grace timer. Frames live in memory only.
type EventRingBuffer struct {
	mu      sync.Mutex
	frames  []Frame
	start   int // index of the oldest retained frame
	count   int
	lastSeq uint64
	changed chan struct{}

	graceTimer *time.Timer
	graceGen   uint64
}

// NewEventRingBuffer returns a buffer retaining up to capacity frames.
// A non-positive capacity selects DefaultCapacity.
func NewEventRingBuffer(capacity int) *EventRingBuffer {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &EventRingBuffer{
		frames:  make([]Frame, capacity),
		changed: make(chan struct{}),
	}
}

// Append encodes payload as a frame of eventType, assigns it the next
// sequence ID, retains it, and wakes every waiter returned by Wait.
func (b *EventRingBuffer) Append(eventType EventType, payload any) (Frame, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	frame, err := NewFrame(b.lastSeq+1, eventType, payload)
	if err != nil {
		return Frame{}, err
	}
	b.lastSeq = frame.ID

	capacity := len(b.frames)
	if b.count < capacity {
		b.frames[(b.start+b.count)%capacity] = frame
		b.count++
	} else {
		b.frames[b.start] = frame
		b.start = (b.start + 1) % capacity
	}

	close(b.changed)
	b.changed = make(chan struct{})
	return frame, nil
}

// ReplayAfter returns every retained frame with an ID strictly greater than
// lastSeqID in ascending order. The boolean reports whether lastSeqID is still
// within the retained window, i.e. whether the returned frames continue the
// stream from lastSeqID without a gap. It is false when frames after lastSeqID
// were already evicted or when lastSeqID is ahead of the latest sequence ID.
func (b *EventRingBuffer) ReplayAfter(lastSeqID uint64) ([]Frame, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if lastSeqID > b.lastSeq {
		return nil, false
	}
	if b.count == 0 {
		return nil, true
	}
	oldest := b.frames[b.start].ID
	inWindow := lastSeqID+1 >= oldest

	capacity := len(b.frames)
	out := make([]Frame, 0, b.count)
	for i := 0; i < b.count; i++ {
		f := b.frames[(b.start+i)%capacity]
		if f.ID > lastSeqID {
			out = append(out, f)
		}
	}
	return out, inWindow
}

// LastSeq returns the most recently assigned sequence ID (0 when empty).
func (b *EventRingBuffer) LastSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastSeq
}

// Wait returns a channel that is closed by the next Append. Obtain it before
// calling ReplayAfter so no frame appended in between is missed.
func (b *EventRingBuffer) Wait() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

// Purge drops every retained frame, cancels any pending grace timer, and
// wakes every waiter. Sequence numbering continues from LastSeq so IDs are
// never reused. It is used when the owning session is torn down.
func (b *EventRingBuffer) Purge() {
	b.mu.Lock()
	defer b.mu.Unlock()

	clear(b.frames)
	b.start, b.count = 0, 0
	b.stopGraceLocked()
	close(b.changed)
	b.changed = make(chan struct{})
}

// Len returns the number of retained frames.
func (b *EventRingBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

// StartDisconnectGrace arms the disconnect grace timer: onExpire runs after
// ttl unless CancelDisconnectGrace is called first. Arming replaces any
// previously pending timer. onExpire runs without the buffer lock held.
func (b *EventRingBuffer) StartDisconnectGrace(ttl time.Duration, onExpire func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.stopGraceLocked()
	gen := b.graceGen
	b.graceTimer = time.AfterFunc(ttl, func() {
		b.mu.Lock()
		if b.graceGen != gen || b.graceTimer == nil {
			b.mu.Unlock()
			return
		}
		b.graceTimer = nil
		b.mu.Unlock()
		onExpire()
	})
}

// CancelDisconnectGrace stops a pending grace timer. It reports whether a
// timer was pending and is now guaranteed never to invoke its callback.
func (b *EventRingBuffer) CancelDisconnectGrace() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopGraceLocked()
}

func (b *EventRingBuffer) stopGraceLocked() bool {
	// Bumping the generation invalidates a callback that already fired but has
	// not yet claimed the timer.
	b.graceGen++
	if b.graceTimer == nil {
		return false
	}
	b.graceTimer.Stop()
	b.graceTimer = nil
	return true
}
