package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRelayPinsModelAndKey: whatever model, stream setting, or Authorization
// header the sandbox sends, the upstream sees the locked model, stream:false,
// and the relay's own key.
func TestRelayPinsModelAndKey(t *testing.T) {
	var got map[string]any
	var auth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	rl := &relay{upstream: upstream.URL, apiKey: "relay-key", model: "locked/model", thinking: false, client: &http.Client{Timeout: 5 * time.Second}}
	srv := httptest.NewServer(http.HandlerFunc(rl.handle))
	defer srv.Close()

	body := `{"model":"attacker/frontier-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sandbox-stolen-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got["model"] != "locked/model" {
		t.Fatalf("model not pinned: upstream saw %v", got["model"])
	}
	if got["stream"] != false {
		t.Fatalf("stream not forced off: %v", got["stream"])
	}
	if auth != "Bearer relay-key" {
		t.Fatalf("upstream must see the relay's key, saw %q", auth)
	}
	if got["messages"] == nil {
		t.Fatal("messages must pass through")
	}
	// Thinking is locked: even a request that asked for it gets the relay's mode.
	ctk, ok := got["chat_template_kwargs"].(map[string]any)
	if !ok || ctk["enable_thinking"] != false {
		t.Fatalf("thinking not locked off: %v", got["chat_template_kwargs"])
	}
}

// TestRelayLocksThinkingOverSandboxChoice: a sandbox-supplied
// chat_template_kwargs cannot re-enable thinking.
func TestRelayLocksThinkingOverSandboxChoice(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	rl := &relay{upstream: upstream.URL, apiKey: "k", model: "m", thinking: false, client: &http.Client{Timeout: 5 * time.Second}}
	srv := httptest.NewServer(http.HandlerFunc(rl.handle))
	defer srv.Close()

	body := `{"model":"m","chat_template_kwargs":{"enable_thinking":true,"custom":"kept"},"messages":[]}`
	if _, err := http.Post(srv.URL, "application/json", strings.NewReader(body)); err != nil {
		t.Fatalf("post: %v", err)
	}
	ctk := got["chat_template_kwargs"].(map[string]any)
	if ctk["enable_thinking"] != false {
		t.Fatalf("sandbox re-enabled thinking: %v", ctk)
	}
	if ctk["custom"] != "kept" {
		t.Fatalf("unrelated kwargs must pass through: %v", ctk)
	}
}

func TestRelayRejectsNonJSON(t *testing.T) {
	rl := &relay{upstream: "http://unreachable.invalid", apiKey: "k", model: "m", client: http.DefaultClient}
	srv := httptest.NewServer(http.HandlerFunc(rl.handle))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for non-JSON, got %d", resp.StatusCode)
	}
}
