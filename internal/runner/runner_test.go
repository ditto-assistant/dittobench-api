package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// TestMain relaxes the SSRF guard so tests can reach loopback httptest servers
// (the default client blocks private/loopback targets in production).
func TestMain(m *testing.M) {
	Configure(true)
	os.Exit(m.Run())
}

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

func TestSeed(t *testing.T) {
	var got protocol.SeedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/seed" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(protocol.SeedResponse{
			Pairs:    len(got.Pairs),
			Subjects: len(got.Subjects),
			Links:    len(got.Links),
		})
	}))
	defer srv.Close()

	req := protocol.SeedRequest{
		UserID:   "miner",
		Pairs:    []protocol.MemoryPair{{PairID: "p1", Prompt: "hi", Response: "yo"}},
		Subjects: []protocol.Subject{{ID: "s1", SubjectText: "x"}},
		Links:    []protocol.SubjectLink{{SubjectID: "s1", PairID: "p1"}},
	}
	resp, err := Seed(context.Background(), srv.URL, req)
	if err != nil {
		t.Fatalf("Seed error: %v", err)
	}
	if resp.Pairs != 1 || resp.Subjects != 1 || resp.Links != 1 {
		t.Fatalf("unexpected seed response: %+v", resp)
	}
	if got.UserID != "miner" || len(got.Pairs) != 1 {
		t.Fatalf("harness received wrong body: %+v", got)
	}
}

func TestSeedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := Seed(context.Background(), srv.URL, protocol.SeedRequest{}); err == nil {
		t.Fatal("expected error for 500 /seed")
	}
}

func TestRunCase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req protocol.RunRequest
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(protocol.RunResponse{FinalText: "answer:" + req.UserInput})
	}))
	defer srv.Close()
	resp, err := RunCase(context.Background(), srv.URL, "m1", "what color?", nil, CaseOptions{})
	if err != nil {
		t.Fatalf("RunCase error: %v", err)
	}
	if resp.FinalText != "answer:what color?" {
		t.Fatalf("unexpected: %q", resp.FinalText)
	}
}
