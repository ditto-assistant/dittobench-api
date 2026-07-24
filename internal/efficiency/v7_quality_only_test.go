package efficiency

import (
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// ApplyForVersion must be byte-identical to Apply for every pre-v7 version, so
// v5/v6 replays and already-recorded reports keep their exact transform.
func TestApplyForVersionPreV7Identity(t *testing.T) {
	baseline := testBaseline()
	for _, v := range []int{protocol.BenchVersionV5, protocol.BenchVersionV6} {
		for _, prompt := range []uint64{500, 900, 1_900, 99_900} {
			got := ApplyForVersion(v, testReport(0.9), completeUsage(prompt, 100), &baseline)
			want := Apply(testReport(0.9), completeUsage(prompt, 100), &baseline)
			if got != want {
				t.Fatalf("v%d prompt=%d: ApplyForVersion diverged from Apply:\n got %#v\nwant %#v", v, prompt, got, want)
			}
		}
	}
}

// The v7 QUALITY-ONLY contract: token usage never moves the v7 composite, no
// matter how extreme, and the neutral record still carries the audited
// observed totals so usage stays first-class in the report.
func TestApplyForVersionV7QualityOnly(t *testing.T) {
	baseline := testBaseline()
	for _, prompt := range []uint64{1, 900, 99_900, 10_000_000} {
		usage := completeUsage(prompt, 100)
		got := ApplyForVersion(protocol.BenchVersionV7, testReport(0.9), usage, &baseline)
		if got.Multiplier != 1 || got.PenaltyApplied || got.AdjustedComposite != 0.9 || got.RawComposite != 0.9 {
			t.Fatalf("v7 must never move the composite (prompt=%d): %#v", prompt, got)
		}
		if got.FormulaVersion != V7QualityOnlyFormula || got.DecisionReason != V7QualityOnlyReason {
			t.Fatalf("v7 record must carry the quality-only contract identity: %#v", got)
		}
		if got.ObservedPromptTokens != usage.PromptTokens ||
			got.ObservedCompletionTokens != usage.CompletionTokens ||
			got.ObservedTotalTokens != usage.TotalTokens {
			t.Fatalf("v7 record must carry the audited observed usage: %#v", got)
		}
		// No absolute budget enters the v7 record at all.
		if got.BaselineID != "" || got.BaselineTotalTokens != 0 || got.ExcessRatio != 0 {
			t.Fatalf("v7 record must not carry an absolute budget: %#v", got)
		}
	}
	// Identical with no baseline and with invalid usage: still neutral.
	if got := ApplyForVersion(protocol.BenchVersionV7, testReport(0.5), protocol.TokenUsage{}, nil); got.Multiplier != 1 || got.AdjustedComposite != 0.5 {
		t.Fatalf("v7 neutral contract must not depend on baseline/usage validity: %#v", got)
	}
}

// Provenance classification and the fail-closed rejection of derived
// manifests by the v7 readiness gate.
func TestManifestProvenanceAndDerivedRejection(t *testing.T) {
	measured := V7ManifestSnapshot()
	if p := ManifestProvenance(measured); p != ProvenanceReviewedMeasured {
		t.Fatalf("embedded manifest must be reviewed-measured, got %s", p)
	}
	// The embedded 2026-07 campaign manifest ships as a candidate
	// (scoring_enabled=false — under the v7 quality-only contract its budgets
	// are reference identity, never activated). Structurally it must still
	// satisfy the readiness gate once enabled, so the gate's derived-rejection
	// below is meaningful.
	enabled := V7ManifestSnapshot()
	enabled.ScoringEnabled = true
	if !ReadyForV7Production(enabled) {
		t.Fatalf("enabled copy of the reviewed-measured manifest must be production-ready")
	}

	derived := V7ManifestSnapshot()
	derived.ScoringEnabled = true
	derived.Derived = &Derivation{Method: "tokenmodel-anchored-v1"}
	if p := ManifestProvenance(derived); p != ProvenanceDerivedUnvalidated {
		t.Fatalf("derivation without smoke must classify derived-unvalidated, got %s", p)
	}
	derived.Derived.Smoke = &SmokeRecord{Passed: true}
	if p := ManifestProvenance(derived); p != ProvenanceDerivedSmokeVerified {
		t.Fatalf("smoke-passed derivation must classify derived-smoke-validated, got %s", p)
	}
	// Regardless of smoke outcome, the v7 readiness gate rejects derived
	// manifests today (the class is reserved for later platform policy).
	for _, smoke := range []*SmokeRecord{nil, {Passed: false}, {Passed: true}} {
		derived.Derived.Smoke = smoke
		if ReadyForV7Production(derived) {
			t.Fatalf("derived manifest (smoke=%v) must be rejected fail-closed", smoke)
		}
	}
}

// The advertised readiness carries the provenance class — but only once the
// manifest is ready. The embedded candidate is scoring-disabled, so today's
// advertised readiness stays dark (empty, fail-closed) with no provenance.
func TestV7CalibrationReadinessCarriesProvenance(t *testing.T) {
	dark := V7CalibrationReadiness()
	if dark.Provenance != "" || dark.ManifestSHA256 != "" {
		t.Fatalf("disabled candidate must keep readiness dark, got %+v", dark)
	}
	enabled := V7ManifestSnapshot()
	enabled.ScoringEnabled = true
	got := v7CalibrationReadiness(enabled, manifestV7JSON)
	if got.Provenance != ProvenanceReviewedMeasured {
		t.Fatalf("enabled readiness provenance = %q, want %q", got.Provenance, ProvenanceReviewedMeasured)
	}
}
