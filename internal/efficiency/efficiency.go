// Package efficiency implements the versioned DittoBench v5 token transform.
// It consumes only validator-observed model-proxy telemetry and keeps the raw
// quality score separate from the adjusted score.
package efficiency

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

const (
	FormulaVersion          = "v5-prompt-dominant-asymmetric-power-v1"
	PromptTokenWeight       = 1.0
	CompletionTokenWeight   = 0.25
	RewardExponent          = 0.25
	PenaltyExponent         = 0.50
	MinMultiplier           = 0.75
	MinimumComposite        = 0.80
	MinimumToolMean         = 0.70
	MinimumMemoryMean       = 0.70
	MinimumResponseCoverage = 0.95
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
	if manifest.SchemaVersion != 1 || manifest.FormulaVersion != FormulaVersion || manifest.BenchVersion != protocol.BenchVersionV5 {
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
			for _, b := range productionManifest.Baselines {
				if b.BenchVersion == protocol.BenchVersionV5 && b.Provider == contract.provider &&
					b.ProfileRevision == contract.revision && b.Model == contract.model && b.RunSize == runSize &&
					b.StarterKitRevision == productionManifest.StarterKitRevision && validBaseline(b) {
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

func Lookup(runSize string, usage protocol.TokenUsage) (Baseline, bool) {
	for _, b := range productionManifest.Baselines {
		if b.BenchVersion == protocol.BenchVersionV5 && b.RunSize == runSize &&
			b.Provider == usage.Provider && b.ProfileRevision == usage.ProfileRevision &&
			b.Model == usage.Model && b.StarterKitRevision == productionManifest.StarterKitRevision && validBaseline(b) {
			return b, true
		}
	}
	return Baseline{}, false
}

func validBaseline(b Baseline) bool {
	return b.ID != "" && b.Samples >= 3 && b.Aggregation == "median" &&
		b.PromptTokens > 0 && b.CompletionTokens > 0 &&
		b.TotalTokens == b.PromptTokens+b.CompletionTokens && b.StarterKitRevision != ""
}

// ResponseCoverage is the share of benchmark cases that produced a visible
// user-facing answer. Miner-reported token fields are intentionally ignored.
func ResponseCoverage(transcripts []protocol.RunResponse) float64 {
	if len(transcripts) == 0 {
		return 0
	}
	visible := 0
	for _, response := range transcripts {
		if strings.TrimSpace(response.FinalText) != "" || strings.TrimSpace(response.Answer) != "" {
			visible++
		}
	}
	return float64(visible) / float64(len(transcripts))
}

// Apply returns the complete audit record and mutates no report. Callers assign
// AdjustedComposite to ScoreReport.Composite only for bench v5.
func Apply(raw protocol.ScoreReport, usage protocol.TokenUsage, baseline *Baseline, responseCoverage float64) protocol.TokenEfficiency {
	result := protocol.TokenEfficiency{
		FormulaVersion:           FormulaVersion,
		ObservedPromptTokens:     usage.PromptTokens,
		ObservedCompletionTokens: usage.CompletionTokens,
		ObservedTotalTokens:      usage.TotalTokens,
		ObservedWeightedTokens:   WeightedTokens(usage.PromptTokens, usage.CompletionTokens),
		PromptTokenWeight:        PromptTokenWeight,
		CompletionTokenWeight:    CompletionTokenWeight,
		RewardExponent:           RewardExponent,
		PenaltyExponent:          PenaltyExponent,
		MinimumMultiplier:        MinMultiplier,
		Multiplier:               1,
		RawComposite:             raw.Composite,
		AdjustedComposite:        raw.Composite,
		MinimumComposite:         MinimumComposite,
		MinimumToolMean:          MinimumToolMean,
		MinimumMemoryMean:        MinimumMemoryMean,
		MinimumResponseCoverage:  MinimumResponseCoverage,
		ResponseCoverage:         responseCoverage,
	}

	switch {
	case raw.Composite < MinimumComposite:
		result.EligibilityReason = "raw_composite_below_floor"
	case raw.ToolMean < MinimumToolMean:
		result.EligibilityReason = "tool_mean_below_floor"
	case raw.MemoryMean < MinimumMemoryMean:
		result.EligibilityReason = "memory_mean_below_floor"
	case responseCoverage < MinimumResponseCoverage:
		result.EligibilityReason = "response_coverage_below_floor"
	default:
		result.QualityEligible = true
		result.EligibilityReason = "eligible"
	}
	if !ValidUsage(usage) {
		if result.QualityEligible {
			result.EligibilityReason = "token_telemetry_unavailable"
		}
		return result
	}
	if baseline == nil || !validBaseline(*baseline) {
		if result.QualityEligible {
			result.EligibilityReason = "baseline_unavailable"
		}
		return result
	}
	result.BaselineID = baseline.ID
	result.BaselinePromptTokens = baseline.PromptTokens
	result.BaselineCompletionTokens = baseline.CompletionTokens
	result.BaselineTotalTokens = baseline.TotalTokens
	result.BaselineWeightedTokens = WeightedTokens(baseline.PromptTokens, baseline.CompletionTokens)

	ratio := result.BaselineWeightedTokens / result.ObservedWeightedTokens
	if ratio >= 1 {
		if !result.QualityEligible {
			// Efficiency cannot rescue low-quality or incomplete answers.
			return result
		}
		// Below baseline, keep rewarding genuine prompt-efficiency improvements.
		// The fourth root is deliberately unbounded but grows slowly.
		result.Multiplier = math.Pow(ratio, RewardExponent)
	} else {
		// Above baseline, retain a bounded penalty for every report, including
		// low-quality ones, so context stuffing never escapes its cost.
		result.Multiplier = math.Max(MinMultiplier, math.Pow(ratio, PenaltyExponent))
	}
	if math.IsNaN(result.Multiplier) || math.IsInf(result.Multiplier, 0) {
		result.Multiplier = 1
		result.EligibilityReason = "token_transform_unavailable"
		return result
	}
	result.AdjustedComposite = round6(raw.Composite * result.Multiplier)
	return result
}

// WeightedTokens is the prompt-dominant v5 cost. Completion tokens retain a
// small cost without making normal helpful prose the main optimization target.
func WeightedTokens(prompt, completion uint64) float64 {
	return PromptTokenWeight*float64(prompt) + CompletionTokenWeight*float64(completion)
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
