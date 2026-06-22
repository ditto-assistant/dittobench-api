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
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/netguard"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// perCaseTimeout bounds a single /run call.
const perCaseTimeout = 60 * time.Second

// healthTimeout bounds the /health probe.
const healthTimeout = 10 * time.Second

// seedTimeout bounds the /seed call (embedding a haystack can take a while).
const seedTimeout = 5 * time.Minute

// client is used for all outbound harness calls. It defaults to a guarded client
// (no private targets); Configure swaps it for local/sandbox use.
var client = netguard.Client(false)

// Configure sets the outbound HTTP client's SSRF policy. allowPrivate=true (local
// dev + Docker sandbox, whose containers are on loopback) permits private/loopback
// targets; false (hosted) blocks them at dial time. Call once at startup.
func Configure(allowPrivate bool) { client = netguard.Client(allowPrivate) }

// Health probes <harnessURL>/health and returns nil on a 2xx response.
func Health(ctx context.Context, harnessURL string) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, harnessURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := client.Do(req)
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
		resp, err := runOne(ctx, harnessURL, c, tools)
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

	httpResp, err := client.Do(httpReq)
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

// RunCase POSTs one tool OR memory case to <harnessURL>/run. For a tool case,
// pass c (the toolcase) and prompt=c.Prompt; for a memory case, pass a synthetic
// ToolCase with the question as the prompt. Exported so the pipeline can run +
// score cases one at a time (appending partial results).
func RunCase(ctx context.Context, harnessURL, caseID, prompt string, tools []protocol.ToolDefinition) (protocol.RunResponse, error) {
	return runOne(ctx, harnessURL, protocol.ToolCase{ID: caseID, Prompt: prompt}, tools)
}

func runOne(ctx context.Context, harnessURL string, c protocol.ToolCase, tools []protocol.ToolDefinition) (protocol.RunResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, perCaseTimeout)
	defer cancel()

	reqBody := protocol.RunRequest{
		CaseID:       c.ID,
		SystemPrompt: "You are Ditto, a helpful assistant with access to tools. Call a tool only when it is the right action for the user's request.",
		UserInput:    c.Prompt,
		Tools:        tools,
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

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return protocol.RunResponse{}, fmt.Errorf("post /run: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 4<<20))
	if err != nil {
		return protocol.RunResponse{}, fmt.Errorf("read /run body: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return protocol.RunResponse{}, fmt.Errorf("/run returned %d", httpResp.StatusCode)
	}

	var out protocol.RunResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return protocol.RunResponse{}, fmt.Errorf("decode /run response: %w", err)
	}
	return out, nil
}
