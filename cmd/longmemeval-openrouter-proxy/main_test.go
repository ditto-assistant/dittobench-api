package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProfilesRewriteOnlyFrozenModels(t *testing.T) {
	tests := []struct {
		name, inputModel, upstreamModel string
	}{
		{"reader", readerModel, readerModel},
		{"judge", officialJudgeModel, openRouterJudgeModel},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected, err := profileFor(test.name)
			if err != nil {
				t.Fatal(err)
			}
			body, err := rewriteRequest(selected, []byte(`{"model":"`+test.inputModel+`","messages":[],"stream":false}`))
			if err != nil {
				t.Fatal(err)
			}
			var request map[string]any
			if json.Unmarshal(body, &request) != nil || request["model"] != test.upstreamModel || request["stream"] != false {
				t.Fatalf("request=%#v", request)
			}
			provider, _ := request["provider"].(map[string]any)
			if provider["allow_fallbacks"] != false || provider["data_collection"] != "deny" {
				t.Fatalf("provider=%#v", provider)
			}
			if _, err := rewriteRequest(selected, []byte(`{"model":"other","messages":[]}`)); err == nil {
				t.Fatal("unreviewed model accepted")
			}
			if _, err := rewriteRequest(selected, []byte(`{"model":"`+test.inputModel+`","stream":true}`)); err == nil {
				t.Fatal("streaming accepted")
			}
		})
	}
}

func TestProxyForwardsPinnedRequestAndUsage(t *testing.T) {
	selected, _ := profileFor("reader")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing upstream authorization")
		}
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil || request["model"] != readerModel {
			t.Fatalf("request=%#v", request)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 3},
		})
	}))
	defer upstream.Close()
	handler := newProxy(selected, "secret", upstream.URL, upstream.Client())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(
		`{"model":"openai/gpt-4.1","messages":[]}`,
	))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || handler.promptTokens.Load() != 12 || handler.completionTokens.Load() != 3 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
