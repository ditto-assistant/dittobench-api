package tokenmodel

import (
	"crypto/sha256"
	"fmt"
	"math"
	"sort"

	"github.com/ditto-assistant/dittobench-api/internal/efficiency"
	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/gen"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// MethodAnchoredV1 is the derivation method id recorded in provenance:
// per-seed offline predictions, nearest-rank p90 over the predicted totals,
// corrected by the fit corpus's measured-vs-predicted anchors.
const MethodAnchoredV1 = "tokenmodel-anchored-v1"

// DerivedAggregation marks a derived baseline so it can never be mistaken for
// a measured nearest_rank_p90 baseline (validBaseline rejects it, so a
// derived manifest cannot silently satisfy any measured-only gate).
const DerivedAggregation = "nearest_rank_p90_derived"

const knownVectorSeed int64 = 123456789

// DeriveManifest derives a token-baseline manifest for targetBenchVersion
// from a REVIEWED-MEASURED source manifest, entirely offline: it regenerates
// the target datasets for the source's pinned (run_size, seed) grid, predicts
// each run's chat tokens with the fitted model, and anchors the nearest-rank
// p90 of the predictions with the fit corpus's measured correction.
//
// Fail-closed preconditions (each is a validity condition of the fitted
// model, not a formality):
//
//   - the source manifest is schema-2, scoring-enabled, reviewed-measured
//     (no derivation provenance), on the exact route/model identity the model
//     was fit for;
//   - the target bench version locks the same harness model;
//   - every regenerated target dataset lies inside the model's interpolation
//     envelope (composition ratios within the fit corpus's observed range).
//
// The derived manifest is emitted with ScoringEnabled=false and no smoke
// record: it is a research artifact until a smoke pass and an explicit
// platform policy decision. Nothing in v7 scoring consumes it.
func DeriveManifest(source efficiency.Manifest, sourceRaw []byte, targetBenchVersion int) (efficiency.Manifest, error) {
	return Production.DeriveManifest(source, sourceRaw, targetBenchVersion)
}

// DeriveManifest on an explicit Model exists so tests (and future refits) can
// exercise the machinery with adjusted parameters; production callers use the
// package-level wrapper bound to the frozen Production fit.
func (m Model) DeriveManifest(source efficiency.Manifest, sourceRaw []byte, targetBenchVersion int) (efficiency.Manifest, error) {
	if source.SchemaVersion != 2 || source.FormulaVersion != efficiency.FormulaVersion {
		return efficiency.Manifest{}, fmt.Errorf("source manifest has an unknown schema/formula contract")
	}
	if !source.ScoringEnabled {
		return efficiency.Manifest{}, fmt.Errorf("source manifest is not a reviewed, scoring-enabled manifest")
	}
	if source.Derived != nil {
		return efficiency.Manifest{}, fmt.Errorf("source manifest is itself derived; transfer requires reviewed-measured provenance")
	}
	if len(source.Baselines) == 0 || len(source.Calibration) == 0 {
		return efficiency.Manifest{}, fmt.Errorf("source manifest carries no baselines/calibration grid")
	}
	for _, b := range source.Baselines {
		if b.Model != m.HarnessModel || b.ProfileRevision != m.RouteProfile {
			return efficiency.Manifest{}, fmt.Errorf(
				"precondition failed: source baseline %s/%s/%s is outside the fitted identity (%s @ %s); run a full calibration",
				b.Provider, b.ProfileRevision, b.Model, m.HarnessModel, m.RouteProfile)
		}
	}
	if !protocol.SupportedBenchVersion(targetBenchVersion) || targetBenchVersion < protocol.BenchVersionV5 {
		return efficiency.Manifest{}, fmt.Errorf("unsupported target bench version %d", targetBenchVersion)
	}
	if got := llm.HarnessModelForVersion(targetBenchVersion); got != m.HarnessModel {
		return efficiency.Manifest{}, fmt.Errorf(
			"precondition failed: bench v%d locks harness model %s but the token model was fit for %s; run a full calibration",
			targetBenchVersion, got, m.HarnessModel)
	}

	derived := efficiency.Manifest{
		SchemaVersion:      source.SchemaVersion,
		FormulaVersion:     source.FormulaVersion,
		BenchVersion:       targetBenchVersion,
		ScoringEnabled:     false,
		StarterKitRevision: source.StarterKitRevision,
	}
	knownProfile, ok := gen.ProfileForVersion("full", targetBenchVersion)
	if !ok {
		return efficiency.Manifest{}, fmt.Errorf("v%d full profile unavailable", targetBenchVersion)
	}
	known, err := gen.GenerateDataset(knownVectorSeed, knownProfile, targetBenchVersion)
	if err != nil {
		return efficiency.Manifest{}, fmt.Errorf("generate known vector: %w", err)
	}
	derived.DatasetKnownVector, _, err = known.SHA256Hex()
	if err != nil {
		return efficiency.Manifest{}, fmt.Errorf("hash known vector: %w", err)
	}

	type seedPrediction struct {
		seed          int64
		prompt, compl float64
	}
	bySize := map[string][]seedPrediction{}
	for _, dataset := range source.Calibration {
		profile, ok := gen.ProfileForVersion(dataset.RunSize, targetBenchVersion)
		if !ok {
			return efficiency.Manifest{}, fmt.Errorf("unknown run size %q", dataset.RunSize)
		}
		artifact, err := gen.GenerateDataset(dataset.Seed, profile, targetBenchVersion)
		if err != nil {
			return efficiency.Manifest{}, fmt.Errorf("generate %s seed %d: %w", dataset.RunSize, dataset.Seed, err)
		}
		hash, _, err := artifact.SHA256Hex()
		if err != nil {
			return efficiency.Manifest{}, fmt.Errorf("hash %s seed %d: %w", dataset.RunSize, dataset.Seed, err)
		}
		f := Extract(artifact)
		if violations := m.EnvelopeViolations(f); len(violations) > 0 {
			return efficiency.Manifest{}, fmt.Errorf(
				"target dataset %s/%d is outside the model's interpolation envelope (%v); run a full calibration",
				dataset.RunSize, dataset.Seed, violations)
		}
		derived.Calibration = append(derived.Calibration, efficiency.CalibrationDataset{
			RunSize: dataset.RunSize, Seed: dataset.Seed, DatasetSHA256: hash,
		})
		bySize[dataset.RunSize] = append(bySize[dataset.RunSize], seedPrediction{
			seed:   dataset.Seed,
			prompt: m.PredictPromptTokens(f),
			compl:  m.PredictCompletionTokens(f),
		})
	}

	factors := map[string]efficiency.TransferFactor{}
	for _, sourceBaseline := range source.Baselines {
		preds := bySize[sourceBaseline.RunSize]
		if len(preds) == 0 {
			return efficiency.Manifest{}, fmt.Errorf("no calibration datasets for run size %q", sourceBaseline.RunSize)
		}
		anchor, ok := m.Anchors[sourceBaseline.RunSize]
		if !ok {
			return efficiency.Manifest{}, fmt.Errorf("no anchor for run size %q; run a full calibration", sourceBaseline.RunSize)
		}
		// Mirror the measured pipeline's ordering (total, prompt, completion,
		// seed) over the PREDICTED values, then take the nearest-rank p90.
		sort.Slice(preds, func(i, j int) bool {
			ti, tj := preds[i].prompt+preds[i].compl, preds[j].prompt+preds[j].compl
			if ti != tj {
				return ti < tj
			}
			if preds[i].prompt != preds[j].prompt {
				return preds[i].prompt < preds[j].prompt
			}
			if preds[i].compl != preds[j].compl {
				return preds[i].compl < preds[j].compl
			}
			return preds[i].seed < preds[j].seed
		})
		p90 := preds[int(math.Ceil(efficiency.BudgetPercentile*float64(len(preds))))-1]
		promptTokens := uint64(math.Round(p90.prompt * anchor.Prompt))
		completionTokens := uint64(math.Round(p90.compl * anchor.Completion))
		b := efficiency.Baseline{
			BenchVersion:       targetBenchVersion,
			RunSize:            sourceBaseline.RunSize,
			Provider:           sourceBaseline.Provider,
			ProfileRevision:    sourceBaseline.ProfileRevision,
			Model:              sourceBaseline.Model,
			PromptTokens:       promptTokens,
			CompletionTokens:   completionTokens,
			TotalTokens:        promptTokens + completionTokens,
			Samples:            sourceBaseline.Samples,
			Aggregation:        DerivedAggregation,
			StarterKitRevision: source.StarterKitRevision,
		}
		b.ID = efficiency.BaselineID(targetBenchVersion, b)
		derived.Baselines = append(derived.Baselines, b)
		factors[sourceBaseline.RunSize] = efficiency.TransferFactor{
			Prompt: anchor.Prompt, Completion: anchor.Completion,
		}
	}
	sort.Slice(derived.Baselines, func(i, j int) bool {
		a, b := derived.Baselines[i], derived.Baselines[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.ProfileRevision != b.ProfileRevision {
			return a.ProfileRevision < b.ProfileRevision
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.RunSize < b.RunSize
	})

	sourceSum := sha256.Sum256(sourceRaw)
	derived.Derived = &efficiency.Derivation{
		Method:               MethodAnchoredV1,
		ModelVersion:         m.Version,
		SourceManifestSHA256: fmt.Sprintf("%x", sourceSum[:]),
		SourceBenchVersion:   source.BenchVersion,
		FitCorpusSHA256:      m.FitCorpusSHA256,
		FitRuns:              m.FitRuns,
		FitResidualMeanPct:   fracMapToPct(m.ResidualMeanFrac),
		FitResidualMaxPct:    fracMapToPct(m.ResidualMaxFrac),
		TransferP90CVMaxPct:  fracMapToPct(m.TransferP90CVMaxFrac),
		TransferFactors:      factors,
	}
	return derived, nil
}

func fracMapToPct(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = math.Round(v*10000) / 100 // two-decimal percent
	}
	return out
}
