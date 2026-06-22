// Package llm is a minimal OpenRouter chat client used by the validator for two
// jobs: (1) the generator model paraphrases freshly-generated cases so wording
// is novel every run (anti-cheat), and (2) the scorer model backs the LLM judge
// (tool response-quality + LongMemEval memory yes/no).
//
// It talks to the OpenAI-compatible Chat Completions endpoint OpenRouter serves
// at https://openrouter.ai/api/v1/chat/completions and reads OPENROUTER_API_KEY
// from the environment. Model ids come from env with defaults:
//   - GENERATOR_MODEL (default "qwen/qwen3-32b")
//   - SCORER_MODEL    (default "google/gemini-3.1-flash-lite")
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// EnvAPIKey is the env var holding the OpenRouter API key.
	EnvAPIKey = "OPENROUTER_API_KEY"

	defaultGeneratorModel = "qwen/qwen3-32b"
	defaultScorerModel    = "google/gemini-3.1-flash-lite"

	endpoint = "https://openrouter.ai/api/v1/chat/completions"
)

// Client is a thin OpenRouter chat client. Safe for concurrent use.
type Client struct {
	apiKey string
	http   *http.Client
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
// is sourced elsewhere).
func NewWithKey(key string) *Client {
	return &Client{
		apiKey: key,
		http:   &http.Client{Timeout: 90 * time.Second},
	}
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

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete sends a single system+user turn and returns the assistant text.
// Temperature is fixed at 0 for deterministic judging/paraphrase fidelity.
func (c *Client) Complete(ctx context.Context, model, system, user string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("llm: nil client")
	}
	msgs := make([]chatMessage, 0, 2)
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: system})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: user})

	body, err := json.Marshal(chatRequest{Model: model, Messages: msgs, Temperature: 0})
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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
