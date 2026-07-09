// Package llm is a minimal OpenRouter chat client used by the validator for two
// jobs: (1) the generator model paraphrases freshly-generated cases so wording
// is novel every run (anti-cheat), and (2) the scorer model backs the LLM judge
// (tool response-quality + LongMemEval memory yes/no).
//
// It talks to the OpenAI-compatible Chat Completions endpoint OpenRouter serves
// at https://openrouter.ai/api/v1/chat/completions and reads OPENROUTER_API_KEY
// from the environment. The base URL is overridable (LLM_BASE_URL) so the judge
// can be pointed at a self-hosted OpenAI-compatible gateway (vLLM / Ollama) for
// reproducible grading; see docs/judge-determinism.md. Model ids come from env
// with defaults:
//   - GENERATOR_MODEL (default "qwen/qwen3-32b")
//   - SCORER_MODEL    (default "google/gemini-3.1-flash-lite")
//
// Every call is sent with temperature 0, a fixed seed, and top_p 1 so that,
// against a serving stack that honors them, two validators grading the same
// submission converge on the same verdict (the k=3 median only means something
// if the judges agree).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// EnvAPIKey is the env var holding the OpenRouter API key.
	EnvAPIKey = "OPENROUTER_API_KEY"

	defaultGeneratorModel = "qwen/qwen3-32b"
	defaultScorerModel    = "google/gemini-3.1-flash-lite"

	// defaultHarnessModel is the v2 locked open-weight model every miner harness
	// is scored against. Locking the harness to ONE open-weight model shrinks the
	// exploit surface (no model routing to game, no judge-family collusion) and
	// makes the median-of-3 validators' scores of a submission comparable. Qwen2.5
	// is the v2 start (strong open-weight tool-calling). This is the ONE place the
	// locked model id lives; bump it here (or via HARNESS_MODEL) for v3. It must
	// name the model the host gateway actually serves.
	defaultHarnessModel = "qwen/qwen2.5-72b-instruct"

	endpoint = "https://openrouter.ai/api/v1/chat/completions"

	// deterministicSeed is the fixed sampling seed sent on every request. It is a
	// consensus constant, NOT configurable: every validator must send the same
	// seed for their judges to reproduce each other's verdicts (the whole point
	// of a fixed seed is cross-validator agreement, so an env override would
	// defeat it). Temperature 0 alone is only approximately reproducible on a
	// batched serving stack; seed + top_p 1 + temperature 0 against a stack that
	// honors them (self-hosted vLLM/Ollama, LLM_BASE_URL) is what actually pins
	// the output. A hosted multi-tenant model may ignore seed, so this is
	// best-effort there and load-bearing only once the judge is self-hosted.
	deterministicSeed = 42

	// defaultMaxTokens caps the completion length of every call. Judges emit a
	// short verdict and paraphrases a sentence or two, so this is generous
	// headroom — its job is to stop a model being coerced into an unbounded /
	// looping completion, the per-call half of the cost cap. Override with
	// LLM_MAX_TOKENS.
	defaultMaxTokens = 8192
	// defaultRunTokenBudget caps total tokens (prompt+completion) a single client
	// — i.e. one run_size job, which gets a fresh client — may spend. A full
	// profile is on the order of ~10^5–10^6 tokens; this leaves headroom while
	// stopping a pathological loop from burning unbounded spend. 0 = unlimited.
	// Override with LLM_RUN_TOKEN_BUDGET.
	defaultRunTokenBudget int64 = 8_000_000
)

// Client is a thin OpenRouter chat client. Safe for concurrent use.
type Client struct {
	apiKey    string
	baseURL   string
	http      *http.Client
	maxTokens int
	// budget is the per-client (== per-run) total-token ceiling; 0 disables it.
	// spent accumulates usage.total_tokens across calls and is checked before
	// each request so a run fails cleanly instead of burning unbounded spend.
	budget int64
	spent  atomic.Int64
}

// New returns a Client reading OPENROUTER_API_KEY from the environment. It
// returns an error if the key is unset so callers can fail fast at submit time.
func New() (*Client, error) {
	key := strings.TrimSpace(os.Getenv(EnvAPIKey))
	if key == "" {
		return nil, fmt.Errorf("%s is not set; an OpenRouter key is required for run_size submissions", EnvAPIKey)
	}
	return NewWithKey(key), nil
}

// NewWithKey returns a Client with an explicit key (used in tests / when the key
// is sourced elsewhere). The per-call and per-run cost caps are read from the
// environment (LLM_MAX_TOKENS / LLM_RUN_TOKEN_BUDGET) with safe defaults.
func NewWithKey(key string) *Client {
	return &Client{
		apiKey:    key,
		baseURL:   envStr("LLM_BASE_URL", endpoint),
		http:      &http.Client{Timeout: 90 * time.Second},
		maxTokens: envInt("LLM_MAX_TOKENS", defaultMaxTokens),
		budget:    int64(envInt("LLM_RUN_TOKEN_BUDGET", int(defaultRunTokenBudget))),
	}
}

// Spent reports the total tokens this client has consumed so far (for logging).
func (c *Client) Spent() int64 {
	if c == nil {
		return 0
	}
	return c.spent.Load()
}

// envInt reads a non-negative int env var, falling back to def when unset or
// unparsable. A parsed 0 is honored (disables the cap).
func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// envStr reads a non-empty trimmed string env var, falling back to def.
func envStr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// GeneratorModel returns the generator model id (env GENERATOR_MODEL or default).
func GeneratorModel() string {
	if m := strings.TrimSpace(os.Getenv("GENERATOR_MODEL")); m != "" {
		return m
	}
	return defaultGeneratorModel
}

// ScorerModel returns the judge model id (env SCORER_MODEL or default).
func ScorerModel() string {
	if m := strings.TrimSpace(os.Getenv("SCORER_MODEL")); m != "" {
		return m
	}
	return defaultScorerModel
}

// ScorerModelB returns the OPTIONAL second judge model id (env SCORER_MODEL_B),
// or "" when unset. When set, the scorer runs it alongside the primary judge on
// an audit slice of cases so a judge-specific exploit (e.g. a prompt-injection
// tuned to one model) is caught by the de-correlated second judge.
func ScorerModelB() string {
	return strings.TrimSpace(os.Getenv("SCORER_MODEL_B"))
}

// HarnessModel returns the v2 locked model id every miner harness is scored
// against (env HARNESS_MODEL or the Qwen2.5 default). It is the model-routing
// indirection: callers force this on the sandbox instead of letting the harness
// pick its own model, so scores are comparable across validators and model
// choice is not an attack surface. See defaultHarnessModel.
func HarnessModel() string {
	if m := strings.TrimSpace(os.Getenv("HARNESS_MODEL")); m != "" {
		return m
	}
	return defaultHarnessModel
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// Temperature/TopP/Seed are the determinism knobs, sent on every request.
	// Temperature 0 + TopP 1 is greedy argmax over the full vocabulary; Seed
	// pins the tiebreak. Always emitted (no omitempty) so the intent is explicit
	// on the wire and a provider default can never silently reintroduce sampling.
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
	Seed        int     `json:"seed"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		TotalTokens int64 `json:"total_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete sends a single system+user turn and returns the assistant text.
// Temperature 0, top_p 1, and a fixed seed are sent on every call so judging is
// reproducible against a serving stack that honors them. Temperature 0 by itself
// is only approximately stable on a batched hosted model, so full reproducibility
// requires a self-hosted judge (LLM_BASE_URL); see docs/judge-determinism.md.
func (c *Client) Complete(ctx context.Context, model, system, user string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("llm: nil client")
	}
	// Per-run cost cap: refuse to start another call once this client (one
	// run_size job) has spent its token budget, so a looping/hostile harness
	// fails the run instead of burning unbounded OpenRouter spend.
	if c.budget > 0 {
		if spent := c.spent.Load(); spent >= c.budget {
			return "", fmt.Errorf("llm: run token budget exhausted (%d/%d tokens); aborting", spent, c.budget)
		}
	}
	msgs := make([]chatMessage, 0, 2)
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: system})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: user})

	body, err := json.Marshal(chatRequest{
		Model:       model,
		Messages:    msgs,
		Temperature: 0,
		TopP:        1,
		Seed:        deterministicSeed,
		MaxTokens:   c.maxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	// Optional attribution headers OpenRouter recommends.
	req.Header.Set("HTTP-Referer", "https://github.com/ditto-assistant/dittobench-api")
	req.Header.Set("X-Title", "DittoBench Validator")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: post: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("llm: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm: %s returned %d: %s", model, resp.StatusCode, tail(string(raw), 500))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm: decode response: %w", err)
	}
	if out.Usage != nil {
		c.spent.Add(out.Usage.TotalTokens)
	}
	if out.Error != nil {
		return "", fmt.Errorf("llm: api error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: no choices returned")
	}
	return out.Choices[0].Message.Content, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
