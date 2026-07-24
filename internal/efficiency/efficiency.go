// Package efficiency implements the versioned DittoBench v5 token transform.
// It consumes only validator-observed model-proxy telemetry and keeps the raw
// quality score separate from the adjusted score.
package efficiency

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

const (
	FormulaVersion   = "v5-relay-token-waste-p90-v1"
	BudgetPercentile = 0.90
	MaximumPenalty   = 0.10
	MinMultiplier    = 1 - MaximumPenalty
)

// v7 strict token transform. The v7 difficulty release (~10x harder datagen
// suite) keeps the reviewed aggregate GPT-OSS manifest as its calibration
// identity anchor (ReadyForV7Production and the manifest digests are
// unchanged), and layers two version-gated changes on top:
//
//   - V7TokenBudgetScale multiplies the manifest's p90 total-token baseline
//     into the effective v7 budget. The embedded baselines were measured on
//     the pre-hardening v7 datasets; the hardened suite legitimately needs
//     more tokens, so the budget is scaled rather than silently taxing every
//     run. The scale is sized from the measured v6 → v7-hard dataset growth
//     (datagen v7-difficulty, avg over seeds 123456789/101/987654321):
//     case count ×1.0–1.25, per-case prompt bytes ×1.4–1.6, haystack pair
//     bytes ×1.0–1.2, plus the new dependent link-chain / recovery categories
//     adding one extra tool hop on a minority of tool cases. Compounded,
//     expected starter-kit p90 growth is well under 2x, so 2 keeps honest
//     headroom while preserving the penalty's bite. The scale is an explicit,
//     documented interim constant: it MUST be replaced by a true recalibrated
//     manifest (cmd/tokenbaseline against the hardened datagen release)
//     before platform rollout. See docs/token-efficiency-v7.md.
//   - Waste beyond the scaled budget is penalized three times deeper
//     (30% max vs 10%), so brute-force strategies — re-reading the whole
//     haystack per question, unbounded self-consistency sampling — lose
//     roughly a third of their composite instead of shrugging off a 10% cap.
//
// The transform stays one-sided and saturating; cheap answers still earn no
// reward. Reported in-band via FormulaVersion/MaximumPenalty on the
// TokenEfficiency record, so a consumer can always tell which contract
// produced a report.
const (
	V7FormulaVersion   = "v7-relay-token-waste-p90-strict-v1"
	V7MaximumPenalty   = 0.30
	V7MinMultiplier    = 1 - V7MaximumPenalty
	V7TokenBudgetScale = 2
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

// CalibrationRouteIdentity is the exact provider route contract covered by a
// reviewed token-baseline manifest. Consumers must intersect all three fields;
// a provider name alone is not a calibrated route identity.
type CalibrationRouteIdentity struct {
	Provider        string `json:"provider"`
	ProfileRevision string `json:"profile_revision"`
	Model           string `json:"model"`
}

// CalibrationReadiness is the public identity of a reviewed baseline
// manifest. An empty digest and route set means no reviewed v7 manifest is
// embedded and v7 must remain unavailable.
type CalibrationReadiness struct {
	ManifestSHA256  string                     `json:"manifest_sha256"`
	SupportedRoutes []CalibrationRouteIdentity `json:"supported_routes"`
}

//go:embed baselines_v5.json
var manifestJSON []byte

//go:embed baselines_v7.json
var manifestV7JSON []byte

var productionManifest = mustManifest(manifestJSON, protocol.BenchVersionV5)
var productionV7Manifest = mustManifest(manifestV7JSON, protocol.BenchVersionV7)

func mustManifest(body []byte, benchVersion int) Manifest {
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		panic(fmt.Sprintf("invalid embedded token baseline manifest: %v", err))
	}
	if manifest.SchemaVersion != 2 || manifest.FormulaVersion != FormulaVersion || manifest.BenchVersion != benchVersion {
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

func V7ManifestSnapshot() Manifest {
	copy := productionV7Manifest
	copy.Calibration = append([]CalibrationDataset(nil), productionV7Manifest.Calibration...)
	copy.Baselines = append([]Baseline(nil), productionV7Manifest.Baselines...)
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
	case protocol.BenchVersionV7:
		return ReadyForV7Production(productionV7Manifest)
	default:
		return false
	}
}

// V7CalibrationReadiness returns only identities proven by the embedded,
// reviewed v7 manifest. There is deliberately no candidate/smoke fallback.
func V7CalibrationReadiness() CalibrationReadiness {
	return v7CalibrationReadiness(productionV7Manifest, manifestV7JSON)
}

func v7CalibrationReadiness(manifest Manifest, rawManifest []byte) CalibrationReadiness {
	readiness := CalibrationReadiness{SupportedRoutes: []CalibrationRouteIdentity{}}
	if len(rawManifest) == 0 || !ReadyForV7Production(manifest) {
		return readiness
	}
	seen := map[CalibrationRouteIdentity]bool{}
	for _, baseline := range manifest.Baselines {
		seen[CalibrationRouteIdentity{
			Provider:        baseline.Provider,
			ProfileRevision: baseline.ProfileRevision,
			Model:           baseline.Model,
		}] = true
	}
	for route := range seen {
		readiness.SupportedRoutes = append(readiness.SupportedRoutes, route)
	}
	sort.Slice(readiness.SupportedRoutes, func(i, j int) bool {
		a, b := readiness.SupportedRoutes[i], readiness.SupportedRoutes[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.ProfileRevision != b.ProfileRevision {
			return a.ProfileRevision < b.ProfileRevision
		}
		return a.Model < b.Model
	})
	sum := sha256.Sum256(rawManifest)
	readiness.ManifestSHA256 = fmt.Sprintf("%x", sum[:])
	return readiness
}

func ValidV7CalibrationReadiness(readiness CalibrationReadiness) bool {
	if !canonicalSHA256(readiness.ManifestSHA256) || len(readiness.SupportedRoutes) != 1 {
		return false
	}
	return readiness.SupportedRoutes[0] == (CalibrationRouteIdentity{
		Provider: v7AggregateProvider, ProfileRevision: v7AggregateProfile,
		Model: llm.V7HarnessModel,
	})
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

const (
	v7AggregateProvider        = "openrouter"
	v7AggregateProfile         = "openrouter-route-a471cd87ae7df5b9-v1"
	v7StarterKitRevision       = "2ec9029568f20015562193a378eb8bce51191470"
	v7DatasetKnownVector       = "1cfc6e3b9f3f4c04afe04b058a6851f9357f6463170b879867e2cf4588f58fcf"
	v7CalibrationDatasetDigest = "11067ee51dd87582af939f26631cfc075588a13f708a726a3ebae1199dcabfa8"
)

// ReadyForV7Production validates the initial aggregate GPT-OSS calibration
// contract. Provider-specific manifests remain dark until adaptive routing is
// separately reviewed and enabled.
func ReadyForV7Production(manifest Manifest) bool {
	if !manifest.ScoringEnabled || manifest.BenchVersion != protocol.BenchVersionV7 ||
		manifest.StarterKitRevision != v7StarterKitRevision ||
		manifest.DatasetKnownVector != v7DatasetKnownVector ||
		calibrationDatasetDigest(manifest.Calibration) != v7CalibrationDatasetDigest ||
		!validV7CalibrationDatasets(manifest.Calibration) {
		return false
	}
	groups := map[string]int{}
	profiles := map[string]bool{}
	for _, b := range manifest.Baselines {
		if b.BenchVersion != protocol.BenchVersionV7 || b.Provider != v7AggregateProvider ||
			b.Model != llm.V7HarnessModel || b.StarterKitRevision != manifest.StarterKitRevision ||
			b.ProfileRevision != v7AggregateProfile || b.Samples != 20 || !validBaseline(b) ||
			b.ID != baselineID(protocol.BenchVersionV7, b) {
			return false
		}
		profile := b.Provider + ":" + b.ProfileRevision
		profiles[profile] = true
		groups[profile+":"+b.RunSize]++
	}
	if len(profiles) != 1 || len(manifest.Baselines) != 3 {
		return false
	}
	for profile := range profiles {
		for _, runSize := range []string{"small", "medium", "full"} {
			if groups[profile+":"+runSize] != 1 {
				return false
			}
		}
	}
	return true
}

func canonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func calibrationDatasetDigest(datasets []CalibrationDataset) string {
	h := sha256.New()
	for _, dataset := range datasets {
		_, _ = fmt.Fprintf(h, "%s:%d:%s\n", dataset.RunSize, dataset.Seed, dataset.DatasetSHA256)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func baselineID(benchVersion int, b Baseline) string {
	canonical := fmt.Sprintf("%s:%s:%s:%s:%s:%d:%d:%d:%s", FormulaVersion, b.RunSize, b.Provider,
		b.ProfileRevision, b.Model, b.PromptTokens, b.CompletionTokens, b.TotalTokens, b.StarterKitRevision)
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("v%d-starter-p90-%x", benchVersion, sum[:8])
}

func validV7CalibrationDatasets(datasets []CalibrationDataset) bool {
	if len(datasets) != 60 {
		return false
	}
	counts := map[string]int{}
	seenSeeds := map[string]bool{}
	for _, dataset := range datasets {
		if dataset.RunSize != "small" && dataset.RunSize != "medium" && dataset.RunSize != "full" {
			return false
		}
		if len(dataset.DatasetSHA256) != sha256.Size*2 {
			return false
		}
		for _, r := range dataset.DatasetSHA256 {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return false
			}
		}
		seedKey := fmt.Sprintf("%s:%d", dataset.RunSize, dataset.Seed)
		if seenSeeds[seedKey] {
			return false
		}
		seenSeeds[seedKey] = true
		counts[dataset.RunSize]++
	}
	return counts["small"] == 20 && counts["medium"] == 20 && counts["full"] == 20
}

func Lookup(runSize string, usage protocol.TokenUsage) (Baseline, bool) {
	return LookupForVersion(protocol.BenchVersionV5, runSize, usage)
}

// LookupForVersion selects only the reviewed manifest for the requested model
// epoch. v6 intentionally retains the historical v5 Qwen calibration.
func LookupForVersion(benchVersion int, runSize string, usage protocol.TokenUsage) (Baseline, bool) {
	manifest := productionManifest
	baselineVersion := benchVersion
	switch benchVersion {
	case protocol.BenchVersionV5:
	case protocol.BenchVersionV6:
		baselineVersion = protocol.BenchVersionV5
	case protocol.BenchVersionV7:
		manifest = productionV7Manifest
	default:
		return Baseline{}, false
	}
	if !ProductionReadyForVersion(benchVersion) {
		return Baseline{}, false
	}
	for _, b := range manifest.Baselines {
		if b.BenchVersion == baselineVersion && b.RunSize == runSize &&
			b.Provider == usage.Provider && b.ProfileRevision == usage.ProfileRevision &&
			b.Model == usage.Model && b.StarterKitRevision == manifest.StarterKitRevision && validBaseline(b) {
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
	return applyWith(raw, usage, baseline, FormulaVersion, MaximumPenalty, 1)
}

// ApplyForVersion selects the token transform for a bench version. v5/v6 are
// byte-identical to Apply; v7 applies the strict contract (scaled interim
// budget, 30% max penalty, its own formula version string). The budgetScale
// multiplies only the effective budget the excess ratio is computed against;
// the reported Baseline*Tokens fields stay the manifest's reviewed values so
// the calibration identity remains auditable.
func ApplyForVersion(benchVersion int, raw protocol.ScoreReport, usage protocol.TokenUsage, baseline *Baseline) protocol.TokenEfficiency {
	if benchVersion >= protocol.BenchVersionV7 {
		return applyWith(raw, usage, baseline, V7FormulaVersion, V7MaximumPenalty, V7TokenBudgetScale)
	}
	return Apply(raw, usage, baseline)
}

func applyWith(raw protocol.ScoreReport, usage protocol.TokenUsage, baseline *Baseline, formulaVersion string, maximumPenalty float64, budgetScale uint64) protocol.TokenEfficiency {
	result := protocol.TokenEfficiency{
		FormulaVersion:           formulaVersion,
		BudgetPercentile:         BudgetPercentile,
		ObservedPromptTokens:     usage.PromptTokens,
		ObservedCompletionTokens: usage.CompletionTokens,
		ObservedTotalTokens:      usage.TotalTokens,
		MaximumPenalty:           maximumPenalty,
		MinimumMultiplier:        1 - maximumPenalty,
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
	if budgetScale < 1 {
		budgetScale = 1
	}
	budget := baseline.TotalTokens * budgetScale
	if usage.TotalTokens <= budget {
		return result
	}

	ratio := float64(usage.TotalTokens) / float64(budget)
	result.ExcessRatio = ratio - 1
	// A one-sided rational curve is neutral through the generous p90 budget,
	// then approaches (but never crosses) the minimum-multiplier floor as waste
	// grows.
	result.Multiplier = 1 - maximumPenalty*(result.ExcessRatio/(1+result.ExcessRatio))
	if math.IsNaN(result.Multiplier) || math.IsInf(result.Multiplier, 0) {
		result.Multiplier = 1
		result.ExcessRatio = 0
		result.DecisionReason = "token_transform_unavailable"
		return result
	}
	result.Multiplier = math.Max(result.MinimumMultiplier, result.Multiplier)
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
