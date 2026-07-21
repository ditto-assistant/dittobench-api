package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
	broker := newInferenceBroker()
	broker.client = upstream.Client()
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSession(t, broker, prepared, upstream.URL)
	if !broker.bindSource(prepared["session_id"], "10.0.0.10") {
		t.Fatal("failed to bind prepared broker session")
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/inference/id/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen/qwen3-32b"}`))
	request.RemoteAddr = "10.0.0.11:4321"
	request.SetPathValue("id", prepared["session_id"])
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
	broker := newInferenceBroker()
	broker.client = upstream.Client()
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSession(t, broker, prepared, upstream.URL)
	if !broker.bindSource(prepared["session_id"], "192.0.2.10") {
		t.Fatal("failed to bind prepared broker session")
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/inference/id/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen/qwen3-32b","max_tokens":32}`))
	request.RemoteAddr = "192.0.2.10:4321"
	request.SetPathValue("id", prepared["session_id"])
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

func TestToolRouteIsSourceBoundAndRemoved(t *testing.T) {
	broker := newInferenceBroker()
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
