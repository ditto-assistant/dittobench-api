package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/sandbox"
	"github.com/ditto-assistant/dittobench-api/internal/store"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

type diagnosticSandbox struct {
	diagnostics sandbox.RuntimeDiagnostics
	stopped     bool
}

func (*diagnosticSandbox) Available(context.Context) error { return nil }
func (*diagnosticSandbox) Build(context.Context, sandbox.Source) (string, string, *protocol.CodeFingerprint, error) {
	return "", "", nil, nil
}
func (*diagnosticSandbox) Run(context.Context, string, map[string]string) (*sandbox.Handle, error) {
	return nil, nil
}
func (*diagnosticSandbox) Release(context.Context, string) {}
func (s *diagnosticSandbox) Stop(context.Context, *sandbox.Handle) {
	s.stopped = true
}
func (s *diagnosticSandbox) Diagnostics(context.Context, *sandbox.Handle) sandbox.RuntimeDiagnostics {
	return s.diagnostics
}

func TestFullRunConcurrencyIsOnePerScorer(t *testing.T) {
	s := &server{
		store:    store.New(),
		runSlots: make(chan struct{}, maxConcurrentRuns),
	}
	if cap(s.runSlots) != 1 {
		t.Fatalf("full miner sandbox concurrency = %d, want 1", cap(s.runSlots))
	}
	s.runSlots <- struct{}{}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/submit", nil)
	s.submitRunSize(recorder, request, submitRequest{
		HarnessURL: "http://127.0.0.1:8080",
		RunSize:    "full",
	})
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "validator at capacity") {
		t.Fatalf("capacity response missing retry guidance: %s", recorder.Body.String())
	}
}

func TestFinishSandboxRunClassifiesOOMBeforeStop(t *testing.T) {
	backend := &diagnosticSandbox{diagnostics: sandbox.RuntimeDiagnostics{
		OOMKilled:       true,
		ExitCode:        137,
		MemoryEvents:    map[string]uint64{"oom": 1, "oom_kill": 1},
		MemoryPeakBytes: pointer(uint64(3 << 30)),
	}}
	s := &server{store: store.New(), sandbox: backend}
	s.store.Create("run-oom", "sandbox", store.StatusRunning, 1, 114)
	s.store.Fail("run-oom", "private miner output and container id")

	s.finishSandboxRun("run-oom", &sandbox.Handle{ContainerID: "private"})
	job, _ := s.store.Get("run-oom")
	if !backend.stopped {
		t.Fatal("sandbox must be stopped after diagnostics")
	}
	if job.Error != "validator sandbox resource envelope exhausted" {
		t.Fatalf("private error was not replaced: %q", job.Error)
	}
	if job.Failure == nil || job.Failure.Code != "sandbox_oom" || !job.Failure.Retryable {
		t.Fatalf("infrastructure classifier missing: %+v", job.Failure)
	}
}

func pointer[T any](value T) *T { return &value }
