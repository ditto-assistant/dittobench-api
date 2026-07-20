package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-api/internal/store"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

const relayProbeTimeout = 30 * time.Second

type relayHealthSnapshot struct {
	AccountingVersion      int    `json:"accounting_version"`
	Status                 string `json:"status"`
	Requests               uint64 `json:"requests"`
	Successes              uint64 `json:"successes"`
	InfrastructureFailures uint64 `json:"infrastructure_failures"`
	Provider               string `json:"provider"`
	ProfileRevision        string `json:"profile_revision"`
	Model                  string `json:"model"`
	UsageAvailable         uint64 `json:"usage_available"`
	UsageUnavailable       uint64 `json:"usage_unavailable"`
	PromptTokens           uint64 `json:"prompt_tokens"`
	PromptBytes            uint64 `json:"prompt_bytes"`
	CompletionTokens       uint64 `json:"completion_tokens"`
	ProviderLatencyMs      uint64 `json:"provider_latency_ms"`
	TTFTStatus             string `json:"ttft_status"`
}

// lockScoredRelayRun serializes the portion of scored jobs that can call the
// single model relay owned by this API instance. The relay's health accounting
// is intentionally aggregate, so isolation must begin before the harness
// preflight can make a model request and remain in force through the final
// counter snapshot. The returned closure makes early-return paths release the
// lease reliably.
func (s *server) lockScoredRelayRun() func() {
	s.relayRunMu.Lock()
	return s.relayRunMu.Unlock
}

// probeLockedModelRelay makes one deliberately tiny completion through the
// same operator-owned gateway injected into every scored harness. The harness
// /health and tool-endpoint preflight do not exercise this dependency, so both
// can pass while a provider outage turns every model-backed case into a silent
// miss.
func probeLockedModelRelay(ctx context.Context, gateway string) error {
	ctx, cancel := context.WithTimeout(ctx, relayProbeTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"model": llm.HarnessModel(),
		"messages": []map[string]string{{
			"role":    "user",
			"content": "Reply OK.",
		}},
		"max_tokens":  1,
		"temperature": 0,
		"stream":      false,
	})
	if err != nil {
		return fmt.Errorf("encode probe")
	}

	endpoint, err := relayURL(gateway, "/v1/chat/completions")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build probe request")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("relay unreachable")
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read relay response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("relay returned %d", resp.StatusCode)
	}
	var decoded struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(responseBody, &decoded); err != nil || len(decoded.Choices) == 0 {
		return fmt.Errorf("relay returned no completion")
	}
	return nil
}

func relayURL(gateway, path string) (string, error) {
	raw := strings.TrimSpace(gateway)
	if raw == "" {
		return "", fmt.Errorf("gateway is not configured")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("gateway is invalid")
	}
	basePath := strings.TrimRight(u.Path, "/")
	basePath = strings.TrimSuffix(basePath, "/v1")
	u.Path = basePath + path
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func readRelayHealth(ctx context.Context, gateway string) (relayHealthSnapshot, error) {
	endpoint, err := relayURL(gateway, "/health")
	if err != nil {
		return relayHealthSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, relayProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return relayHealthSnapshot{}, fmt.Errorf("build relay health request")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return relayHealthSnapshot{}, fmt.Errorf("relay health unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return relayHealthSnapshot{}, fmt.Errorf("relay health returned %d", resp.StatusCode)
	}
	var snapshot relayHealthSnapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&snapshot); err != nil {
		return relayHealthSnapshot{}, fmt.Errorf("relay health malformed")
	}
	if snapshot.Status != "ok" {
		return relayHealthSnapshot{}, fmt.Errorf("relay health is not ok")
	}
	if snapshot.AccountingVersion != 1 && snapshot.AccountingVersion != 2 {
		return relayHealthSnapshot{}, fmt.Errorf("relay health lacks run accounting")
	}
	return snapshot, nil
}

func requireTokenAccounting(snapshot relayHealthSnapshot) error {
	if snapshot.AccountingVersion != 2 {
		return fmt.Errorf("relay health lacks trusted token accounting")
	}
	if snapshot.Provider == "" || snapshot.ProfileRevision == "" || snapshot.Model == "" {
		return fmt.Errorf("relay health lacks immutable provider identity")
	}
	return nil
}

func relayDegradedSince(start, end relayHealthSnapshot) error {
	if end.Requests < start.Requests || end.Successes < start.Successes || end.InfrastructureFailures < start.InfrastructureFailures {
		return fmt.Errorf("relay restarted during benchmark")
	}
	if end.InfrastructureFailures > start.InfrastructureFailures {
		return fmt.Errorf("relay recorded %d upstream failure(s) during benchmark", end.InfrastructureFailures-start.InfrastructureFailures)
	}
	return nil
}

func relayUsageSince(start, end relayHealthSnapshot) (protocol.TokenUsage, error) {
	usage := protocol.TokenUsage{
		AccountingVersion: end.AccountingVersion,
		Status:            "unavailable",
		Source:            "model_proxy_provider_response",
		Provider:          end.Provider,
		ProfileRevision:   end.ProfileRevision,
		Model:             end.Model,
		TTFTStatus:        end.TTFTStatus,
	}
	if start.AccountingVersion != end.AccountingVersion {
		return usage, fmt.Errorf("relay accounting version changed during benchmark")
	}
	if start.Provider != end.Provider || start.ProfileRevision != end.ProfileRevision || start.Model != end.Model {
		return usage, fmt.Errorf("relay provider profile changed during benchmark")
	}
	if end.Requests < start.Requests || end.Successes < start.Successes ||
		end.UsageAvailable < start.UsageAvailable || end.UsageUnavailable < start.UsageUnavailable ||
		end.PromptTokens < start.PromptTokens || end.PromptBytes < start.PromptBytes ||
		end.CompletionTokens < start.CompletionTokens ||
		end.ProviderLatencyMs < start.ProviderLatencyMs {
		return usage, fmt.Errorf("relay restarted during benchmark")
	}
	usage.Requests = end.Requests - start.Requests
	usage.Successes = end.Successes - start.Successes
	usage.UsageAvailable = end.UsageAvailable - start.UsageAvailable
	usage.UsageUnavailable = end.UsageUnavailable - start.UsageUnavailable
	usage.PromptTokens = end.PromptTokens - start.PromptTokens
	usage.PromptBytes = end.PromptBytes - start.PromptBytes
	usage.CompletionTokens = end.CompletionTokens - start.CompletionTokens
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	usage.ProviderLatencyMs = end.ProviderLatencyMs - start.ProviderLatencyMs
	if usage.Successes == usage.UsageAvailable && usage.UsageUnavailable == 0 {
		usage.Status = "complete"
	}
	return usage, nil
}

func (s *server) relayRunStart(ctx context.Context, runID string, benchVersion int) (relayHealthSnapshot, bool) {
	gateway := envOr("HARNESS_GATEWAY_URL", "http://host.docker.internal:11434")
	if err := probeLockedModelRelay(ctx, gateway); err != nil {
		s.failRelayUnavailable(runID, err)
		return relayHealthSnapshot{}, false
	}
	snapshot, err := readRelayHealth(ctx, gateway)
	if err != nil {
		s.failRelayUnavailable(runID, err)
		return relayHealthSnapshot{}, false
	}
	if benchVersion >= protocol.BenchVersionV5 {
		if err := requireTokenAccounting(snapshot); err != nil {
			s.failRelayUnavailable(runID, err)
			return relayHealthSnapshot{}, false
		}
	}
	return snapshot, true
}

func (s *server) relayRunResult(ctx context.Context, runID string, start relayHealthSnapshot) (protocol.TokenUsage, bool) {
	gateway := envOr("HARNESS_GATEWAY_URL", "http://host.docker.internal:11434")
	end, err := readRelayHealth(ctx, gateway)
	if err == nil {
		err = relayDegradedSince(start, end)
	}
	if err != nil {
		s.failRelayUnavailable(runID, err)
		return protocol.TokenUsage{}, false
	}
	usage, usageErr := relayUsageSince(start, end)
	if usageErr != nil {
		s.failRelayUnavailable(runID, usageErr)
		return protocol.TokenUsage{}, false
	}
	return usage, true
}

func (s *server) failRelayUnavailable(runID string, err error) {
	s.store.FailWith(runID, "locked model relay unavailable: "+err.Error(), &store.Failure{
		Kind:      "validator_infrastructure",
		Code:      "model_relay_unavailable",
		Retryable: true,
	})
}
