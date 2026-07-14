package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/store"
)

func TestCancelRunStopsActiveJob(t *testing.T) {
	s := &server{store: store.New(), runCancels: make(map[string]context.CancelFunc)}
	s.store.Create("run-1", "run_size", store.StatusRunning, 1, 114)
	ctx, cancel := context.WithCancel(context.Background())
	s.registerRunCancel("run-1", cancel)

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/runs/{id}", s.handleCancelRun)
	req := httptest.NewRequest(http.MethodDelete, "/v1/runs/run-1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("run context was not cancelled")
	}
	job, ok := s.store.Get("run-1")
	if !ok || job.Status != store.StatusFailed || job.Error != "run cancelled by client" {
		t.Fatalf("unexpected cancelled job: %+v", job)
	}
}

func TestCancelRunIsIdempotentAfterTerminalState(t *testing.T) {
	s := &server{store: store.New(), runCancels: make(map[string]context.CancelFunc)}
	s.store.Create("run-1", "run_size", store.StatusFailed, 1, 114)

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/runs/{id}", s.handleCancelRun)
	req := httptest.NewRequest(http.MethodDelete, "/v1/runs/run-1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}
