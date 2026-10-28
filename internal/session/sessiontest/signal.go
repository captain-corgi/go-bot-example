package sessiontest

import (
	"slices"
	"sync"
	"syscall"
)

// GroupSignal is one recorded process group signal.
type GroupSignal struct {
	PGID   int
	Signal syscall.Signal
}

// SignalRecorder is a session.GroupSignaler that records signals instead of
// delivering them to real processes.
type SignalRecorder struct {
	// OnSignal, if set, runs after each signal is recorded, e.g. to simulate
	// the process group halting the turn.
	OnSignal func(GroupSignal)

	mu      sync.Mutex
	signals []GroupSignal
}

// Signal records sig for pgid. It satisfies session.GroupSignaler.
func (r *SignalRecorder) Signal(pgid int, sig syscall.Signal) error {
	s := GroupSignal{PGID: pgid, Signal: sig}
	r.mu.Lock()
	r.signals = append(r.signals, s)
	r.mu.Unlock()
	if r.OnSignal != nil {
		r.OnSignal(s)
	}
	return nil
}

// Signals returns every recorded signal, oldest first.
func (r *SignalRecorder) Signals() []GroupSignal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.signals)
}
