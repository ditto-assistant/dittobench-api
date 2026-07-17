// Command provider-load runs a bounded, no-retry concurrency ramp against an
// OpenAI-compatible chat endpoint. It is intended for controlled provider
// certification, not production load generation.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/providercert"
)

type config struct {
	Endpoint       string
	Model          string
	Provider       string
	APIKey         string
	Concurrency    []int
	Waves          int
	MaxTokens      int
	RequestTimeout time.Duration
}

type report struct {
	SchemaVersion int           `json:"schema_version"`
	StartedAt     time.Time     `json:"started_at"`
	FinishedAt    time.Time     `json:"finished_at"`
	Endpoint      string        `json:"endpoint"`
	Model         string        `json:"model"`
	Provider      string        `json:"provider,omitempty"`
	Waves         int           `json:"waves"`
	MaxTokens     int           `json:"max_tokens"`
	RetryAttempts int           `json:"retry_attempts"`
	Levels        []levelReport `json:"levels"`
}

type levelReport struct {
	Concurrency                 int            `json:"concurrency"`
	Requests                    int            `json:"requests"`
	Successful                  int            `json:"successful"`
	SuccessRate                 float64        `json:"success_rate"`
	RateLimited                 int            `json:"rate_limited"`
	ServerErrors                int            `json:"server_errors"`
	SchemaErrors                int            `json:"schema_errors"`
	WallMS                      int64          `json:"wall_ms"`
	SuccessfulRequestsPerSecond float64        `json:"successful_requests_per_second"`
	CompletionTokensPerSecond   float64        `json:"completion_tokens_per_second"`
	P50LatencyMS                int64          `json:"p50_latency_ms"`
	P95LatencyMS                int64          `json:"p95_latency_ms"`
	P99LatencyMS                int64          `json:"p99_latency_ms"`
	PromptTokens                int            `json:"prompt_tokens"`
	CompletionTokens            int            `json:"completion_tokens"`
	TotalTokens                 int            `json:"total_tokens"`
	CostUSD                     float64        `json:"cost_usd"`
	ResponseProviders           map[string]int `json:"response_providers"`
	Observations                []observation  `json:"observations"`
}

type observation struct {
	Index              int     `json:"index"`
	HTTPStatus         int     `json:"http_status"`
	LatencyMS          int64   `json:"latency_ms"`
	Success            bool    `json:"success"`
	ErrorKind          string  `json:"error_kind,omitempty"`
	FinishReason       string  `json:"finish_reason,omitempty"`
	NativeFinishReason string  `json:"native_finish_reason,omitempty"`
	ResponseProvider   string  `json:"response_provider,omitempty"`
	PromptTokens       int     `json:"prompt_tokens"`
	CompletionTokens   int     `json:"completion_tokens"`
	TotalTokens        int     `json:"total_tokens"`
	CostUSD            float64 `json:"cost_usd"`
}

type chatResponse struct {
	Provider string `json:"provider"`
	Choices  []struct {
		FinishReason       string `json:"finish_reason"`
		NativeFinishReason string `json:"native_finish_reason"`
		Message            struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		TotalTokens      int     `json:"total_tokens"`
		Cost             float64 `json:"cost"`
	} `json:"usage"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func main() {
	endpoint := flag.String("endpoint", "", "chat completions endpoint")
	model := flag.String("model", "", "exact upstream model identifier")
	provider := flag.String("provider", "", "OpenRouter provider slug; pins routing and disables fallbacks")
	concurrency := flag.String("concurrency", "1,2,4,8,16", "comma-separated bounded concurrency levels")
	waves := flag.Int("waves", 3, "requests per worker at each level")
	maxTokens := flag.Int("max-tokens", 128, "maximum completion tokens per request")
	timeout := flag.Duration("timeout", 120*time.Second, "per-request timeout")
	output := flag.String("output", "", "write JSON report here")
	flag.Parse()
	levels, err := parseLevels(*concurrency)
	if err != nil {
		fatal(err)
	}
	apiKey := strings.TrimSpace(os.Getenv("PROVIDER_LOAD_API_KEY"))
	if *endpoint == "" || *model == "" || apiKey == "" || *waves < 1 || *maxTokens < 1 {
		fatal(errors.New("-endpoint, -model, positive -waves/-max-tokens, and PROVIDER_LOAD_API_KEY are required"))
	}
	cfg := config{Endpoint: *endpoint, Model: *model, Provider: *provider, APIKey: apiKey, Concurrency: levels, Waves: *waves, MaxTokens: *maxTokens, RequestTimeout: *timeout}
	result := run(context.Background(), cfg)
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatal(err)
	}
	raw = append(raw, '\n')
	if *output == "" {
		_, _ = os.Stdout.Write(raw)
		return
	}
	if err := os.WriteFile(*output, raw, 0o644); err != nil {
		fatal(err)
	}
}

func run(ctx context.Context, cfg config) report {
	result := report{SchemaVersion: 1, StartedAt: time.Now().UTC(), Endpoint: cfg.Endpoint, Model: cfg.Model, Provider: cfg.Provider, Waves: cfg.Waves, MaxTokens: cfg.MaxTokens, RetryAttempts: 0}
	client := &http.Client{}
	for _, concurrency := range cfg.Concurrency {
		result.Levels = append(result.Levels, runLevel(ctx, client, cfg, concurrency))
	}
	result.FinishedAt = time.Now().UTC()
	return result
}

func runLevel(ctx context.Context, client *http.Client, cfg config, concurrency int) levelReport {
	count := concurrency * cfg.Waves
	observations := make([]observation, count)
	jobs := make(chan int)
	started := time.Now()
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				item := request(ctx, client, cfg, concurrency*100000+index)
				item.Index = index
				observations[index] = item
			}
		}()
	}
	for index := range count {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return summarizeLevel(concurrency, time.Since(started), observations)
}

func request(parent context.Context, client *http.Client, cfg config, index int) observation {
	body := map[string]any{
		"model":       cfg.Model,
		"messages":    []map[string]string{{"role": "user", "content": fmt.Sprintf("Load certification request %d. Write exactly 100 short English words about reliable distributed systems. Do not use tools.", index)}},
		"temperature": 0, "max_tokens": cfg.MaxTokens, "stream": false,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	if cfg.Provider != "" {
		body["reasoning"] = map[string]any{"enabled": false, "exclude": false}
		body["provider"] = map[string]any{"only": []string{cfg.Provider}, "order": []string{cfg.Provider}, "allow_fallbacks": false, "require_parameters": false}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return observation{Index: index, ErrorKind: "marshal"}
	}
	ctx, cancel := context.WithTimeout(parent, cfg.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return observation{Index: index, ErrorKind: "request"}
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if strings.Contains(strings.ToLower(cfg.Endpoint), "openrouter.ai") {
		providercert.SetAttributionHeaders(req.Header)
	}
	started := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		kind := "transport"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			kind = "timeout"
		}
		return observation{Index: index, LatencyMS: latency, ErrorKind: kind}
	}
	defer resp.Body.Close()
	out := observation{Index: index, HTTPStatus: resp.StatusCode, LatencyMS: latency}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		out.ErrorKind = "read"
		return out
	}
	var decoded chatResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		out.ErrorKind = "malformed_json"
		return out
	}
	out.ResponseProvider = decoded.Provider
	out.PromptTokens, out.CompletionTokens, out.TotalTokens, out.CostUSD = decoded.Usage.PromptTokens, decoded.Usage.CompletionTokens, decoded.Usage.TotalTokens, decoded.Usage.Cost
	if resp.StatusCode == http.StatusTooManyRequests {
		out.ErrorKind = "rate_limited"
	} else if resp.StatusCode >= 500 {
		out.ErrorKind = "server_error"
	} else if resp.StatusCode < 200 || resp.StatusCode >= 300 || decoded.Error != nil {
		out.ErrorKind = "api_error"
	} else if len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content == "" || decoded.Usage.TotalTokens == 0 {
		out.ErrorKind = "schema"
	} else {
		out.Success = true
		out.FinishReason = decoded.Choices[0].FinishReason
		out.NativeFinishReason = decoded.Choices[0].NativeFinishReason
	}
	return out
}

func summarizeLevel(concurrency int, wall time.Duration, observations []observation) levelReport {
	result := levelReport{Concurrency: concurrency, Requests: len(observations), WallMS: wall.Milliseconds(), ResponseProviders: map[string]int{}, Observations: observations}
	var latencies []int64
	for _, item := range observations {
		if item.Success {
			result.Successful++
			latencies = append(latencies, item.LatencyMS)
		}
		if item.ErrorKind == "rate_limited" {
			result.RateLimited++
		}
		if item.ErrorKind == "server_error" {
			result.ServerErrors++
		}
		if item.ErrorKind == "schema" || item.ErrorKind == "malformed_json" {
			result.SchemaErrors++
		}
		result.PromptTokens += item.PromptTokens
		result.CompletionTokens += item.CompletionTokens
		result.TotalTokens += item.TotalTokens
		result.CostUSD += item.CostUSD
		if item.ResponseProvider != "" {
			result.ResponseProviders[item.ResponseProvider]++
		}
	}
	if result.Requests > 0 {
		result.SuccessRate = float64(result.Successful) / float64(result.Requests)
	}
	if seconds := wall.Seconds(); seconds > 0 {
		result.SuccessfulRequestsPerSecond = float64(result.Successful) / seconds
		result.CompletionTokensPerSecond = float64(result.CompletionTokens) / seconds
	}
	result.P50LatencyMS, result.P95LatencyMS, result.P99LatencyMS = percentile(latencies, .50), percentile(latencies, .95), percentile(latencies, .99)
	return result
}

func percentile(values []int64, quantile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]int64(nil), values...)
	sort.Slice(copyValues, func(i, j int) bool { return copyValues[i] < copyValues[j] })
	index := int(float64(len(copyValues)-1) * quantile)
	return copyValues[index]
}

func parseLevels(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	levels := make([]int, 0, len(parts))
	seen := map[int]bool{}
	for _, part := range parts {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || value < 1 || value > 64 {
			return nil, fmt.Errorf("invalid concurrency %q; values must be in [1,64]", part)
		}
		if !seen[value] {
			seen[value] = true
			levels = append(levels, value)
		}
	}
	if len(levels) == 0 {
		return nil, errors.New("at least one concurrency level is required")
	}
	return levels, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "provider-load:", err)
	os.Exit(1)
}
