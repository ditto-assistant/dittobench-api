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

	"github.com/ditto-assistant/dittobench-api/internal/efficiency"
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
	CallerCancellations    uint64 `json:"caller_cancellations"`
	UpstreamAttempts       uint64 `json:"upstream_attempts"`
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

type relayExecutionSummary struct {
	Requests               uint64 `json:"requests"`
	Successes              uint64 `json:"successes"`
	InfrastructureFailures uint64 `json:"infrastructure_failures"`
	CallerCancellations    uint64 `json:"caller_cancellations"`
	UpstreamAttempts       uint64 `json:"upstream_attempts"`
	Retries                uint64 `json:"retries"`
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
func probeLockedModelRelay(ctx context.Context, gateway string, benchVersion int) error {
	ctx, cancel := context.WithTimeout(ctx, relayProbeTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"model": llm.HarnessModelForVersion(benchVersion),
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

func validV7RouteProfile(profile string) bool {
	const prefix = "openrouter-route-"
	const suffix = "-v1"
	if !strings.HasPrefix(profile, prefix) || !strings.HasSuffix(profile, suffix) {
		return false
	}
	digest := strings.TrimSuffix(strings.TrimPrefix(profile, prefix), suffix)
	if len(digest) != 16 || digest != strings.ToLower(digest) {
		return false
	}
	for _, r := range digest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func requireTokenAccounting(snapshot relayHealthSnapshot, benchVersion int, runSize string, v7TokenCalibration bool) error {
	if snapshot.AccountingVersion != 2 {
		return fmt.Errorf("relay health lacks trusted token accounting")
	}
	if snapshot.Provider == "" || snapshot.ProfileRevision == "" || snapshot.Model == "" {
		return fmt.Errorf("relay health lacks immutable provider identity")
	}
	if benchVersion >= protocol.BenchVersionV7 {
		if snapshot.Model != llm.V7HarnessModel {
			return fmt.Errorf("relay model does not match benchmark v7")
		}
		if !validV7RouteProfile(snapshot.ProfileRevision) {
			return fmt.Errorf("relay profile does not match benchmark v7")
		}
		identity := protocol.TokenUsage{
			Provider:        snapshot.Provider,
			ProfileRevision: snapshot.ProfileRevision,
			Model:           snapshot.Model,
		}
		if _, ok := efficiency.LookupForVersion(benchVersion, runSize, identity); !ok && !v7TokenCalibration {
			return fmt.Errorf("relay profile lacks a reviewed benchmark v7 token baseline")
		}
	}
	return nil
}

func requireCompleteV7Usage(benchVersion int, usage protocol.TokenUsage) error {
	if benchVersion >= protocol.BenchVersionV7 && !efficiency.ValidUsage(usage) {
		return fmt.Errorf("benchmark v7 requires complete provider token usage")
	}
	return nil
}

func relayDegradedSince(start, end relayHealthSnapshot) error {
	if end.Requests < start.Requests || end.Successes < start.Successes ||
		end.InfrastructureFailures < start.InfrastructureFailures ||
		end.CallerCancellations < start.CallerCancellations || end.UpstreamAttempts < start.UpstreamAttempts {
		return fmt.Errorf("relay restarted during benchmark")
	}
	if end.InfrastructureFailures > start.InfrastructureFailures {
		return fmt.Errorf("relay recorded %d upstream failure(s) during benchmark", end.InfrastructureFailures-start.InfrastructureFailures)
	}
	return nil
}

func relayExecutionSince(start, end relayHealthSnapshot) (relayExecutionSummary, error) {
	if end.Requests < start.Requests || end.Successes < start.Successes ||
		end.InfrastructureFailures < start.InfrastructureFailures ||
		end.CallerCancellations < start.CallerCancellations || end.UpstreamAttempts < start.UpstreamAttempts {
		return relayExecutionSummary{}, fmt.Errorf("relay restarted during benchmark")
	}
	summary := relayExecutionSummary{
		Requests:               end.Requests - start.Requests,
		Successes:              end.Successes - start.Successes,
		InfrastructureFailures: end.InfrastructureFailures - start.InfrastructureFailures,
		CallerCancellations:    end.CallerCancellations - start.CallerCancellations,
		UpstreamAttempts:       end.UpstreamAttempts - start.UpstreamAttempts,
	}
	if summary.UpstreamAttempts > summary.Requests {
		summary.Retries = summary.UpstreamAttempts - summary.Requests
	}
	return summary, nil
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

func (s *server) relayRunStart(ctx context.Context, runID string, benchVersion int, runSize, gateway, sessionID string) (relayHealthSnapshot, bool) {
	var probeErr error
	if sessionID != "" {
		probeErr = s.broker.trustedProbe(ctx, sessionID)
	} else {
		probeErr = probeLockedModelRelay(ctx, gateway, benchVersion)
	}
	if probeErr != nil {
		s.failRelayUnavailable(runID, probeErr)
		return relayHealthSnapshot{}, false
	}
	var snapshot relayHealthSnapshot
	var err error
	if sessionID != "" {
		snapshot, err = s.broker.snapshot(sessionID)
	} else {
		snapshot, err = readRelayHealth(ctx, gateway)
	}
	if err != nil {
		s.failRelayUnavailable(runID, err)
		return relayHealthSnapshot{}, false
	}
	if benchVersion >= protocol.BenchVersionV5 {
		if err := requireTokenAccounting(snapshot, benchVersion, runSize, s.v7TokenCalibration); err != nil {
			s.failRelayUnavailable(runID, err)
			return relayHealthSnapshot{}, false
		}
	}
	return snapshot, true
}

func (s *server) relayRunResult(ctx context.Context, runID string, start relayHealthSnapshot, gateway, sessionID string) (protocol.TokenUsage, relayExecutionSummary, bool) {
	var end relayHealthSnapshot
	var err error
	if sessionID != "" {
		end, err = s.broker.snapshot(sessionID)
	} else {
		end, err = readRelayHealth(ctx, gateway)
	}
	if err == nil {
		err = relayDegradedSince(start, end)
	}
	if err != nil {
		s.failRelayUnavailable(runID, err)
		return protocol.TokenUsage{}, relayExecutionSummary{}, false
	}
	usage, usageErr := relayUsageSince(start, end)
	if usageErr != nil {
		s.failRelayUnavailable(runID, usageErr)
		return protocol.TokenUsage{}, relayExecutionSummary{}, false
	}
	execution, executionErr := relayExecutionSince(start, end)
	if executionErr != nil {
		s.failRelayUnavailable(runID, executionErr)
		return protocol.TokenUsage{}, relayExecutionSummary{}, false
	}
	return usage, execution, true
}

// handleRelayPreflight lets an off-netns caller verify the locked model relay is
// reachable on the SAME path a scored run uses. The subnet validator reaches this
// scorer at sandbox-docker:8000 but is NOT in sandbox-docker's network namespace,
// so it cannot itself resolve host.docker.internal — its own embed preflight hits
// a service name and therefore stays green even when the scorer's
// host.docker.internal:11435 relay path is broken (e.g. a managed stack whose
// compose predates the extra_hosts-on-sandbox-docker fix). This endpoint runs the
// same relay-health read the scored path runs at run start, inside this container's
// shared netns, so a mis-wired stack fails the validator's preflight and claims no
// ticket instead of leasing tickets it will fast-fail. Cheap: a relay /health GET,
// no provider completion — provider degradation is caught per-run, not here. The
// failure envelope mirrors the scored path's classification (model_relay_unavailable)
// so the caller maps it to a validator-infrastructure back-off.
func (s *server) handleRelayPreflight(w http.ResponseWriter, r *http.Request) {
	gateway := envOr("HARNESS_GATEWAY_URL", "http://host.docker.internal:11434")
	snapshot, err := readRelayHealth(r.Context(), gateway)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
			"failure": map[string]any{
				"kind":      "validator_infrastructure",
				"code":      "model_relay_unavailable",
				"retryable": true,
			},
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"provider":         snapshot.Provider,
		"model":            snapshot.Model,
		"profile_revision": snapshot.ProfileRevision,
	})
}

func (s *server) failRelayUnavailable(runID string, err error) {
	s.store.FailWith(runID, "locked model relay unavailable: "+err.Error(), &store.Failure{
		Kind:      "validator_infrastructure",
		Code:      "model_relay_unavailable",
		Retryable: true,
	})
}

func trustedEmbeddingInfrastructureFailure(err error) *store.Failure {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"embedding service unavailable",
		"embedding platform returned",
		"ticket embedding probe failed",
	} {
		if strings.Contains(message, marker) {
			return &store.Failure{
				Kind:      "validator_infrastructure",
				Code:      "embedding_provider_unavailable",
				Retryable: true,
			}
		}
	}
	return nil
}

func (s *server) failV7Seeding(runID, message string, err error) {
	failure := trustedEmbeddingInfrastructureFailure(err)
	s.store.FailWith(runID, message+err.Error(), failure)
}
