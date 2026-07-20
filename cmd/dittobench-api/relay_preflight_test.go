package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/runner"
	"github.com/ditto-assistant/dittobench-api/internal/store"
	"github.com/ditto-assistant/dittobench-datagen/catalog"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func TestProbeLockedModelRelay(t *testing.T) {
	t.Run("healthy tiny completion", func(t *testing.T) {
		var got map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				t.Fatalf("path = %q", r.URL.Path)
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "OK"}}}})
		}))
		defer srv.Close()

		if err := probeLockedModelRelay(context.Background(), srv.URL); err != nil {
			t.Fatalf("probe failed: %v", err)
		}
		if got["max_tokens"] != float64(1) || got["stream"] != false {
			t.Fatalf("probe was not bounded: %#v", got)
		}
	})

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "upstream failure", status: http.StatusBadGateway, body: `upstream unavailable`},
		{name: "malformed success", status: http.StatusOK, body: `{"choices":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			err := probeLockedModelRelay(context.Background(), srv.URL)
			if err == nil {
				t.Fatal("expected relay probe failure")
			}
			if strings.Contains(err.Error(), tc.body) {
				t.Fatalf("probe exposed upstream response: %v", err)
			}
		})
	}
}

func TestRelayFailureIsRetryableValidatorInfrastructure(t *testing.T) {
	s := &server{store: store.New()}
	s.store.Create("run", "run_size", store.StatusRunning, 1, 1)
	s.failRelayUnavailable("run", errors.New("relay returned 503"))
	job, _ := s.store.Get("run")
	if job.Status != store.StatusFailed || job.Failure == nil {
		t.Fatalf("job = %#v", job)
	}
	if job.Failure.Kind != "validator_infrastructure" || job.Failure.Code != "model_relay_unavailable" || !job.Failure.Retryable {
		t.Fatalf("failure = %#v", job.Failure)
	}
}

func TestRelayDegradedSince(t *testing.T) {
	start := relayHealthSnapshot{Requests: 10, Successes: 9, InfrastructureFailures: 1}
	if err := relayDegradedSince(start, relayHealthSnapshot{Requests: 20, Successes: 19, InfrastructureFailures: 1}); err != nil {
		t.Fatalf("healthy run was rejected: %v", err)
	}
	if err := relayDegradedSince(start, relayHealthSnapshot{Requests: 20, Successes: 18, InfrastructureFailures: 2}); err == nil {
		t.Fatal("provider failure during run must reject the attempt")
	}
	if err := relayDegradedSince(start, relayHealthSnapshot{}); err == nil {
		t.Fatal("relay restart during run must reject the attempt")
	}
}

func TestReadRelayHealthRejectsStaticLegacyHealth(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer relay.Close()
	if _, err := readRelayHealth(context.Background(), relay.URL); err == nil || !strings.Contains(err.Error(), "run accounting") {
		t.Fatalf("legacy static health must fail closed, got %v", err)
	}
}

func TestRelayCountersCatchMaskedTwoHundredEmptyHarnessResponse(t *testing.T) {
	var failures uint64
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(relayHealthSnapshot{AccountingVersion: 1, Status: "ok", InfrastructureFailures: failures})
		case "/v1/chat/completions":
			failures++
			http.Error(w, "provider unavailable", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer relay.Close()

	harness := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/run" {
			http.NotFound(w, r)
			return
		}
		// Reproduce the production incident exactly: the harness observes a relay
		// failure but masks it behind a valid HTTP 200 empty RunResponse.
		_, _ = http.Post(relay.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
		_ = json.NewEncoder(w).Encode(protocol.RunResponse{})
	}))
	defer harness.Close()

	start, err := readRelayHealth(context.Background(), relay.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := runner.TrustSandbox(context.Background())
	resp, runErr := runner.RunCase(ctx, harness.URL, "case", "prompt", catalog.Catalog(), runner.CaseOptions{})
	if runErr != nil {
		t.Fatalf("incident shape must look delivered to the runner: %v", runErr)
	}
	if resp.FinalText != "" || len(resp.ToolCalls) != 0 {
		t.Fatalf("expected empty delivered response, got %#v", resp)
	}
	end, err := readRelayHealth(context.Background(), relay.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := relayDegradedSince(start, end); err == nil {
		t.Fatal("masked 200-empty response must not survive relay health validation")
	}
}
