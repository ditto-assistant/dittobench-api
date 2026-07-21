package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/runner"
	"github.com/ditto-assistant/dittobench-api/internal/store"
	"github.com/ditto-assistant/dittobench-datagen/catalog"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func TestLockScoredRelayRunSerializesSharedRelayAccounting(t *testing.T) {
	t.Parallel()

	s := &server{}
	unlockFirst := s.lockScoredRelayRun()

	started := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		close(started)
		unlockSecond := s.lockScoredRelayRun()
		close(acquired)
		unlockSecond()
	}()
	<-started

	select {
	case <-acquired:
		t.Fatal("a second scored run acquired the shared relay lease concurrently")
	case <-time.After(50 * time.Millisecond):
	}

	unlockFirst()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("a waiting scored run did not acquire the relay lease after release")
	}
}

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

func TestHandleRelayPreflight(t *testing.T) {
	t.Run("reachable relay returns ok identity", func(t *testing.T) {
		relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/health" {
				t.Fatalf("path = %q", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":             "ok",
				"accounting_version": 2,
				"provider":           "openrouter",
				"profile_revision":   "profile-v1",
				"model":              "qwen/qwen3-32b",
			})
		}))
		defer relay.Close()
		t.Setenv("HARNESS_GATEWAY_URL", relay.URL)

		rec := httptest.NewRecorder()
		(&server{}).handleRelayPreflight(rec, httptest.NewRequest(http.MethodGet, "/v1/relay-preflight", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["status"] != "ok" || body["provider"] != "openrouter" || body["model"] != "qwen/qwen3-32b" {
			t.Fatalf("unexpected body: %#v", body)
		}
	})

	t.Run("unreachable relay fails closed as validator infrastructure", func(t *testing.T) {
		// A gateway that refuses the connection stands in for host.docker.internal
		// not resolving inside a mis-wired managed-stack netns — the exact failure
		// the off-netns validator preflight cannot otherwise see.
		t.Setenv("HARNESS_GATEWAY_URL", "http://127.0.0.1:1/")
		rec := httptest.NewRecorder()
		(&server{}).handleRelayPreflight(rec, httptest.NewRequest(http.MethodGet, "/v1/relay-preflight", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Status  string `json:"status"`
			Failure struct {
				Kind      string `json:"kind"`
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
			} `json:"failure"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Status != "unavailable" || body.Failure.Kind != "validator_infrastructure" ||
			body.Failure.Code != "model_relay_unavailable" || !body.Failure.Retryable {
			t.Fatalf("unexpected failure envelope: %s", rec.Body.String())
		}
	})
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

func TestRelayUsageSinceProducesTrustedRunDelta(t *testing.T) {
	start := relayHealthSnapshot{
		AccountingVersion: 2, Provider: "openrouter", ProfileRevision: "profile-v1",
		Model: "qwen/qwen3-32b", Requests: 10, Successes: 9, UsageAvailable: 9,
		PromptTokens: 1000, CompletionTokens: 200, ProviderLatencyMs: 3000,
		PromptBytes: 4000,
		TTFTStatus:  "unavailable_non_streaming",
	}
	end := start
	end.Requests += 4
	end.Successes += 4
	end.UsageAvailable += 4
	end.PromptTokens += 700
	end.PromptBytes += 2800
	end.CompletionTokens += 100
	end.ProviderLatencyMs += 900

	got, err := relayUsageSince(start, end)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "complete" || got.Source != "model_proxy_provider_response" ||
		got.PromptTokens != 700 || got.PromptBytes != 2800 || got.CompletionTokens != 100 || got.TotalTokens != 800 ||
		got.ProviderLatencyMs != 900 || got.UsageAvailable != 4 || got.UsageUnavailable != 0 {
		t.Fatalf("usage = %#v", got)
	}

	end.UsageUnavailable++
	got, err = relayUsageSince(start, end)
	if err != nil || got.Status != "unavailable" {
		t.Fatalf("missing usage must be explicit and neutral: usage=%#v err=%v", got, err)
	}

	end.ProfileRevision = "profile-v2"
	if _, err := relayUsageSince(start, end); err == nil {
		t.Fatal("provider profile change must invalidate attribution")
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

func TestV5RequiresTrustedTokenAccountingWhileLegacyAccountingRemainsReadable(t *testing.T) {
	legacy := relayHealthSnapshot{AccountingVersion: 1, Status: "ok"}
	if err := requireTokenAccounting(legacy); err == nil {
		t.Fatal("v5 accepted relay accounting v1")
	}
	metered := relayHealthSnapshot{
		AccountingVersion: 2, Status: "ok", Provider: "p",
		ProfileRevision: "r", Model: "m",
	}
	if err := requireTokenAccounting(metered); err != nil {
		t.Fatalf("v5 rejected trusted accounting: %v", err)
	}

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(legacy)
	}))
	defer relay.Close()
	if _, err := readRelayHealth(context.Background(), relay.URL); err != nil {
		t.Fatalf("v2-v4 rollout compatibility rejected accounting v1: %v", err)
	}
}

func TestRelayCountersCatchMaskedTwoHundredEmptyHarnessResponse(t *testing.T) {
	var failures uint64
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(relayHealthSnapshot{AccountingVersion: 2, Status: "ok", InfrastructureFailures: failures, Provider: "provider", ProfileRevision: "revision", Model: "model", TTFTStatus: "unavailable_non_streaming"})
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
