package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
)

func TestLegacyBrokerSessionsRunConcurrentlyWithIsolatedAccounting(t *testing.T) {
	var inFlight atomic.Int64
	var peak atomic.Int64
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("legacy broker must not send a provider bearer to model-relay")
		}
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4},"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer upstream.Close()

	broker := newInferenceBroker(2)
	ids := make([]string, 2)
	for i, ip := range []string{"192.0.2.40", "192.0.2.41"} {
		id, err := broker.prepareLegacy(upstream.URL, relayHealthSnapshot{
			Provider: "openrouter", ProfileRevision: llm.OpenRouterRelayProfileRevision,
			Model: llm.LockedHarnessModel,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		if !broker.bindSource(id, ip) {
			t.Fatalf("failed to bind legacy session %d", i)
		}
	}

	var wg sync.WaitGroup
	recorders := make([]*httptest.ResponseRecorder, 2)
	for i, ip := range []string{"192.0.2.40", "192.0.2.41"} {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			// Match the frozen starter-kit adapter exactly: CHUTES_BASE_URL is
			// the broker base and the client appends /chat/completions.
			request := httptest.NewRequest(http.MethodPost, "/v1/inference/chat/completions", bytes.NewBufferString(`{"model":"qwen/qwen3-32b"}`))
			request.RemoteAddr = ip + ":4321"
			request.SetPathValue("rest", "chat/completions")
			recorders[i] = httptest.NewRecorder()
			broker.handle(recorders[i], request)
		}(i, ip)
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("legacy requests did not overlap")
		}
	}
	close(release)
	wg.Wait()

	if peak.Load() != 2 {
		t.Fatalf("peak legacy relay concurrency = %d, want 2", peak.Load())
	}
	for i, id := range ids {
		if recorders[i].Code != http.StatusOK {
			t.Fatalf("legacy response %d = %d: %s", i, recorders[i].Code, recorders[i].Body.String())
		}
		snapshot, err := broker.snapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Provider != "openrouter" || snapshot.ProfileRevision != llm.OpenRouterRelayProfileRevision ||
			snapshot.Model != llm.LockedHarnessModel || snapshot.Requests != 1 || snapshot.Successes != 1 ||
			snapshot.UsageAvailable != 1 {
			t.Fatalf("legacy session %d accounting = %+v", i, snapshot)
		}
		if snapshot.PromptTokens != 3 || snapshot.CompletionTokens != 4 {
			t.Fatalf("legacy session %d token accounting = %+v", i, snapshot)
		}
	}
}

func TestLegacyBrokerRejectsUnreviewedRelayIdentity(t *testing.T) {
	broker := newInferenceBroker(1)
	if _, err := broker.prepareLegacy("http://127.0.0.1:11434", relayHealthSnapshot{
		Provider: "openrouter", ProfileRevision: "mutable-route", Model: llm.LockedHarnessModel,
	}); err == nil {
		t.Fatal("unreviewed legacy relay identity was accepted")
	}
}

func prepareBrokerSession(t *testing.T, broker *inferenceBroker) map[string]string {
	t.Helper()
	recorder := httptest.NewRecorder()
	broker.prepare(recorder, httptest.NewRequest(http.MethodPost, "/v1/inference/session", nil))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("prepare status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var prepared map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	return prepared
}

func activateBrokerSession(t *testing.T, broker *inferenceBroker, prepared map[string]string, proxyURL string) {
	t.Helper()
	body, _ := json.Marshal(brokerActivation{
		ActivationSecret: prepared["activation_secret"],
		GrantID:          "00000000-0000-0000-0000-000000000001",
		Bearer:           "platform-bearer-never-given-to-harness",
		ProxyURL:         proxyURL,
		Generation:       1,
		ExpiresAt:        time.Now().Add(time.Minute),
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/inference/session/id/activate", bytes.NewReader(body))
	request.SetPathValue("id", prepared["session_id"])
	recorder := httptest.NewRecorder()
	broker.activate(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("activate status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestInferenceBrokerRejectsSiblingSource(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("wrong-source request reached upstream")
	}))
	defer upstream.Close()
	broker := newInferenceBroker(1)
	broker.client = upstream.Client()
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSession(t, broker, prepared, upstream.URL)
	if !broker.bindSource(prepared["session_id"], "10.0.0.10") {
		t.Fatal("failed to bind prepared broker session")
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/inference/id/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen/qwen3-32b"}`))
	request.RemoteAddr = "10.0.0.11:4321"
	request.SetPathValue("rest", "v1/chat/completions")
	recorder := httptest.NewRecorder()
	broker.handle(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong source status = %d", recorder.Code)
	}
}

func TestInferenceBrokerAddsProofWithoutExposingBearerToHarness(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, header := range []string{"Authorization", "X-Ditto-Grant", "X-Ditto-Generation", "X-Ditto-Nonce", "X-Ditto-Requested-At", "X-Ditto-Proof"} {
			if r.Header.Get(header) == "" {
				t.Errorf("missing trusted broker header %s", header)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4},"choices":[]}`))
	}))
	defer upstream.Close()
	broker := newInferenceBroker(1)
	broker.client = upstream.Client()
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSession(t, broker, prepared, upstream.URL)
	if !broker.bindSource(prepared["session_id"], "192.0.2.10") {
		t.Fatal("failed to bind prepared broker session")
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/inference/id/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen/qwen3-32b","max_tokens":32}`))
	request.RemoteAddr = "192.0.2.10:4321"
	request.SetPathValue("rest", "v1/chat/completions")
	recorder := httptest.NewRecorder()
	broker.handle(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("platform-bearer")) {
		t.Fatal("platform bearer leaked to harness response")
	}
}

func TestInferenceBrokerTrustedProbeUsesControlPlaneSession(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":2,"completion_tokens":1},"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer upstream.Close()
	broker := newInferenceBroker(1)
	broker.client = upstream.Client()
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSession(t, broker, prepared, upstream.URL)
	if !broker.bindSource(prepared["session_id"], "192.0.2.30") {
		t.Fatal("failed to bind prepared broker session")
	}

	if err := broker.trustedProbe(context.Background(), prepared["session_id"]); err != nil {
		t.Fatalf("trusted probe: %v", err)
	}
	snapshot, err := broker.snapshot(prepared["session_id"])
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Requests != 1 || snapshot.Successes != 1 || snapshot.UsageAvailable != 1 {
		t.Fatalf("unexpected trusted probe accounting: %+v", snapshot)
	}
}

func TestInferenceBrokerPrunesUnactivatedSessionsBeforeCapacityCheck(t *testing.T) {
	broker := newInferenceBroker(1) // bounded to two prepared/active sessions
	first := prepareBrokerSession(t, broker)
	_ = prepareBrokerSession(t, broker)

	broker.mu.RLock()
	stale := broker.sessions[first["session_id"]]
	broker.mu.RUnlock()
	stale.mu.Lock()
	stale.preparedAt = time.Now().Add(-3 * time.Minute)
	stale.mu.Unlock()

	third := prepareBrokerSession(t, broker)
	if third["session_id"] == "" {
		t.Fatal("stale prepared session did not release broker capacity")
	}
}

func TestToolRouteIsSourceBoundAndRemoved(t *testing.T) {
	broker := newInferenceBroker(1)
	called := 0
	id, stop, err := broker.registerTool(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.URL.Path != "/tool" {
			t.Errorf("forwarded path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}), "192.0.2.20")
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/tools/route/tool", nil)
	request.SetPathValue("id", id)
	request.RemoteAddr = "192.0.2.21:1234"
	recorder := httptest.NewRecorder()
	broker.handleTool(recorder, request)
	if recorder.Code != http.StatusUnauthorized || called != 0 {
		t.Fatalf("sibling source status=%d called=%d", recorder.Code, called)
	}

	request.RemoteAddr = "192.0.2.20:1234"
	recorder = httptest.NewRecorder()
	broker.handleTool(recorder, request)
	if recorder.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("bound source status=%d called=%d", recorder.Code, called)
	}

	stop()
	recorder = httptest.NewRecorder()
	broker.handleTool(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("removed route status=%d", recorder.Code)
	}
}
