package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/refharness"
	"github.com/ditto-assistant/dittobench-api/internal/runner"
	"github.com/ditto-assistant/dittobench-api/internal/scorer"
	"github.com/ditto-assistant/dittobench-api/internal/store"
	"github.com/ditto-assistant/dittobench-datagen/catalog"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
	"github.com/ditto-assistant/dittobench-datagen/toolexec"
)

// preflightHarness is a minimal /run harness. When comply is true it executes
// one search_web call through the advertised tool_endpoint (the protocol's
// preflight requirement); when false it answers without calling any tool —
// indistinguishable, from validator state alone, from an unreachable endpoint.
func preflightHarness(t *testing.T, comply bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req protocol.RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if comply && req.ToolEndpoint != "" && refharness.IsPreflight(req.CaseID) {
			if _, err := refharness.Execute(r.Context(), req.ToolEndpoint, req.CaseID, req.UserID, refharness.PreflightCalls()); err != nil {
				t.Logf("harness tool exec: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.RunResponse{FinalText: "ok"})
	}))
}

// startObservedEndpoint mounts a toolexec server the same way the pipeline does
// and returns its URL.
func startObservedEndpoint(t *testing.T, toolSrv *toolexec.Server) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("POST /tool", toolSrv)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/tool"
}

func TestProbeToolReachabilityObserved(t *testing.T) {
	harness := preflightHarness(t, true)
	defer harness.Close()
	toolSrv := toolexec.NewServer()
	endpoint := startObservedEndpoint(t, toolSrv)

	ctx := runner.TrustSandbox(context.Background())
	err := probeToolReachability(ctx, harness.URL, endpoint, toolSrv, 42, catalog.Catalog(), "run-preflight-ok")
	if err != nil {
		t.Fatalf("probe against compliant harness: %v", err)
	}
}

func TestEnforcePreflightFailsScoredRun(t *testing.T) {
	harness := preflightHarness(t, false)
	defer harness.Close()
	toolSrv := toolexec.NewServer()
	endpoint := startObservedEndpoint(t, toolSrv)

	s := &server{store: store.New()}
	runID := "run-preflight-scored"
	s.store.Create(runID, "run_size", store.StatusRunning, 42, 0)

	ctx := runner.TrustSandbox(context.Background())
	ok := s.enforcePreflight(ctx, runID, scorer.ScopeScored, harness.URL, endpoint, toolSrv, 42, catalog.Catalog())
	if ok {
		t.Fatal("enforcePreflight allowed a scored run whose probe was never observed")
	}
	run, found := s.store.Get(runID)
	if !found {
		t.Fatal("run missing from store")
	}
	if run.Status != store.StatusFailed {
		t.Fatalf("scored run status = %q, want %q (failed run, not a completed zero report)", run.Status, store.StatusFailed)
	}
	if run.Report != nil {
		t.Fatal("failed scored run must not carry a score report")
	}
	if !strings.Contains(run.Error, "unreachable") {
		t.Fatalf("failure reason should name the unreachable endpoint, got %q", run.Error)
	}
}

func TestEnforcePreflightPracticeContinues(t *testing.T) {
	harness := preflightHarness(t, false)
	defer harness.Close()
	toolSrv := toolexec.NewServer()
	endpoint := startObservedEndpoint(t, toolSrv)

	s := &server{store: store.New()}
	runID := "run-preflight-practice"
	s.store.Create(runID, "run_size", store.StatusRunning, 42, 0)

	ctx := runner.TrustSandbox(context.Background())
	ok := s.enforcePreflight(ctx, runID, scorer.ScopePractice, harness.URL, endpoint, toolSrv, 42, catalog.Catalog())
	if !ok {
		t.Fatal("practice run must continue (capped) when the probe is unobserved")
	}
	run, found := s.store.Get(runID)
	if !found {
		t.Fatal("run missing from store")
	}
	if run.Status == store.StatusFailed {
		t.Fatal("practice run must not be failed by the preflight")
	}
}
