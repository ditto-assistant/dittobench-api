package store

import (
	"sync"
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func TestCreateFinishGet(t *testing.T) {
	s := New()
	if _, ok := s.Get("nope"); ok {
		t.Fatalf("expected miss for unknown id")
	}

	s.Create("r1", "direct", StatusRunning, 42, 10)
	got, ok := s.Get("r1")
	if !ok || got.Status != StatusRunning || got.Seed != 42 {
		t.Fatalf("expected running job seed 42, got ok=%v %+v", ok, got)
	}

	s.Finish("r1", protocol.ScoreReport{RunID: "r1", Composite: 0.5})
	got, _ = s.Get("r1")
	if got.Status != StatusDone {
		t.Fatalf("expected done, got %q", got.Status)
	}
	if got.Report == nil || got.Report.Composite != 0.5 {
		t.Fatalf("expected report composite 0.5, got %+v", got.Report)
	}
}

func TestFail(t *testing.T) {
	s := New()
	s.Create("r2", "sandbox", StatusBuilding, 1, 5)
	s.Fail("r2", "boom")
	got, _ := s.Get("r2")
	if got.Status != StatusFailed || got.Error != "boom" {
		t.Fatalf("expected failed/boom, got %q/%q", got.Status, got.Error)
	}
}

func TestFailWithSanitizedInfrastructureClassification(t *testing.T) {
	s := New()
	s.Create("r-infra", "sandbox", StatusRunning, 1, 5)
	failure := &Failure{
		Kind:      "validator_infrastructure",
		Code:      "sandbox_oom",
		Retryable: true,
		Diagnostics: map[string]any{
			"oom_killed":        true,
			"memory_peak_bytes": uint64(3 << 30),
		},
	}
	s.FailWith("r-infra", "validator sandbox resource envelope exhausted", failure)
	got, _ := s.Get("r-infra")
	if got.Failure == nil || got.Failure.Kind != "validator_infrastructure" || !got.Failure.Retryable {
		t.Fatalf("sanitized failure missing: %+v", got)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s := New()
	s.Create("r3", "direct", StatusQueued, 0, 0)
	got, _ := s.Get("r3")
	got.Status = StatusDone // mutate the copy
	again, _ := s.Get("r3")
	if again.Status != StatusQueued {
		t.Fatalf("Get must return a copy; stored job was mutated to %q", again.Status)
	}
}

func TestRunSizeProgressAndPartial(t *testing.T) {
	s := New()
	s.Create("r", "run_size", StatusQueued, 5, 12)
	s.SetRunSize("r", "small")
	s.SetStage("r", StatusGenerating, 0, 12)

	got, _ := s.Get("r")
	if got.RunSize != "small" {
		t.Fatalf("expected run_size small, got %q", got.RunSize)
	}
	if got.Status != StatusGenerating || got.Progress.Stage != string(StatusGenerating) || got.Progress.Total != 12 {
		t.Fatalf("stage not set: %+v", got)
	}

	s.AppendPartial("r", protocol.CaseScore{CaseID: "c1", Score: 1.0})
	s.AppendPartial("r", protocol.CaseScore{CaseID: "c2", Score: 0.0})
	got, _ = s.Get("r")
	if len(got.Partial) != 2 {
		t.Fatalf("expected 2 partials, got %d", len(got.Partial))
	}
	if got.Progress.Done != 2 {
		t.Fatalf("expected progress done 2, got %d", got.Progress.Done)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := New()
	s.Create("r", "direct", StatusRunning, 0, 0)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s.Update("r", func(j *Job) { j.N = n })
			s.Get("r")
		}(i)
	}
	wg.Wait()
}
