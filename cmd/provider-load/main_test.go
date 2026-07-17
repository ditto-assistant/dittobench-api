package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/providercert"
)

func TestOpenRouterLoadPinsProviderAndAttributesApp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("HTTP-Referer"); got != providercert.AppReferer {
			t.Fatalf("HTTP-Referer = %q", got)
		}
		for _, header := range []string{"X-OpenRouter-Title", "X-Title"} {
			if got := r.Header.Get(header); got != providercert.AppTitle {
				t.Fatalf("%s = %q", header, got)
			}
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		provider := body["provider"].(map[string]any)
		if provider["allow_fallbacks"] != false || provider["only"].([]any)[0] != "nebius" {
			t.Fatalf("provider routing = %#v", provider)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"Nebius","choices":[{"finish_reason":"stop","native_finish_reason":"stop","message":{"content":"load response"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"cost":0.001}}`))
	}))
	defer server.Close()

	cfg := config{Endpoint: server.URL + "/chat/completions?openrouter.ai", Model: providercert.DefaultModel, Provider: "nebius", APIKey: "test-key", Concurrency: []int{1}, Waves: 1, MaxTokens: 32, RequestTimeout: time.Second}
	got := run(context.Background(), cfg)
	if len(got.Levels) != 1 || got.Levels[0].Successful != 1 || got.Levels[0].CompletionTokens != 20 {
		t.Fatalf("report = %#v", got)
	}
}

func TestParseLevels(t *testing.T) {
	got, err := parseLevels("1, 4,4,16")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 4 || got[2] != 16 {
		t.Fatalf("levels = %v", got)
	}
	if _, err := parseLevels("65"); err == nil {
		t.Fatal("expected upper-bound error")
	}
}
