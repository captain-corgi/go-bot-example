package session

import (
	"maps"
	"slices"
	"sync"
	"time"
)

// Record is the persisted state of a session used for resumption.
type Record struct {
	ID            string
	InitialPrompt string
	Options       map[string]any
	History       []Message
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (r Record) clone() Record {
	r.Options = maps.Clone(r.Options)
	r.History = slices.Clone(r.History)
	return r
}

// Store persists session records so sessions can be resumed by ID.
type Store interface {
	Save(rec Record) error
	Load(id string) (Record, bool, error)
}

// MemoryStore is an in-memory Store. Records are copied on the way in and out
// so callers never share mutable state with the store.
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]Record
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]Record)}
}

// Save stores or replaces rec.
func (s *MemoryStore) Save(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[rec.ID] = rec.clone()
	return nil
}

// Load returns the record for id, if any.
func (s *MemoryStore) Load(id string) (Record, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[id]
	if !ok {
		return Record{}, false, nil
	}
	return rec.clone(), true, nil
}
