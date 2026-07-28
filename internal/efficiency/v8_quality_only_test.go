package efficiency

import (
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func TestV8QualityOnlyManifestFailsClosedOnIdentityDrift(t *testing.T) {
	manifest := V8ManifestSnapshot()
	if !ReadyForV8QualityOnly(manifest) {
		t.Fatal("embedded v8 manifest is not ready")
	}
	mutations := []func(*Manifest){
		func(m *Manifest) { m.ScoringEnabled = true },
		func(m *Manifest) { m.DatasetKnownVector = "" },
		func(m *Manifest) { m.StarterKitRevision = "" },
		func(m *Manifest) { m.Calibration[0].DatasetSHA256 = "" },
		func(m *Manifest) { m.Baselines = []Baseline{{ID: "unexpected"}} },
		func(m *Manifest) { m.Embedding.Model = "attacker/model" },
	}
	for i, mutate := range mutations {
		candidate := V8ManifestSnapshot()
		mutate(&candidate)
		if ReadyForV8QualityOnly(candidate) {
			t.Fatalf("mutation %d did not fail closed", i)
		}
	}
}

func TestV8InheritsV7QualityOnlyTokenContract(t *testing.T) {
	report := testReport(0.91)
	usage := completeUsage(12_000_000, 500_000)
	got := ApplyForVersion(protocol.BenchVersionV8, report, usage, nil)
	if got.Multiplier != 1 || got.AdjustedComposite != report.Composite || got.PenaltyApplied {
		t.Fatalf("v8 token usage moved quality score: %+v", got)
	}
}
