package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

func TestHealthOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := Health(context.Background(), srv.URL); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}
}

func TestHealthBad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := Health(context.Background(), srv.URL); err == nil {
		t.Fatalf("expected error for 503 health")
	}
}

func TestRunHarnessEchoesTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/run" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req protocol.RunRequest
		json.NewDecoder(r.Body).Decode(&req)
		resp := protocol.RunResponse{
			FinalText: "ok",
			ToolCalls: []protocol.ObservedToolCall{{Name: "search_web"}},
			LatencyMs: 5,
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ds := protocol.Dataset{ToolCases: []protocol.ToolCase{
		{ID: "c1", Category: "web_search", Prompt: "search foo"},
	}}
	out, err := RunHarness(context.Background(), srv.URL, ds, nil)
	if err != nil {
		t.Fatalf("RunHarness error: %v", err)
	}
	got, ok := out["c1"]
	if !ok {
		t.Fatalf("missing case c1 in output")
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "search_web" {
		t.Fatalf("unexpected response: %+v", got)
	}
}

func TestRunHarnessPerCaseFailureIsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ds := protocol.Dataset{ToolCases: []protocol.ToolCase{{ID: "c1", Prompt: "x"}}}
	out, err := RunHarness(context.Background(), srv.URL, ds, nil)
	if err != nil {
		t.Fatalf("RunHarness should not abort on per-case failure: %v", err)
	}
	if got, ok := out["c1"]; !ok || len(got.ToolCalls) != 0 {
		t.Fatalf("failed case should be present and empty, got ok=%v %+v", ok, got)
	}
}
