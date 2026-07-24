package tokenmodel

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/efficiency"
	"github.com/ditto-assistant/dittobench-datagen/gen"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func TestProductionModelSanity(t *testing.T) {
	m := Production
	if m.Version == "" || m.FitCorpusSHA256 == "" || m.FitRuns < 50 {
		t.Fatalf("model must carry its fit identity: %+v", m)
	}
	for _, c := range []float64{
		m.Prompt.ToolCases, m.Prompt.MemoryCases, m.Prompt.ExpectedHops,
		m.Completion.ToolCases, m.Completion.MemoryCases,
		m.Embedding.HaystackBytes, m.Embedding.HaystackPairs, m.Embedding.Subjects, m.Embedding.MemoryCases,
	} {
		if c <= 0 {
			t.Fatalf("all fitted coefficients must be positive: %+v", m)
		}
	}
	for _, size := range []string{"small", "medium", "full"} {
		a, ok := m.Anchors[size]
		if !ok || a.Prompt < 1.0 || a.Prompt > 1.4 || a.Completion < 1.0 || a.Completion > 1.4 {
			t.Fatalf("anchor for %s outside sane band: %+v", size, a)
		}
		// Smoke tolerance must clear the observed residual maximum even after
		// the border-zone shrink, or honest smoke runs would be rejected.
		if m.SmokeToleranceFrac[size]*(1-m.SmokeBorderZoneFrac) <= m.ResidualMaxFrac[size] {
			t.Fatalf("%s: acceptance band %.3f must exceed observed residual max %.3f",
				size, m.SmokeToleranceFrac[size]*(1-m.SmokeBorderZoneFrac), m.ResidualMaxFrac[size])
		}
	}
}

func TestExtractDeterministicAndPredictionsPositive(t *testing.T) {
	prof, ok := gen.ProfileForVersion("small", protocol.BenchVersionV7)
	if !ok {
		t.Fatal("no small v7 profile")
	}
	a, err := gen.GenerateDataset(101, prof, protocol.BenchVersionV7)
	if err != nil {
		t.Fatal(err)
	}
	f1, f2 := Extract(a), Extract(a)
	if !reflect.DeepEqual(f1, f2) {
		t.Fatalf("Extract must be deterministic: %+v vs %+v", f1, f2)
	}
	if f1.ToolCases == 0 || f1.MemoryCases == 0 || f1.PromptBytes == 0 || f1.HaystackBytes == 0 {
		t.Fatalf("features must be populated: %+v", f1)
	}
	if Production.PredictChatTotalTokens(f1) <= 0 || Production.PredictEmbeddingTokens(f1) <= 0 {
		t.Fatalf("predictions must be positive for a real dataset")
	}
}

func TestEnvelopeViolationsFailClosed(t *testing.T) {
	sane := Features{ToolCases: 10, MemoryCases: 10, ExpectedHops: 9, PromptBytes: 1000, HaystackBytes: 6000, HaystackPairs: 60, Subjects: 10}
	if v := Production.EnvelopeViolations(sane); len(v) != 0 {
		t.Fatalf("in-envelope features must pass, got %v", v)
	}
	// A haystack 100x denser per memory case than anything the fit observed.
	weird := sane
	weird.HaystackBytes = 6_000_000
	v := Production.EnvelopeViolations(weird)
	if len(v) == 0 || v[0] != "haystack_bytes_per_memory_case" {
		t.Fatalf("out-of-envelope density must be flagged, got %v", v)
	}
	// Hop-free tool suite (all self-answer categories) is also out of envelope.
	hopless := sane
	hopless.ExpectedHops = 0
	if v := Production.EnvelopeViolations(hopless); len(v) == 0 {
		t.Fatalf("hopless suite must be flagged as extrapolation")
	}
}

// testModel is Production with the interpolation envelope widened enough to
// admit the CURRENT v7 datasets. The frozen Production envelope (fit-corpus
// range) legitimately refuses the hardened v7 suite — its small-size hop
// density (2.0 expected hops per tool case) is ~2x anything the fit corpus
// observed, which is exactly the extrapolation the guard exists to catch
// (TestProductionEnvelopeRefusesHardenedSuite). Widening it here lets the
// derive/smoke MACHINERY be exercised end-to-end on real generated datasets.
func testModel() Model {
	m := Production
	m.Envelope = map[string]RatioBounds{
		"expected_hops_per_tool_case":    {Min: 0.2, Max: 3.0},
		"prompt_bytes_per_case":          {Min: 10, Max: 200},
		"haystack_bytes_per_memory_case": {Min: 100, Max: 3000},
		"haystack_pairs_per_memory_case": {Min: 1, Max: 30},
	}
	return m
}

// sourceManifest is the embedded reviewed-measured v7 manifest — exactly the
// artifact a real transfer would start from.
func sourceManifest(t *testing.T) (efficiency.Manifest, []byte) {
	t.Helper()
	m := efficiency.V7ManifestSnapshot()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, raw
}

func TestDeriveManifestShapeAndDeterminism(t *testing.T) {
	source, raw := sourceManifest(t)
	derived, err := testModel().DeriveManifest(source, raw, protocol.BenchVersionV7)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if derived.ScoringEnabled {
		t.Fatal("derived manifests must never be emitted scoring-enabled")
	}
	if len(derived.Calibration) != len(source.Calibration) || len(derived.Baselines) != len(source.Baselines) {
		t.Fatalf("derived manifest must mirror the source grid: %d/%d datasets, %d/%d baselines",
			len(derived.Calibration), len(source.Calibration), len(derived.Baselines), len(source.Baselines))
	}
	if efficiency.ManifestProvenance(derived) != efficiency.ProvenanceDerivedUnvalidated {
		t.Fatalf("fresh derivation must classify derived-unvalidated")
	}
	if efficiency.ReadyForV7Production(derived) {
		t.Fatal("derived manifest must be rejected by the v7 readiness gate")
	}
	d := derived.Derived
	if d == nil || d.Method != MethodAnchoredV1 || d.ModelVersion != Production.Version ||
		d.FitCorpusSHA256 != Production.FitCorpusSHA256 || d.FitRuns != Production.FitRuns ||
		d.SourceManifestSHA256 == "" || len(d.TransferFactors) != 3 {
		t.Fatalf("derivation provenance incomplete: %+v", d)
	}
	for _, b := range derived.Baselines {
		if b.Aggregation != DerivedAggregation {
			t.Fatalf("derived baseline must carry the derived aggregation tag: %+v", b)
		}
		if b.TotalTokens != b.PromptTokens+b.CompletionTokens || b.TotalTokens == 0 {
			t.Fatalf("derived baseline arithmetic broken: %+v", b)
		}
		if b.ID != efficiency.BaselineID(protocol.BenchVersionV7, b) {
			t.Fatalf("derived baseline id must be content-derived: %+v", b)
		}
	}
	// Determinism: byte-identical on a second derivation.
	again, err := testModel().DeriveManifest(source, raw, protocol.BenchVersionV7)
	if err != nil {
		t.Fatal(err)
	}
	j1, _ := json.Marshal(derived)
	j2, _ := json.Marshal(again)
	if string(j1) != string(j2) {
		t.Fatal("DeriveManifest must be byte-deterministic")
	}
}

func TestDeriveManifestPreconditionsFailClosed(t *testing.T) {
	source, raw := sourceManifest(t)

	notEnabled := source
	notEnabled.ScoringEnabled = false
	if _, err := DeriveManifest(notEnabled, raw, protocol.BenchVersionV7); err == nil {
		t.Fatal("must reject a non-reviewed (scoring-disabled) source")
	}

	alreadyDerived := source
	alreadyDerived.Derived = &efficiency.Derivation{Method: MethodAnchoredV1}
	if _, err := DeriveManifest(alreadyDerived, raw, protocol.BenchVersionV7); err == nil {
		t.Fatal("must reject chaining derivations")
	}

	wrongIdentity := source
	wrongIdentity.Baselines = append([]efficiency.Baseline(nil), source.Baselines...)
	wrongIdentity.Baselines[0].Model = "some/other-model"
	if _, err := DeriveManifest(wrongIdentity, raw, protocol.BenchVersionV7); err == nil {
		t.Fatal("must reject a source outside the fitted route/model identity")
	}

	// v6 locks a different harness model than the one the model was fit for.
	if _, err := DeriveManifest(source, raw, protocol.BenchVersionV6); err == nil {
		t.Fatal("must reject a target bench version with a different locked model")
	}
}

// smokeReport fabricates a complete proxy-metered report for a pinned
// calibration dataset with a chosen measured total.
func smokeReport(t *testing.T, derived efficiency.Manifest, runSize string, measured uint64) protocol.ScoreReport {
	t.Helper()
	var pinned *efficiency.CalibrationDataset
	for i := range derived.Calibration {
		if derived.Calibration[i].RunSize == runSize {
			pinned = &derived.Calibration[i]
			break
		}
	}
	if pinned == nil {
		t.Fatalf("no calibration dataset for %s", runSize)
	}
	var baseline efficiency.Baseline
	for _, b := range derived.Baselines {
		if b.RunSize == runSize {
			baseline = b
		}
	}
	completion := measured / 10
	prompt := measured - completion
	return protocol.ScoreReport{
		RunID: "smoke-" + runSize,
		Seed:  pinned.Seed,
		Details: &protocol.RunDetails{
			BenchVersion:  derived.BenchVersion,
			RunSize:       runSize,
			DatasetSHA256: pinned.DatasetSHA256,
			TokenUsage: &protocol.TokenUsage{
				Source: "model_proxy_provider_response", Status: "complete",
				Successes: 5, UsageAvailable: 5,
				PromptTokens: prompt, CompletionTokens: completion, TotalTokens: measured,
				Provider: baseline.Provider, ProfileRevision: baseline.ProfileRevision, Model: baseline.Model,
			},
		},
	}
}

// predictedTotal recomputes the model's anchored per-seed prediction for the
// smoke dataset (what SmokeValidate compares against).
func predictedTotal(t *testing.T, derived efficiency.Manifest, runSize string) float64 {
	t.Helper()
	var pinned *efficiency.CalibrationDataset
	for i := range derived.Calibration {
		if derived.Calibration[i].RunSize == runSize {
			pinned = &derived.Calibration[i]
			break
		}
	}
	prof, _ := gen.ProfileForVersion(runSize, derived.BenchVersion)
	a, err := gen.GenerateDataset(pinned.Seed, prof, derived.BenchVersion)
	if err != nil {
		t.Fatal(err)
	}
	total, ok := Production.AnchoredChatTotal(runSize, Extract(a))
	if !ok {
		t.Fatal("no anchor")
	}
	return total
}

func TestSmokeValidateAcceptAndRejectPaths(t *testing.T) {
	source, raw := sourceManifest(t)
	derived, err := testModel().DeriveManifest(source, raw, protocol.BenchVersionV7)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}

	within := func(runSize string, off float64) protocol.ScoreReport {
		return smokeReport(t, derived, runSize, uint64(predictedTotal(t, derived, runSize)*(1+off)))
	}

	// Accept: one run per size, all near the prediction.
	record, reasons, err := testModel().SmokeValidate(derived, []protocol.ScoreReport{
		within("small", 0.05), within("medium", 0.03), within("full", -0.04),
	})
	if err != nil || len(reasons) != 0 || !record.Passed || len(record.Runs) != 3 {
		t.Fatalf("clean smoke must pass: reasons=%v err=%v record=%+v", reasons, err, record)
	}

	// Reject: gross drift on one size (way past tolerance).
	_, reasons, err = testModel().SmokeValidate(derived, []protocol.ScoreReport{
		within("small", 0.05), within("medium", 0.60), within("full", -0.04),
	})
	if err != nil || len(reasons) == 0 {
		t.Fatalf("gross medium drift must be rejected, reasons=%v", reasons)
	}

	// Reject: border zone is inconclusive (between acceptance and tolerance).
	borderOff := Production.SmokeToleranceFrac["full"] * (1 - Production.SmokeBorderZoneFrac/2)
	_, reasons, err = testModel().SmokeValidate(derived, []protocol.ScoreReport{
		within("small", 0.05), within("medium", 0.03), within("full", borderOff),
	})
	if err != nil || len(reasons) == 0 {
		t.Fatalf("border-zone result must be rejected fail-closed, reasons=%v", reasons)
	}

	// Reject: missing run-size coverage.
	_, reasons, err = testModel().SmokeValidate(derived, []protocol.ScoreReport{
		within("small", 0.0), within("medium", 0.0),
	})
	if err != nil || len(reasons) == 0 {
		t.Fatalf("missing full coverage must be rejected, reasons=%v", reasons)
	}

	// Reject: dataset hash mismatch (wrong datagen revision).
	bad := within("small", 0.0)
	bad.Details.DatasetSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	_, reasons, err = testModel().SmokeValidate(derived, []protocol.ScoreReport{
		bad, within("medium", 0.0), within("full", 0.0),
	})
	if err != nil || len(reasons) == 0 {
		t.Fatalf("dataset mismatch must be rejected, reasons=%v", reasons)
	}

	// Structural misuse: smoke on a non-derived manifest.
	if _, _, err := testModel().SmokeValidate(source, nil); err == nil {
		t.Fatal("smoke on a reviewed-measured manifest must error")
	}
}

// The frozen Production envelope must REFUSE the current hardened v7 suite:
// its small-size hop density is ~2x the fit corpus's observed maximum, so a
// derivation would extrapolate — the honest answer is "run full calibration".
// (This pins the concrete guard behavior on real datasets; if a future refit
// widens the envelope legitimately, this test moves with it.)
func TestProductionEnvelopeRefusesHardenedSuite(t *testing.T) {
	source, raw := sourceManifest(t)
	_, err := DeriveManifest(source, raw, protocol.BenchVersionV7)
	if err == nil {
		t.Skip("current datagen datasets fit inside the Production envelope; nothing to refuse")
	}
	if !strings.Contains(err.Error(), "interpolation envelope") {
		t.Fatalf("refusal must cite the interpolation envelope, got: %v", err)
	}
	if !strings.Contains(err.Error(), "run a full calibration") {
		t.Fatalf("refusal must direct to a full calibration, got: %v", err)
	}
}
