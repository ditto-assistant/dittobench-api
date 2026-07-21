package efficiency

import (
	"math"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func testReport(composite float64) protocol.ScoreReport {
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
		ProfileRevision: "r", Model: "m", PromptTokens: 900,
		CompletionTokens: 100, TotalTokens: 1_000, Samples: 20,
		Aggregation: "nearest_rank_p90", StarterKitRevision: "sha",
	}
}

func TestWastePenaltyIsOneSidedAndSaturating(t *testing.T) {
	baseline := testBaseline()
	for _, tc := range []struct {
		name      string
		prompt    uint64
		complete  uint64
		wantMult  float64
		wantScore float64
		applied   bool
	}{
		{name: "far below budget is neutral", prompt: 50, complete: 50, wantMult: 1, wantScore: 0.9},
		{name: "budget is neutral", prompt: 900, complete: 100, wantMult: 1, wantScore: 0.9},
		{name: "twenty five percent over", prompt: 1_150, complete: 100, wantMult: 0.98, wantScore: 0.882, applied: true},
		{name: "double budget", prompt: 1_900, complete: 100, wantMult: 0.95, wantScore: 0.855, applied: true},
		{name: "four times budget", prompt: 3_900, complete: 100, wantMult: 0.925, wantScore: 0.8325, applied: true},
		{name: "gross waste approaches floor", prompt: 99_900, complete: 100, wantMult: 0.901, wantScore: 0.8109, applied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Apply(testReport(0.9), completeUsage(tc.prompt, tc.complete), &baseline)
			if math.Abs(got.Multiplier-tc.wantMult) > 1e-9 || got.AdjustedComposite != tc.wantScore || got.PenaltyApplied != tc.applied {
				t.Fatalf("result = %#v", got)
			}
			if got.Multiplier > 1 || got.Multiplier < MinMultiplier {
				t.Fatalf("waste-only multiplier outside [%v,1]: %#v", MinMultiplier, got)
			}
		})
	}
}

func TestPromptAndCompletionTokensBothCountTowardWaste(t *testing.T) {
	baseline := testBaseline()
	promptGrowth := Apply(testReport(0.9), completeUsage(1_000, 100), &baseline)
	completionGrowth := Apply(testReport(0.9), completeUsage(900, 200), &baseline)
	if promptGrowth.Multiplier != completionGrowth.Multiplier {
		t.Fatalf("equal provider-token totals must have equal waste cost: prompt=%#v completion=%#v", promptGrowth, completionGrowth)
	}
}

func TestCheapAnswersNeverReceiveAReward(t *testing.T) {
	baseline := testBaseline()
	for _, composite := range []float64{0, 0.1, 0.9, 1} {
		got := Apply(testReport(composite), completeUsage(1, 0), &baseline)
		if got.Multiplier != 1 || got.AdjustedComposite != composite || got.PenaltyApplied {
			t.Fatalf("composite %v received a minimization reward: %#v", composite, got)
		}
	}
}

func TestMissingTelemetryAndBaselineFailNeutral(t *testing.T) {
	baseline := testBaseline()
	for _, tc := range []struct {
		name   string
		usage  protocol.TokenUsage
		base   *Baseline
		reason string
	}{
		{name: "zero usage", usage: completeUsage(0, 0), base: &baseline, reason: "token_telemetry_unavailable"},
		{name: "missing provider usage", usage: protocol.TokenUsage{Status: "unavailable", Successes: 10}, base: &baseline, reason: "token_telemetry_unavailable"},
		{name: "missing baseline", usage: completeUsage(500, 10), reason: "baseline_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Apply(testReport(0.9), tc.usage, tc.base)
			if got.Multiplier != 1 || got.AdjustedComposite != 0.9 || got.DecisionReason != tc.reason {
				t.Fatalf("result = %#v", got)
			}
		})
	}
}

func TestEmbeddedManifestIsMeasuredAndPhaseBApproved(t *testing.T) {
	if !ProductionReady() {
		t.Fatal("reviewed dual-provider v5 manifest must be production ready")
	}
	manifest := ManifestSnapshot()
	if !manifest.ScoringEnabled || manifest.StarterKitRevision == "" || len(manifest.Calibration) != 60 || len(manifest.Baselines) != 6 {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestReadyForProductionRequiresExplicitDualProviderPhaseBManifest(t *testing.T) {
	manifest := ManifestSnapshot()
	manifest.ScoringEnabled = true
	for _, provider := range []struct{ name, revision, model string }{
		{"chutes", llm.ChutesRelayProfileRevision, llm.LockedUpstreamModel},
		{"openrouter", llm.OpenRouterRelayProfileRevision, llm.LockedHarnessModel},
	} {
		for _, runSize := range []string{"small", "medium", "full"} {
			manifest.Baselines = append(manifest.Baselines, Baseline{
				ID: "reviewed-" + provider.name + "-" + runSize, BenchVersion: 5,
				RunSize: runSize, Provider: provider.name, ProfileRevision: provider.revision,
				Model: provider.model, PromptTokens: 900, CompletionTokens: 100,
				TotalTokens: 1_000, Samples: 20, Aggregation: "nearest_rank_p90",
				StarterKitRevision: manifest.StarterKitRevision,
			})
		}
	}
	if !ReadyForProduction(manifest) {
		t.Fatal("complete explicitly enabled dual-provider manifest must be ready")
	}
	manifest.Baselines = manifest.Baselines[:5]
	if ReadyForProduction(manifest) {
		t.Fatal("missing provider/run-size group must fail closed")
	}
}
