package efficiency

import (
	"math"
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func eligibleReport(composite float64) protocol.ScoreReport {
	return protocol.ScoreReport{Composite: composite, ToolMean: 0.8, MemoryMean: 0.8}
}

func completeUsage(total uint64) protocol.TokenUsage {
	return protocol.TokenUsage{
		Status: "complete", Successes: 10, UsageAvailable: 10,
		PromptTokens: total, TotalTokens: total,
	}
}

func testBaseline(total uint64) Baseline {
	return Baseline{
		ID: "v5-test", BenchVersion: 5, RunSize: "full", Provider: "p",
		ProfileRevision: "r", Model: "m", PromptTokens: total - 100,
		CompletionTokens: 100, TotalTokens: total, Samples: 5,
		Aggregation: "median", StarterKitRevision: "sha",
	}
}

func TestTransformAboveEqualAndBelowBaseline(t *testing.T) {
	baseline := testBaseline(1_000)
	for _, tc := range []struct {
		name      string
		observed  uint64
		wantMult  float64
		wantScore float64
	}{
		{name: "below receives bounded reward", observed: 640, wantMult: 1.25, wantScore: 1.125},
		{name: "equal is neutral", observed: 1_000, wantMult: 1, wantScore: 0.9},
		{name: "above is penalized", observed: 1_600, wantMult: math.Sqrt(1_000.0 / 1_600.0), wantScore: 0.711512},
		{name: "extreme is bounded", observed: 100_000, wantMult: 0.75, wantScore: 0.675},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Apply(eligibleReport(0.9), completeUsage(tc.observed), &baseline, 1)
			if math.Abs(got.Multiplier-tc.wantMult) > 1e-9 || got.AdjustedComposite != tc.wantScore {
				t.Fatalf("result = %#v", got)
			}
		})
	}
}

func TestQualityAndTelemetryFloorsFailNeutral(t *testing.T) {
	baseline := testBaseline(1_000)
	tests := []struct {
		name     string
		report   protocol.ScoreReport
		usage    protocol.TokenUsage
		coverage float64
		reason   string
	}{
		{name: "empty answers", report: eligibleReport(0.9), usage: completeUsage(1), coverage: 0, reason: "response_coverage_below_floor"},
		{name: "terse but wrong", report: eligibleReport(0.1), usage: completeUsage(1), coverage: 1, reason: "raw_composite_below_floor"},
		{name: "weak suite", report: protocol.ScoreReport{Composite: 0.6, ToolMean: 0.9, MemoryMean: 0.1}, usage: completeUsage(1), coverage: 1, reason: "memory_mean_below_floor"},
		{name: "zero usage", report: eligibleReport(0.9), usage: completeUsage(0), coverage: 1, reason: "token_telemetry_unavailable"},
		{name: "missing provider usage", report: eligibleReport(0.9), usage: protocol.TokenUsage{Status: "unavailable", Successes: 10}, coverage: 1, reason: "token_telemetry_unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Apply(tc.report, tc.usage, &baseline, tc.coverage)
			if got.Multiplier != 1 || got.AdjustedComposite != tc.report.Composite || got.EligibilityReason != tc.reason {
				t.Fatalf("result = %#v", got)
			}
		})
	}
}

func TestMissingBaselineFailsNeutral(t *testing.T) {
	got := Apply(eligibleReport(0.9), completeUsage(500), nil, 1)
	if got.Multiplier != 1 || got.AdjustedComposite != 0.9 || got.EligibilityReason != "baseline_unavailable" {
		t.Fatalf("result = %#v", got)
	}
}

func TestResponseCoverageIgnoresMinerReportedTokens(t *testing.T) {
	got := ResponseCoverage([]protocol.RunResponse{
		{FinalText: "helpful answer", PromptTokens: 999999},
		{Answer: "short answer", OutputTokens: 999999},
		{Abstain: true, PromptTokens: 1, OutputTokens: 1},
	})
	if got != 2.0/3.0 {
		t.Fatalf("coverage = %v", got)
	}
}

func TestEmbeddedManifestIsDeliberatelyInactiveUntilMeasured(t *testing.T) {
	if ProductionReady() {
		t.Fatal("v5 must not be advertised before all provider/run-size baselines are measured")
	}
	manifest := ManifestSnapshot()
	if manifest.StarterKitRevision == "" || len(manifest.Calibration) != 15 || len(manifest.Baselines) != 0 {
		t.Fatalf("manifest = %#v", manifest)
	}
}
