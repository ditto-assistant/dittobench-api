package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestModelDefaults(t *testing.T) {
	t.Setenv("GENERATOR_MODEL", "")
	t.Setenv("SCORER_MODEL", "")
	t.Setenv("HARNESS_MODEL", "")
	if got := GeneratorModel(); got != defaultGeneratorModel {
		t.Fatalf("generator default: got %q want %q", got, defaultGeneratorModel)
	}
	// The judge defaults to the locked harness model: one frozen open-weight
	// model, one source constant, no drift between the two.
	if got := ScorerModel(); got != defaultHarnessModel {
		t.Fatalf("scorer default: got %q want %q (the locked harness model)", got, defaultHarnessModel)
	}
}

func TestScorerModelTracksLockedHarnessModel(t *testing.T) {
	// Bumping the locked model (HARNESS_MODEL) must carry the judge with it
	// unless the operator pins a distinct judge via SCORER_MODEL.
	t.Setenv("SCORER_MODEL", "")
	t.Setenv("HARNESS_MODEL", "qwen/qwen3-next")
	if got := ScorerModel(); got != "qwen/qwen3-next" {
		t.Fatalf("scorer should follow HARNESS_MODEL: got %q", got)
	}
	t.Setenv("SCORER_MODEL", "acme/dedicated-judge")
	if got := ScorerModel(); got != "acme/dedicated-judge" {
		t.Fatalf("explicit SCORER_MODEL must win: got %q", got)
	}
}

func TestModelEnvOverride(t *testing.T) {
	t.Setenv("GENERATOR_MODEL", "acme/gen-1")
	t.Setenv("SCORER_MODEL", "acme/judge-1")
	if got := GeneratorModel(); got != "acme/gen-1" {
		t.Fatalf("generator override: got %q", got)
	}
	if got := ScorerModel(); got != "acme/judge-1" {
		t.Fatalf("scorer override: got %q", got)
	}
}

func TestNewRequiresKey(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	if _, err := New(); err == nil {
		t.Fatal("New() must error when the OpenRouter key is unset")
	}
	t.Setenv(EnvAPIKey, "sk-test")
	c, err := New()
	if err != nil || c == nil {
		t.Fatalf("New() with key: %v", err)
	}
}

// mockOpenRouter stands in for OpenRouter: it records the last request body and
// replies with fixed content + a usage total so the cost-cap machinery can be
// exercised without real spend.
func mockOpenRouter(t *testing.T, totalTokens int64, lastBody *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastBody != nil {
			_ = json.NewDecoder(r.Body).Decode(lastBody)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":` +
			strconv.FormatInt(totalTokens, 10) + `}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rewriteHost is a RoundTripper that redirects every request to the test server
// (the OpenRouter endpoint is a const, so we can't point the client by URL).
type rewriteHost struct{ target string }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := req.URL.Parse(r.target)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme, req.URL.Host, req.URL.Path = u.Scheme, u.Host, u.Path
	return http.DefaultTransport.RoundTrip(req)
}

func clientTo(url string) *Client {
	c := NewWithKey("test-key")
	c.http.Transport = rewriteHost{target: url}
	return c
}

func TestComplete_SendsMaxTokens(t *testing.T) {
	var body map[string]any
	srv := mockOpenRouter(t, 10, &body)
	c := clientTo(srv.URL)

	if _, err := c.Complete(context.Background(), "m", "sys", "hi"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	mt, ok := body["max_tokens"]
	if !ok {
		t.Fatal("request did not carry max_tokens (per-call cost cap missing)")
	}
	if int(mt.(float64)) != defaultMaxTokens {
		t.Fatalf("max_tokens = %v, want %d", mt, defaultMaxTokens)
	}
}

func TestComplete_SendsDeterminismKnobs(t *testing.T) {
	var body map[string]any
	srv := mockOpenRouter(t, 10, &body)
	c := clientTo(srv.URL)

	if _, err := c.Complete(context.Background(), "m", "sys", "hi"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Every request must pin the sampler so two validators reproduce each other.
	temp, ok := body["temperature"]
	if !ok || temp.(float64) != 0 {
		t.Fatalf("temperature = %v, want 0", body["temperature"])
	}
	topP, ok := body["top_p"]
	if !ok || topP.(float64) != 1 {
		t.Fatalf("top_p = %v, want 1", body["top_p"])
	}
	seed, ok := body["seed"]
	if !ok || int(seed.(float64)) != deterministicSeed {
		t.Fatalf("seed = %v, want %d", body["seed"], deterministicSeed)
	}
}

func TestComplete_HonorsBaseURLOverride(t *testing.T) {
	var body map[string]any
	srv := mockOpenRouter(t, 10, &body)
	// Point the judge at a self-hosted gateway via env, no transport rewrite.
	t.Setenv("LLM_BASE_URL", srv.URL)
	c := NewWithKey("test-key")
	if c.baseURL != srv.URL {
		t.Fatalf("baseURL = %q, want %q", c.baseURL, srv.URL)
	}
	if _, err := c.Complete(context.Background(), "m", "", "hi"); err != nil {
		t.Fatalf("Complete against override URL: %v", err)
	}
	if body["seed"] == nil {
		t.Fatal("request did not reach the overridden base URL")
	}
}

func TestCompleteJSON_ResponseFormat(t *testing.T) {
	// Self-hosted gateway (LLM_BASE_URL set): JSON mode defaults on, so judge
	// calls carry response_format json_object; plain Complete never does.
	var body map[string]any
	srv := mockOpenRouter(t, 10, &body)
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_RESPONSE_FORMAT", "")
	c := NewWithKey("test-key")

	if _, err := c.CompleteJSON(context.Background(), "m", "sys", "hi"); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	rf, ok := body["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Fatalf("response_format = %v, want {type: json_object}", body["response_format"])
	}
	// The determinism knobs must still be pinned on JSON-mode calls.
	if body["temperature"].(float64) != 0 || body["top_p"].(float64) != 1 || int(body["seed"].(float64)) != deterministicSeed {
		t.Fatalf("determinism knobs missing on JSON-mode call: %v", body)
	}

	body = nil
	if _, err := c.Complete(context.Background(), "m", "sys", "hi"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, present := body["response_format"]; present {
		t.Fatal("plain Complete must never send response_format (generator calls are free text)")
	}
}

func TestCompleteJSON_HostedDefaultOmitsResponseFormat(t *testing.T) {
	// Hosted default (no LLM_BASE_URL): the stack may reject response_format,
	// so JSON mode is off and CompleteJSON degrades to a plain call.
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_RESPONSE_FORMAT", "")
	var body map[string]any
	srv := mockOpenRouter(t, 10, &body)
	c := NewWithKey("test-key")
	c.http.Transport = rewriteHost{target: srv.URL}

	if _, err := c.CompleteJSON(context.Background(), "m", "sys", "hi"); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if _, present := body["response_format"]; present {
		t.Fatal("hosted default must omit response_format unless LLM_RESPONSE_FORMAT forces it")
	}
}

func TestCompleteJSON_EnvOverride(t *testing.T) {
	// LLM_RESPONSE_FORMAT wins in both directions over the LLM_BASE_URL default.
	var body map[string]any
	srv := mockOpenRouter(t, 10, &body)

	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_RESPONSE_FORMAT", "json_object")
	c := NewWithKey("test-key")
	c.http.Transport = rewriteHost{target: srv.URL}
	if _, err := c.CompleteJSON(context.Background(), "m", "", "hi"); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if _, present := body["response_format"]; !present {
		t.Fatal("LLM_RESPONSE_FORMAT=json_object must force JSON mode on")
	}

	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_RESPONSE_FORMAT", "off")
	c = NewWithKey("test-key")
	body = nil
	if _, err := c.CompleteJSON(context.Background(), "m", "", "hi"); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if _, present := body["response_format"]; present {
		t.Fatal("LLM_RESPONSE_FORMAT=off must suppress JSON mode even self-hosted")
	}
}

func TestComplete_TokenBudgetAccumulatesAndTrips(t *testing.T) {
	srv := mockOpenRouter(t, 100, nil)
	c := clientTo(srv.URL)
	c.budget = 150 // 100 tokens/call → the third call must be refused

	if _, err := c.Complete(context.Background(), "m", "", "a"); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if c.Spent() != 100 {
		t.Fatalf("spent = %d, want 100", c.Spent())
	}
	if _, err := c.Complete(context.Background(), "m", "", "b"); err != nil {
		t.Fatalf("call 2: %v", err) // allowed: spent 100 < 150, pushes to 200
	}
	if _, err := c.Complete(context.Background(), "m", "", "c"); err == nil {
		t.Fatal("expected the third call to be refused once the budget is exhausted")
	}
}

func TestComplete_ZeroBudgetUnlimited(t *testing.T) {
	srv := mockOpenRouter(t, 1_000_000, nil)
	c := clientTo(srv.URL)
	c.budget = 0 // disabled
	for i := 0; i < 3; i++ {
		if _, err := c.Complete(context.Background(), "m", "", "x"); err != nil {
			t.Fatalf("call %d failed with budget disabled: %v", i, err)
		}
	}
}
