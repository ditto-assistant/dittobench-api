// Package runner drives a miner's harness over HTTP: it POSTs one RunRequest
// per dataset case to <harnessURL>/run and collects the RunResponses. Per-case
// failures (timeout, non-200, bad JSON) are recorded as empty responses so a
// single bad case never aborts the whole evaluation.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/netguard"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// perCaseTimeout bounds a single /run call.
const perCaseTimeout = 60 * time.Second

// healthTimeout bounds the /health probe.
const healthTimeout = 10 * time.Second

// seedTimeout bounds the /seed call (embedding a haystack can take a while).
// A CPU-only validator embedding a full "small" haystack — hundreds of long
// documents through a local embeddings server (e.g. Ollama) — can legitimately
// take several minutes, so the default is overridable via DITTOBENCH_SEED_TIMEOUT
// (a Go duration string such as "20m"). Values <= 0 or unparseable fall back to
// the default.
var seedTimeout = envDuration("DITTOBENCH_SEED_TIMEOUT", 5*time.Minute)

// envDuration reads a Go duration from key, returning def when unset, empty,
// unparseable, or non-positive.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// client is used for caller-supplied harness URLs. It defaults to a guarded
// client (no private targets); Configure relaxes it only for explicit local dev.
var client = netguard.Client(false)
var sandboxClient = netguard.Client(true)

type sandboxContextKey struct{}

// Configure sets the caller-supplied harness URL policy. allowPrivate=true is
// for explicit local development; hosted deployments leave it false. Validator-
// owned Docker sandboxes use TrustSandbox instead. Call once at startup.
func Configure(allowPrivate bool) { client = netguard.Client(allowPrivate) }

// TrustSandbox marks requests to a harness URL returned by the validator's own
// Sandbox.Run implementation. Those URLs are loopback-bound by construction,
// so they need a private-address-capable client. Caller-supplied harness URLs
// never receive this context and remain protected by the SSRF guard.
func TrustSandbox(ctx context.Context) context.Context {
	return context.WithValue(ctx, sandboxContextKey{}, struct{}{})
}

func clientFor(ctx context.Context) *http.Client {
	if _, ok := ctx.Value(sandboxContextKey{}).(struct{}); ok {
		return sandboxClient
	}
	return client
}

// Health probes <harnessURL>/health and returns nil on a 2xx response.
func Health(ctx context.Context, harnessURL string) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, harnessURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := clientFor(ctx).Do(req)
	if err != nil {
		return fmt.Errorf("harness unreachable: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("harness health returned %d", resp.StatusCode)
	}
	return nil
}

// WaitHealthy polls <harnessURL>/health until it returns 2xx or the deadline
// passes. Used by the sandbox path to wait for a freshly started container to
// come up before spending the evaluation on it.
func WaitHealthy(ctx context.Context, harnessURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if last = Health(ctx, harnessURL); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if last == nil {
		last = fmt.Errorf("timeout")
	}
	return fmt.Errorf("harness not healthy after %s: %w", timeout, last)
}

// RunHarness evaluates every case in ds against the harness and returns a map
// keyed by case ID. The returned map always contains an entry for every case
// (failed cases get a zero-value RunResponse). The error is non-nil only for a
// fundamentally unusable input (none currently); per-case errors are swallowed.
func RunHarness(ctx context.Context, harnessURL string, ds protocol.Dataset, tools []protocol.ToolDefinition) (map[string]protocol.RunResponse, error) {
	out := make(map[string]protocol.RunResponse, len(ds.ToolCases))

	for _, c := range ds.ToolCases {
		resp, err := runOne(ctx, harnessURL, c, tools, CaseOptions{})
		if err != nil {
			// Record an empty response; scorer treats absence/zero as a miss.
			out[c.ID] = protocol.RunResponse{}
			continue
		}
		out[c.ID] = resp
	}
	return out, nil
}

// Seed POSTs a fresh haystack to <harnessURL>/seed and returns the loaded
// counts the harness reports. Used by the run_size pipeline before memory cases.
func Seed(ctx context.Context, harnessURL string, req protocol.SeedRequest) (protocol.SeedResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()

	buf, err := json.Marshal(req)
	if err != nil {
		return protocol.SeedResponse{}, fmt.Errorf("marshal seed request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, harnessURL+"/seed", bytes.NewReader(buf))
	if err != nil {
		return protocol.SeedResponse{}, fmt.Errorf("build seed request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := clientFor(ctx).Do(httpReq)
	if err != nil {
		return protocol.SeedResponse{}, fmt.Errorf("post /seed: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return protocol.SeedResponse{}, fmt.Errorf("read /seed body: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return protocol.SeedResponse{}, fmt.Errorf("/seed returned %d: %s", httpResp.StatusCode, string(body))
	}
	var out protocol.SeedResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return protocol.SeedResponse{}, fmt.Errorf("decode /seed response: %w", err)
	}
	return out, nil
}

// CaseOptions carries the optional observed-execution per-case wire fields: a
// validator-served mock tool-execution endpoint the harness should route its
// non-memory tool calls through (so the validator observes the trajectory), and
// the user_id the case's memory graph was seeded under (multi-graph isolation).
// The zero value reproduces self-report behavior (no endpoint, default user).
type CaseOptions struct {
	ToolEndpoint string
	UserID       string
}

// RunCase POSTs one tool OR memory case to <harnessURL>/run. For a tool case,
// pass c (the toolcase) and prompt=c.Prompt; for a memory case, pass a synthetic
// ToolCase with the question as the prompt. Exported so the pipeline can run +
// score cases one at a time (appending partial results). opts carries the
// optional observed-execution fields (tool_endpoint, user_id).
func RunCase(ctx context.Context, harnessURL, caseID, prompt string, tools []protocol.ToolDefinition, opts CaseOptions) (protocol.RunResponse, error) {
	return runOne(ctx, harnessURL, protocol.ToolCase{ID: caseID, Prompt: prompt}, tools, opts)
}

func runOne(ctx context.Context, harnessURL string, c protocol.ToolCase, tools []protocol.ToolDefinition, opts CaseOptions) (protocol.RunResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, perCaseTimeout)
	defer cancel()

	reqBody := protocol.RunRequest{
		CaseID:       c.ID,
		SystemPrompt: "You are Ditto, a helpful assistant with access to tools. Call a tool only when it is the right action for the user's request.",
		UserInput:    c.Prompt,
		Tools:        tools,
		ToolEndpoint: opts.ToolEndpoint,
		UserID:       opts.UserID,
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return protocol.RunResponse{}, fmt.Errorf("marshal run request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, harnessURL+"/run", bytes.NewReader(buf))
	if err != nil {
		return protocol.RunResponse{}, fmt.Errorf("build run request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	httpResp, err := clientFor(ctx).Do(httpReq)
	if err != nil {
		return protocol.RunResponse{}, fmt.Errorf("post /run: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 4<<20))
	if err != nil {
		return protocol.RunResponse{}, fmt.Errorf("read /run body: %w", err)
	}
	elapsed := time.Since(start)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return protocol.RunResponse{}, fmt.Errorf("/run returned %d", httpResp.StatusCode)
	}

	var out protocol.RunResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return protocol.RunResponse{}, fmt.Errorf("decode /run response: %w", err)
	}
	// Measure latency validator-side (the /run round trip) and override any
	// self-reported value: a harness-supplied latency_ms is untrusted and must
	// never reach a score.
	out.LatencyMs = elapsed.Milliseconds()
	return out, nil
}
