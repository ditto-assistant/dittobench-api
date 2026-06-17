// Package store is a tiny thread-safe in-memory store for ScoreReports, keyed
// by run ID. It backs GET /v1/runs/{id}. Reports are lost on restart — this is
// a practice service, not the on-chain validator.
package store

import (
	"sync"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// Store holds completed score reports.
type Store struct {
	mu      sync.RWMutex
	reports map[string]protocol.ScoreReport
}

// New returns an empty store.
func New() *Store {
	return &Store{reports: make(map[string]protocol.ScoreReport)}
}

// Put stores (or overwrites) a report by its RunID.
func (s *Store) Put(r protocol.ScoreReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports[r.RunID] = r
}

// Get returns the report for id and whether it was found.
func (s *Store) Get(id string) (protocol.ScoreReport, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.reports[id]
	return r, ok
}
