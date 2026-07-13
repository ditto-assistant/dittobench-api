// Package store is a tiny thread-safe in-memory store for evaluation jobs,
// keyed by run ID. It backs GET /v1/runs/{id}. Jobs are lost on restart — this
// is a practice service, not the on-chain validator.
//
// A Job carries a lifecycle status so the sandbox path (which builds + runs a
// submitted harness in Docker, taking minutes) can be polled, mirroring the
// SN118 upload→poll flow. The direct harness_url path completes synchronously
// and lands a job already in StatusDone.
package store

import (
	"sync"
	"time"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// Status is a job lifecycle state.
type Status string

const (
	StatusQueued     Status = "queued"     // accepted, not yet started
	StatusBuilding   Status = "building"   // docker build in progress (sandbox mode)
	StatusGenerating Status = "generating" // generating the fresh anti-cheat dataset
	StatusSeeding    Status = "seeding"    // pushing the fresh haystack to the harness
	StatusRunning    Status = "running"    // harness up, dataset executing
	StatusScoring    Status = "scoring"    // computing the score report
	StatusDone       Status = "done"       // Report is populated
	StatusFailed     Status = "failed"     // Error is populated
)

// Progress is the coarse stage progress surfaced to a polling UI.
type Progress struct {
	Stage string `json:"stage"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// Job is one evaluation, in any lifecycle state.
type Job struct {
	RunID     string                `json:"run_id"`
	Status    Status                `json:"status"`
	Mode      string                `json:"mode"`               // "direct" | "sandbox"
	RunSize   string                `json:"run_size,omitempty"` // "small" | "medium" | "full"
	Seed      int64                 `json:"seed,omitempty"`     // dataset seed used
	N         int                   `json:"n,omitempty"`        // case count requested
	Error     string                `json:"error,omitempty"`    // set when Status == failed
	Progress  Progress              `json:"progress"`           // stage/done/total for the UI
	Partial   []protocol.CaseScore  `json:"partial,omitempty"`  // case scores appended as they complete
	Report    *protocol.ScoreReport `json:"report,omitempty"`   // set when Status == done
	CreatedAt time.Time             `json:"created_at"`
	UpdatedAt time.Time             `json:"updated_at"`
}

// Store holds jobs by run ID.
type Store struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

// New returns an empty store.
func New() *Store {
	return &Store{jobs: make(map[string]*Job)}
}

// Create inserts a new job in the given mode/status and returns a copy.
func (s *Store) Create(runID, mode string, status Status, seed int64, n int) *Job {
	now := time.Now()
	j := &Job{
		RunID:     runID,
		Status:    status,
		Mode:      mode,
		Seed:      seed,
		N:         n,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.mu.Lock()
	s.jobs[runID] = j
	s.mu.Unlock()
	cp := *j
	return &cp
}

// Update applies mutate to the stored job under lock and bumps UpdatedAt.
func (s *Store) Update(runID string, mutate func(*Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[runID]; ok {
		mutate(j)
		j.UpdatedAt = time.Now()
	}
}

// SetStatus is a convenience for a status-only transition.
func (s *Store) SetStatus(runID string, status Status) {
	s.Update(runID, func(j *Job) { j.Status = status })
}

// SetRunSize records the requested run_size on a job.
func (s *Store) SetRunSize(runID, runSize string) {
	s.Update(runID, func(j *Job) { j.RunSize = runSize })
}

// SetStage sets Status + the progress stage label together.
func (s *Store) SetStage(runID string, status Status, done, total int) {
	s.Update(runID, func(j *Job) {
		j.Status = status
		j.Progress = Progress{Stage: string(status), Done: done, Total: total}
	})
}

// AppendPartial adds a finished case score and advances Progress.Done.
func (s *Store) AppendPartial(runID string, cs protocol.CaseScore) {
	s.Update(runID, func(j *Job) {
		j.Partial = append(j.Partial, cs)
		j.Progress.Done = len(j.Partial)
	})
}

// Fail marks a job failed with the given error message.
func (s *Store) Fail(runID, msg string) {
	s.Update(runID, func(j *Job) {
		j.Status = StatusFailed
		j.Error = msg
	})
}

// Finish marks a job done and attaches its report.
func (s *Store) Finish(runID string, report protocol.ScoreReport) {
	s.Update(runID, func(j *Job) {
		j.Status = StatusDone
		r := report
		j.Report = &r
	})
}

// Get returns a copy of the job for id and whether it was found.
func (s *Store) Get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}
