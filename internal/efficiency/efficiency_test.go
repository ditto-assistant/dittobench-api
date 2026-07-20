package efficiency

import (
	"math"
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func eligibleReport(composite float64) protocol.ScoreReport {
	return protocol.ScoreReport{Composite: composite, ToolMean: 0.8, MemoryMean: 0.8}
}

func completeUsage(prompt, completion uint64) protocol.TokenUsage {
	return protocol.TokenUsage{
		Status: "complete", Successes: 10, UsageAvailable: 10,
		PromptTokens: prompt, CompletionTokens: completion,
		TotalTokens: prompt + completion,
	}
}

func testBaseline() Baseline {
	return Baseline{
		ID: "v5-test", BenchVersion: 5, RunSize: "full", Provider: "p",
		ProfileRevision: "r", Model: "m", PromptTokens: 999,
		CompletionTokens: 4, TotalTokens: 1_003, Samples: 5,
		Aggregation: "median", StarterKitRevision: "sha",
	}
}

func TestTransformAboveEqualAndBelowBaseline(t *testing.T) {
	baseline := testBaseline() // weighted baseline = 999 + 0.25*4 = 1,000
	for _, tc := range []struct {
		name      string
		prompt    uint64
		wantMult  float64
		wantScore float64
	}{
		{name: "below receives continuing reward", prompt: 639, wantMult: math.Pow(1_000.0/640.0, 0.25), wantScore: 1.006231},
		{name: "equal is neutral", prompt: 999, wantMult: 1, wantScore: 0.9},
		{name: "above is penalized", prompt: 1_599, wantMult: math.Sqrt(1_000.0 / 1_600.0), wantScore: 0.711512},
		{name: "extreme penalty is bounded", prompt: 99_999, wantMult: 0.75, wantScore: 0.675},
		{name: "quarter baseline has no reward ceiling", prompt: 249, wantMult: math.Sqrt(2), wantScore: 1.272792},
		{name: "one percent baseline keeps improving", prompt: 9, wantMult: math.Sqrt(10), wantScore: 2.84605},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Apply(eligibleReport(0.9), completeUsage(tc.prompt, 4), &baseline, 1)
			if math.Abs(got.Multiplier-tc.wantMult) > 1e-9 || got.AdjustedComposite != tc.wantScore {
				t.Fatalf("result = %#v", got)
			}
		})
	}
}

func TestPromptTokensCarryFourTimesCompletionWeight(t *testing.T) {
	baseline := testBaseline()
	promptGrowth := Apply(eligibleReport(0.9), completeUsage(1_099, 4), &baseline, 1)
	completionGrowth := Apply(eligibleReport(0.9), completeUsage(999, 404), &baseline, 1)
	if promptGrowth.Multiplier != completionGrowth.Multiplier {
		t.Fatalf("100 prompt tokens and 400 completion tokens must have equal weight: prompt=%#v completion=%#v", promptGrowth, completionGrowth)
	}
	if promptGrowth.PromptTokenWeight != 1 || promptGrowth.CompletionTokenWeight != 0.25 {
		t.Fatalf("weights not audited in decision: %#v", promptGrowth)
	}
}

func TestQualityFloorBlocksRewardButNotStuffingPenalty(t *testing.T) {
	baseline := testBaseline()
	lowQuality := eligibleReport(0.79)

	rewardAttempt := Apply(lowQuality, completeUsage(9, 4), &baseline, 1)
	if rewardAttempt.Multiplier != 1 || rewardAttempt.AdjustedComposite != 0.79 {
		t.Fatalf("low quality must not earn a token reward: %#v", rewardAttempt)
	}

	stuffing := Apply(lowQuality, completeUsage(1_599, 4), &baseline, 1)
	if stuffing.Multiplier >= 1 || stuffing.AdjustedComposite >= 0.79 {
		t.Fatalf("quality floor must not excuse context stuffing: %#v", stuffing)
	}
}

func TestQualityAndTelemetryFloorsFailNeutral(t *testing.T) {
	baseline := testBaseline()
	tests := []struct {
		name     string
		report   protocol.ScoreReport
		usage    protocol.TokenUsage
		coverage float64
		reason   string
	}{
		{name: "empty answers", report: eligibleReport(0.9), usage: completeUsage(1, 0), coverage: 0, reason: "response_coverage_below_floor"},
		{name: "terse but wrong", report: eligibleReport(0.1), usage: completeUsage(1, 0), coverage: 1, reason: "raw_composite_below_floor"},
		{name: "quality below reward floor", report: eligibleReport(0.79), usage: completeUsage(1, 0), coverage: 1, reason: "raw_composite_below_floor"},
		{name: "weak suite", report: protocol.ScoreReport{Composite: 0.8, ToolMean: 0.9, MemoryMean: 0.69}, usage: completeUsage(1, 0), coverage: 1, reason: "memory_mean_below_floor"},
		{name: "zero usage", report: eligibleReport(0.9), usage: completeUsage(0, 0), coverage: 1, reason: "token_telemetry_unavailable"},
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
	got := Apply(eligibleReport(0.9), completeUsage(500, 10), nil, 1)
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
