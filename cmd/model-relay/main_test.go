package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRelayHealthTracksAuthoritativeUpstreamOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		wantSuccess  uint64
		wantFailures uint64
	}{
		{name: "completion", status: http.StatusOK, body: `{"choices":[{"message":{"content":"ok"}}]}`, wantSuccess: 1},
		{name: "provider unavailable", status: http.StatusServiceUnavailable, body: `unavailable`, wantFailures: 1},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `limited`, wantFailures: 1},
		{name: "bad credentials", status: http.StatusUnauthorized, body: `unauthorized`, wantFailures: 1},
		{name: "malformed success", status: http.StatusOK, body: `{"choices":[]}`, wantFailures: 1},
		{name: "harness request error", status: http.StatusBadRequest, body: `bad request`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			r := &relay{upstream: upstream.URL, apiKey: "k", model: "m", client: &http.Client{Timeout: 5 * time.Second}}
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/chat/completions", r.handle)
			mux.HandleFunc("GET /health", r.health)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			healthResp, err := http.Get(srv.URL + "/health")
			if err != nil {
				t.Fatal(err)
			}
			defer healthResp.Body.Close()
			var got relayHealth
			if err := json.NewDecoder(healthResp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.AccountingVersion != 1 || got.Status != "ok" || got.Requests != 1 || got.Successes != tc.wantSuccess || got.InfrastructureFailures != tc.wantFailures {
				t.Fatalf("health = %#v", got)
			}
		})
	}
}

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

// TestProviderProfilesAreFrozen: both certified profiles pin the exact scored
// backend. Chutes serves the TEE model id; OpenRouter serves the canonical
// slug with routing locked to the certified Nebius deployment and fallbacks
// disabled, so OpenRouter cannot silently reroute to an uncertified host.
func TestProviderProfilesAreFrozen(t *testing.T) {
	chutes, ok := providers["chutes"]
	if !ok || chutes.model != "Qwen/Qwen3-32B-TEE" || chutes.pinBody != nil {
		t.Fatalf("chutes profile drifted: %+v", chutes)
	}
	or, ok := providers["openrouter"]
	if !ok || or.model != "qwen/qwen3-32b" {
		t.Fatalf("openrouter profile drifted: %+v", or)
	}
	body := map[string]any{"provider": map[string]any{"only": []string{"attacker-host"}, "allow_fallbacks": true}}
	or.pinBody(body)
	pin, _ := body["provider"].(map[string]any)
	only, _ := pin["only"].([]string)
	if len(only) != 1 || only[0] != "nebius" || pin["allow_fallbacks"] != false {
		t.Fatalf("openrouter routing not locked to nebius/no-fallbacks: %v", body["provider"])
	}
}

// TestRelayOpenRouterPinsServingProvider: end to end through handle, a sandbox
// request that names its own provider routing reaches the upstream with the
// locked model slug and the nebius-only routing pin.
func TestRelayOpenRouterPinsServingProvider(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	profile := providers["openrouter"]
	rl := &relay{upstream: upstream.URL, apiKey: "k", model: profile.model, thinking: false, pinBody: profile.pinBody, client: &http.Client{Timeout: 5 * time.Second}}
	srv := httptest.NewServer(http.HandlerFunc(rl.handle))
	defer srv.Close()

	body := `{"model":"other/model","provider":{"only":["cheapest-host"],"allow_fallbacks":true},"messages":[]}`
	if _, err := http.Post(srv.URL, "application/json", strings.NewReader(body)); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got["model"] != "qwen/qwen3-32b" {
		t.Fatalf("model not pinned to canonical slug: %v", got["model"])
	}
	pin, _ := got["provider"].(map[string]any)
	only, _ := pin["only"].([]any)
	if len(only) != 1 || only[0] != "nebius" || pin["allow_fallbacks"] != false {
		t.Fatalf("serving provider not locked: %v", got["provider"])
	}
}

// TestRelayOpenRouterAppendsNoThinkLast: the openrouter profile must place the
// Qwen3 /no_think soft switch AFTER everything the sandbox wrote, because the
// latest soft directive wins — a sandbox-supplied "/think" must not be able to
// re-enable reasoning on a provider that ignores the hard template switch.
func TestRelayOpenRouterAppendsNoThinkLast(t *testing.T) {
	profile := providers["openrouter"]

	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "You are Ditto."},
		map[string]any{"role": "user", "content": "answer this /think"},
	}}
	profile.pinBody(body)
	msgs := body["messages"].([]any)
	lastContent := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	if !strings.HasSuffix(lastContent, "/no_think") {
		t.Fatalf("last message must end with /no_think, got %q", lastContent)
	}
	if !strings.Contains(lastContent, "/think") {
		t.Fatalf("sandbox content must be preserved, got %q", lastContent)
	}

	// Empty conversation still gets the switch.
	empty := map[string]any{}
	profile.pinBody(empty)
	msgs = empty["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "/no_think" {
		t.Fatalf("empty conversation must gain a /no_think message: %v", empty["messages"])
	}

	// The chutes profile never rewrites messages (the hard template switch
	// makes soft directives inert there).
	chutesBody := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if providers["chutes"].pinBody != nil {
		providers["chutes"].pinBody(chutesBody)
	}
	if got := chutesBody["messages"].([]any)[0].(map[string]any)["content"]; got != "hi" {
		t.Fatalf("chutes profile must not rewrite messages, got %q", got)
	}
}
