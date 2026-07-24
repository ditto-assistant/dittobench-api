package tokenmodel

import (
	"fmt"
	"math"
	"sort"

	"github.com/ditto-assistant/dittobench-api/internal/efficiency"
	"github.com/ditto-assistant/dittobench-datagen/gen"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// SmokeValidate checks a derived manifest against K live runs of the
// unmodified reference harness (recommended K: one small + one medium + one
// full, ~15–20 minutes) and returns the smoke record plus human-readable
// rejection reasons.
//
// Acceptance is FAIL-CLOSED on every axis:
//
//   - every run size in the derived manifest must be covered by at least one
//     report (a p90 tail cannot be validated per size otherwise);
//   - each report must be a complete proxy-metered run of a dataset pinned in
//     the derived calibration grid (seed + dataset_sha256 match, so the smoke
//     provably ran the target datasets), on the exact route/model identity;
//   - each run's measured chat total must land within the model's acceptance
//     band: |measured − predicted|/predicted ≤ tolerance × (1 − border zone).
//     A result inside the border zone is treated as INCONCLUSIVE and rejected.
//
// Any rejection means: run the full 60-run calibration campaign instead.
//
// Scope note: three runs reject gross drift (a template overhaul, a routing
// or tokenizer change shifts totals far outside these bands); they cannot
// certify the p90 tail by themselves. That is why acceptance additionally
// leans on the derivation-time interpolation-envelope check (per-run-size
// dataset-composition strata) and why the derived class still requires an
// explicit platform policy decision before any gate consumes it.
func SmokeValidate(derived efficiency.Manifest, reports []protocol.ScoreReport) (efficiency.SmokeRecord, []string, error) {
	return Production.SmokeValidate(derived, reports)
}

// SmokeValidate on an explicit Model mirrors DeriveManifest's injectability.
func (m Model) SmokeValidate(derived efficiency.Manifest, reports []protocol.ScoreReport) (efficiency.SmokeRecord, []string, error) {
	if derived.Derived == nil {
		return efficiency.SmokeRecord{}, nil, fmt.Errorf("manifest carries no derivation provenance; smoke applies only to derived manifests")
	}
	record := efficiency.SmokeRecord{
		TolerancePct:   fracMapToPct(m.SmokeToleranceFrac),
		BorderZoneFrac: m.SmokeBorderZoneFrac,
	}
	var reasons []string

	calibration := map[string]efficiency.CalibrationDataset{}
	sizes := map[string]bool{}
	for _, d := range derived.Calibration {
		calibration[fmt.Sprintf("%s:%d", d.RunSize, d.Seed)] = d
		sizes[d.RunSize] = true
	}
	baselines := map[string]efficiency.Baseline{}
	for _, b := range derived.Baselines {
		baselines[b.RunSize] = b
	}

	covered := map[string]bool{}
	for i, report := range reports {
		label := fmt.Sprintf("report[%d]", i)
		if report.Details == nil || report.Details.TokenUsage == nil {
			reasons = append(reasons, label+": missing trusted details.token_usage")
			continue
		}
		usage := report.Details.TokenUsage
		runSize := report.Details.RunSize
		label = fmt.Sprintf("%s/%d", runSize, report.Seed)
		if report.Details.BenchVersion != derived.BenchVersion {
			reasons = append(reasons, fmt.Sprintf("%s: bench_version %d does not match the derived manifest's %d", label, report.Details.BenchVersion, derived.BenchVersion))
			continue
		}
		if usage.Source != "model_proxy_provider_response" || !efficiency.ValidUsage(*usage) {
			reasons = append(reasons, label+": not a complete proxy-metered run")
			continue
		}
		baseline, ok := baselines[runSize]
		if !ok {
			reasons = append(reasons, label+": run size not present in the derived manifest")
			continue
		}
		if usage.Provider != baseline.Provider || usage.ProfileRevision != baseline.ProfileRevision || usage.Model != baseline.Model {
			reasons = append(reasons, fmt.Sprintf("%s: route/model identity %s/%s/%s does not match the derived baseline", label, usage.Provider, usage.ProfileRevision, usage.Model))
			continue
		}
		pinned, ok := calibration[fmt.Sprintf("%s:%d", runSize, report.Seed)]
		if !ok {
			reasons = append(reasons, label+": seed is outside the derived calibration grid")
			continue
		}
		if report.Details.DatasetSHA256 != pinned.DatasetSHA256 {
			reasons = append(reasons, label+": dataset_sha256 does not match the derived calibration dataset (wrong datagen revision?)")
			continue
		}

		profile, ok := gen.ProfileForVersion(runSize, derived.BenchVersion)
		if !ok {
			reasons = append(reasons, label+": no generation profile")
			continue
		}
		artifact, err := gen.GenerateDataset(report.Seed, profile, derived.BenchVersion)
		if err != nil {
			return record, reasons, fmt.Errorf("regenerate %s: %w", label, err)
		}
		predicted, ok := m.AnchoredChatTotal(runSize, Extract(artifact))
		if !ok || predicted <= 0 {
			reasons = append(reasons, label+": model carries no anchor for this run size")
			continue
		}
		errFrac := math.Abs(float64(usage.TotalTokens)-predicted) / predicted
		record.Runs = append(record.Runs, efficiency.SmokeRun{
			RunSize:              runSize,
			Seed:                 report.Seed,
			MeasuredTotalTokens:  usage.TotalTokens,
			PredictedTotalTokens: uint64(math.Round(predicted)),
			ErrorPct:             math.Round(errFrac*10000) / 100,
		})
		tolerance, ok := m.SmokeToleranceFrac[runSize]
		if !ok {
			reasons = append(reasons, label+": no smoke tolerance for this run size")
			continue
		}
		accept := tolerance * (1 - m.SmokeBorderZoneFrac)
		switch {
		case errFrac > tolerance:
			reasons = append(reasons, fmt.Sprintf("%s: measured %d deviates %.1f%% from predicted %.0f (tolerance %.1f%%)",
				label, usage.TotalTokens, 100*errFrac, predicted, 100*tolerance))
		case errFrac > accept:
			reasons = append(reasons, fmt.Sprintf("%s: measured %d is inside the border zone (%.1f%% vs acceptance %.1f%%) — inconclusive, rejected fail-closed",
				label, usage.TotalTokens, 100*errFrac, 100*accept))
		default:
			covered[runSize] = true
		}
	}

	for size := range sizes {
		if !covered[size] {
			reasons = append(reasons, fmt.Sprintf("run size %q has no accepted smoke run", size))
		}
	}
	sort.Slice(record.Runs, func(i, j int) bool {
		if record.Runs[i].RunSize != record.Runs[j].RunSize {
			return record.Runs[i].RunSize < record.Runs[j].RunSize
		}
		return record.Runs[i].Seed < record.Runs[j].Seed
	})
	sort.Strings(reasons)
	record.Passed = len(reasons) == 0
	return record, reasons, nil
}
