package efficiency

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func testReport(composite float64) protocol.ScoreReport {
	return protocol.ScoreReport{Composite: composite, ToolMean: 0.8, MemoryMean: 0.8}
}

func TestV7CalibrationReadinessIsBoundToEmbeddedManifest(t *testing.T) {
	got := V7CalibrationReadiness()
	if !ValidV7CalibrationReadiness(got) || len(got.SupportedRoutes) != 1 {
		t.Fatalf("reviewed v7 readiness = %+v", got)
	}
	want := CalibrationRouteIdentity{
		Provider: v7AggregateProvider, ProfileRevision: v7AggregateProfile,
		Model: llm.V7HarnessModel,
	}
	if got.SupportedRoutes[0] != want {
		t.Fatalf("reviewed v7 route = %+v, want %+v", got.SupportedRoutes[0], want)
	}
}

func TestV7CalibrationReadinessRejectsProviderSpecificManifest(t *testing.T) {
	manifest := Manifest{
		BenchVersion: protocol.BenchVersionV7, ScoringEnabled: true,
		StarterKitRevision: "60aab4e5e2839ddb0fe8c80492bd7b76ba2668fd",
		Calibration:        validV7Datasets(),
	}
	for _, provider := range []struct{ name, profile string }{
		{"groq", "openrouter-route-bbbbbbbbbbbbbbbb-v1"},
		{"amazon", "openrouter-route-aaaaaaaaaaaaaaaa-v1"},
	} {
		for _, runSize := range []string{"small", "medium", "full"} {
			manifest.Baselines = append(manifest.Baselines, Baseline{
				ID:           "reviewed-" + provider.name + "-" + runSize,
				BenchVersion: protocol.BenchVersionV7, RunSize: runSize,
				Provider: provider.name, ProfileRevision: provider.profile, Model: llm.V7HarnessModel,
				PromptTokens: 900, CompletionTokens: 100, TotalTokens: 1_000,
				Samples: 20, Aggregation: "nearest_rank_p90", StarterKitRevision: manifest.StarterKitRevision,
			})
		}
	}
	got := v7CalibrationReadiness(manifest, []byte(`{"reviewed":"manifest"}`))
	if got.ManifestSHA256 != "" || len(got.SupportedRoutes) != 0 {
		t.Fatalf("provider-specific readiness escaped aggregate gate: %+v", got)
	}
}

func TestAggregateV7ManifestRequiresExactTwentyByThreeContract(t *testing.T) {
	const profile = "openrouter-route-8efde5ce9f5a4e58-v1"
	manifest := Manifest{
		BenchVersion: protocol.BenchVersionV7, ScoringEnabled: true,
		StarterKitRevision: v7StarterKitRevision,
		DatasetKnownVector: v7DatasetKnownVector,
		Calibration:        append([]CalibrationDataset(nil), productionV7Manifest.Calibration...),
	}
	for _, runSize := range []string{"small", "medium", "full"} {
		baseline := Baseline{
			BenchVersion: protocol.BenchVersionV7,
			RunSize:      runSize, Provider: "openrouter", ProfileRevision: profile,
			Model: llm.V7HarnessModel, PromptTokens: 900, CompletionTokens: 100,
			TotalTokens: 1_000, Samples: 20, Aggregation: "nearest_rank_p90",
			StarterKitRevision: manifest.StarterKitRevision,
		}
		baseline.ID = baselineID(protocol.BenchVersionV7, baseline)
		manifest.Baselines = append(manifest.Baselines, baseline)
	}
	if !ReadyForV7Production(manifest) {
		t.Fatal("exact aggregate 20x3 manifest must be production-ready")
	}
	readiness := v7CalibrationReadiness(manifest, []byte(`{"reviewed":true}`))
	if len(readiness.SupportedRoutes) != 1 || readiness.SupportedRoutes[0] != (CalibrationRouteIdentity{
		Provider: "openrouter", ProfileRevision: profile, Model: llm.V7HarnessModel,
	}) {
		t.Fatalf("aggregate route identity = %+v", readiness.SupportedRoutes)
	}

	manifest.Baselines[0].Samples = 21
	if ReadyForV7Production(manifest) {
		t.Fatal("v7 accepted a baseline not built from exactly 20 runs")
	}
	manifest.Baselines[0].Samples = 20
	manifest.Baselines[0].ID = "v7-starter-p90-0000000000000000"
	if ReadyForV7Production(manifest) {
		t.Fatal("v7 accepted a baseline whose content-derived id was forged")
	}
	manifest.Baselines[0].ID = baselineID(protocol.BenchVersionV7, manifest.Baselines[0])
	manifest.Calibration[20].RunSize = manifest.Calibration[0].RunSize
	manifest.Calibration[20].Seed = manifest.Calibration[0].Seed
	if ReadyForV7Production(manifest) {
		t.Fatal("v7 accepted a duplicate run-size/seed contract with a different digest")
	}
}

func TestV7ManifestPinsReviewedGeneratorAndStarterIdentity(t *testing.T) {
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.StarterKitRevision = "60aab4e5e2839ddb0fe8c80492bd7b76ba2668fd" },
		func(m *Manifest) { m.DatasetKnownVector = fmt.Sprintf("%064x", 1) },
		func(m *Manifest) { m.Calibration[0], m.Calibration[1] = m.Calibration[1], m.Calibration[0] },
	} {
		manifest := V7ManifestSnapshot()
		mutate(&manifest)
		if ReadyForV7Production(manifest) {
			t.Fatal("v7 accepted drift from the reviewed starter/generator contract")
		}
	}
}

func TestEmbeddedV7ManifestDiffersFromCandidateOnlyByApprovalFlag(t *testing.T) {
	body, err := os.ReadFile("../../docs/baselines-v7-candidate.json")
	if err != nil {
		t.Fatal(err)
	}
	var candidate Manifest
	if err := json.Unmarshal(body, &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.ScoringEnabled {
		t.Fatal("candidate calibration artifact must remain fail-closed")
	}
	candidate.ScoringEnabled = true
	if !reflect.DeepEqual(candidate, V7ManifestSnapshot()) {
		t.Fatal("embedded production manifest drifted from the reviewed candidate beyond the approval flag")
	}
}

func validV7Datasets() []CalibrationDataset {
	datasets := make([]CalibrationDataset, 0, 60)
	for _, runSize := range []string{"small", "medium", "full"} {
		for seed := int64(1); seed <= 20; seed++ {
			datasets = append(datasets, CalibrationDataset{
				RunSize: runSize, Seed: seed,
				DatasetSHA256: fmt.Sprintf("%064x", len(datasets)+1),
			})
		}
	}
	return datasets
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

func TestEmbeddedV7ManifestIsMeasuredAndPhaseBApproved(t *testing.T) {
	if !ProductionReadyForVersion(protocol.BenchVersionV7) {
		t.Fatal("reviewed aggregate v7 manifest must be production ready")
	}
	manifest := V7ManifestSnapshot()
	if !manifest.ScoringEnabled || manifest.StarterKitRevision == "" || len(manifest.Calibration) != 60 || len(manifest.Baselines) != 3 {
		t.Fatalf("v7 manifest = %#v", manifest)
	}
	usage := protocol.TokenUsage{
		Provider: v7AggregateProvider, ProfileRevision: v7AggregateProfile,
		Model: llm.V7HarnessModel,
	}
	baseline, ok := LookupForVersion(protocol.BenchVersionV7, "full", usage)
	if !ok || baseline.TotalTokens != 936_353 {
		t.Fatalf("v7 full baseline = %#v, ok=%v", baseline, ok)
	}
}

func TestEmbeddingV7ManifestDoesNotChangeV6QwenLookup(t *testing.T) {
	v5 := ManifestSnapshot()
	baseline := v5.Baselines[0]
	usage := protocol.TokenUsage{
		Provider: baseline.Provider, ProfileRevision: baseline.ProfileRevision,
		Model: baseline.Model,
	}
	fromV5, okV5 := LookupForVersion(protocol.BenchVersionV5, baseline.RunSize, usage)
	fromV6, okV6 := LookupForVersion(protocol.BenchVersionV6, baseline.RunSize, usage)
	if !okV5 || !okV6 || fromV5 != fromV6 {
		t.Fatalf("v6 lookup changed: v5=%#v/%v v6=%#v/%v", fromV5, okV5, fromV6, okV6)
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
