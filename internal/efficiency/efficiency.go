// Package efficiency implements the versioned DittoBench v5 token transform.
// It consumes only validator-observed model-proxy telemetry and keeps the raw
// quality score separate from the adjusted score.
package efficiency

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

const (
	FormulaVersion   = "v5-relay-token-waste-p90-v1"
	BudgetPercentile = 0.90
	MaximumPenalty   = 0.10
	MinMultiplier    = 1 - MaximumPenalty
)

type Baseline struct {
	ID                 string `json:"id"`
	BenchVersion       int    `json:"bench_version"`
	RunSize            string `json:"run_size"`
	Provider           string `json:"provider"`
	ProfileRevision    string `json:"profile_revision"`
	Model              string `json:"model"`
	PromptTokens       uint64 `json:"prompt_tokens"`
	CompletionTokens   uint64 `json:"completion_tokens"`
	TotalTokens        uint64 `json:"total_tokens"`
	Samples            int    `json:"samples"`
	Aggregation        string `json:"aggregation"`
	StarterKitRevision string `json:"starter_kit_revision"`
}

type CalibrationDataset struct {
	RunSize       string `json:"run_size"`
	Seed          int64  `json:"seed"`
	DatasetSHA256 string `json:"dataset_sha256"`
}

type Manifest struct {
	SchemaVersion      int                  `json:"schema_version"`
	FormulaVersion     string               `json:"formula_version"`
	BenchVersion       int                  `json:"bench_version"`
	ScoringEnabled     bool                 `json:"scoring_enabled"`
	DatasetKnownVector string               `json:"dataset_known_vector"`
	StarterKitRevision string               `json:"starter_kit_revision"`
	Calibration        []CalibrationDataset `json:"calibration_datasets"`
	Baselines          []Baseline           `json:"baselines"`
}

//go:embed baselines_v5.json
var manifestJSON []byte

var productionManifest = mustManifest(manifestJSON)

func mustManifest(body []byte) Manifest {
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		panic(fmt.Sprintf("invalid embedded token baseline manifest: %v", err))
	}
	if manifest.SchemaVersion != 2 || manifest.FormulaVersion != FormulaVersion || manifest.BenchVersion != protocol.BenchVersionV5 {
		panic("embedded token baseline manifest has incompatible contract metadata")
	}
	return manifest
}

func ManifestSnapshot() Manifest {
	copy := productionManifest
	copy.Calibration = append([]CalibrationDataset(nil), productionManifest.Calibration...)
	copy.Baselines = append([]Baseline(nil), productionManifest.Baselines...)
	return copy
}

// ProductionReady is true only after reviewed baselines exist for every run
// size on both certified provider profiles. Until then v5 remains hidden from
// capability negotiation and any direct v5 report fails neutral.
func ProductionReady() bool {
	return ReadyForProduction(productionManifest)
}

// ProductionReadyForVersion keeps execution support separate from capability
// advertisement. v7 remains dark until its GPT-OSS starter-kit manifest is
// reviewed and embedded; v5/v6 retain the existing Qwen calibration.
func ProductionReadyForVersion(benchVersion int) bool {
	switch benchVersion {
	case protocol.BenchVersionV5, protocol.BenchVersionV6:
		return ProductionReady()
	default:
		return false
	}
}

// ReadyForProduction validates a candidate manifest using the same gate as the
// embedded production manifest. Calibration tooling uses this before it can
// emit an explicitly enabled phase-B candidate.
func ReadyForProduction(manifest Manifest) bool {
	if !manifest.ScoringEnabled {
		return false
	}
	required := []struct {
		provider string
		revision string
		model    string
	}{
		{"chutes", llm.ChutesRelayProfileRevision, llm.LockedUpstreamModel},
		{"openrouter", llm.OpenRouterRelayProfileRevision, llm.LockedHarnessModel},
	}
	for _, contract := range required {
		for _, runSize := range []string{"small", "medium", "full"} {
			found := false
			for _, b := range manifest.Baselines {
				if b.BenchVersion == protocol.BenchVersionV5 && b.Provider == contract.provider &&
					b.ProfileRevision == contract.revision && b.Model == contract.model && b.RunSize == runSize &&
					b.StarterKitRevision == manifest.StarterKitRevision && validBaseline(b) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

// ReadyForV7Production validates the single-provider-per-run GPT-OSS
// calibration contract. Provider eligibility is dynamic on the platform, but
// every route must carry this exact immutable profile and its own 20-sample p90
// for each run size before it may serve a scored ticket.
func ReadyForV7Production(manifest Manifest) bool {
	if !manifest.ScoringEnabled || manifest.BenchVersion != protocol.BenchVersionV7 ||
		manifest.StarterKitRevision == "" || len(manifest.Calibration) != 60 {
		return false
	}
	groups := map[string]bool{}
	for _, b := range manifest.Baselines {
		if b.BenchVersion == protocol.BenchVersionV7 && b.Provider != "" &&
			b.Model == llm.V7HarnessModel && b.StarterKitRevision == manifest.StarterKitRevision &&
			b.ProfileRevision != "" && b.RunSize != "" && validBaseline(b) {
			groups[b.Provider+":"+b.ProfileRevision+":"+b.RunSize] = true
		}
	}
	if len(manifest.Baselines) == 0 || len(manifest.Baselines)%3 != 0 {
		return false
	}
	profiles := map[string]bool{}
	for _, b := range manifest.Baselines {
		profiles[b.Provider+":"+b.ProfileRevision] = true
	}
	for profile := range profiles {
		for _, runSize := range []string{"small", "medium", "full"} {
			if !groups[profile+":"+runSize] {
				return false
			}
		}
	}
	return true
}

func Lookup(runSize string, usage protocol.TokenUsage) (Baseline, bool) {
	return LookupForVersion(protocol.BenchVersionV5, runSize, usage)
}

// LookupForVersion refuses to compare v7 GPT-OSS usage with the historical
// Qwen manifest. A direct pre-rollout v7 run therefore scores neutrally until
// the separately reviewed v7 calibration lands.
func LookupForVersion(benchVersion int, runSize string, usage protocol.TokenUsage) (Baseline, bool) {
	if !productionManifest.ScoringEnabled {
		return Baseline{}, false
	}
	baselineVersion := benchVersion
	if benchVersion == protocol.BenchVersionV6 {
		baselineVersion = protocol.BenchVersionV5
	}
	for _, b := range productionManifest.Baselines {
		if b.BenchVersion == baselineVersion && b.RunSize == runSize &&
			b.Provider == usage.Provider && b.ProfileRevision == usage.ProfileRevision &&
			b.Model == usage.Model && b.StarterKitRevision == productionManifest.StarterKitRevision && validBaseline(b) {
			return b, true
		}
	}
	return Baseline{}, false
}

func validBaseline(b Baseline) bool {
	return b.ID != "" && b.Samples >= 20 && b.Aggregation == "nearest_rank_p90" &&
		b.PromptTokens > 0 && b.CompletionTokens > 0 &&
		b.TotalTokens == b.PromptTokens+b.CompletionTokens && b.StarterKitRevision != ""
}

// Apply returns the complete audit record and mutates no report. Callers assign
// AdjustedComposite to ScoreReport.Composite only for bench v5.
func Apply(raw protocol.ScoreReport, usage protocol.TokenUsage, baseline *Baseline) protocol.TokenEfficiency {
	result := protocol.TokenEfficiency{
		FormulaVersion:           FormulaVersion,
		BudgetPercentile:         BudgetPercentile,
		ObservedPromptTokens:     usage.PromptTokens,
		ObservedCompletionTokens: usage.CompletionTokens,
		ObservedTotalTokens:      usage.TotalTokens,
		MaximumPenalty:           MaximumPenalty,
		MinimumMultiplier:        MinMultiplier,
		Multiplier:               1,
		RawComposite:             raw.Composite,
		AdjustedComposite:        raw.Composite,
		DecisionReason:           "within_budget",
	}

	if !ValidUsage(usage) {
		result.DecisionReason = "token_telemetry_unavailable"
		return result
	}
	if baseline == nil || !validBaseline(*baseline) {
		result.DecisionReason = "baseline_unavailable"
		return result
	}
	result.BaselineID = baseline.ID
	result.BaselinePromptTokens = baseline.PromptTokens
	result.BaselineCompletionTokens = baseline.CompletionTokens
	result.BaselineTotalTokens = baseline.TotalTokens
	if usage.TotalTokens <= baseline.TotalTokens {
		return result
	}

	ratio := float64(usage.TotalTokens) / float64(baseline.TotalTokens)
	result.ExcessRatio = ratio - 1
	// A one-sided rational curve is neutral through the generous p90 budget,
	// then approaches (but never crosses) the 0.90 floor as waste grows.
	result.Multiplier = 1 - MaximumPenalty*(result.ExcessRatio/(1+result.ExcessRatio))
	if math.IsNaN(result.Multiplier) || math.IsInf(result.Multiplier, 0) {
		result.Multiplier = 1
		result.ExcessRatio = 0
		result.DecisionReason = "token_transform_unavailable"
		return result
	}
	result.Multiplier = math.Max(MinMultiplier, result.Multiplier)
	result.PenaltyApplied = true
	result.DecisionReason = "above_budget"
	result.AdjustedComposite = round6(raw.Composite * result.Multiplier)
	return result
}

// ValidUsage applies the same completeness and arithmetic checks to scoring
// and starter-kit calibration inputs.
func ValidUsage(usage protocol.TokenUsage) bool {
	if usage.Status != "complete" || usage.Successes == 0 ||
		usage.UsageAvailable != usage.Successes || usage.UsageUnavailable != 0 ||
		usage.TotalTokens == 0 || usage.PromptTokens == 0 ||
		usage.PromptTokens > ^uint64(0)-usage.CompletionTokens {
		return false
	}
	return usage.TotalTokens == usage.PromptTokens+usage.CompletionTokens
}

func round6(value float64) float64 { return math.Round(value*1e6) / 1e6 }
