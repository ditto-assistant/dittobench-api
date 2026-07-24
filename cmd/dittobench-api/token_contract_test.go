package main

import (
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/efficiency"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// The v7 quality-only token contract at the pipeline level: an extreme
// audited usage never moves the v7 composite, while the report still carries
// both the audited usage block and the neutral token-efficiency record.
// v5 keeps the absolute transform; pre-v5 reports are untouched.
func TestApplyTokenContractV7QualityOnly(t *testing.T) {
	hugeUsage := protocol.TokenUsage{
		Status: "complete", Successes: 10, UsageAvailable: 10,
		PromptTokens: 50_000_000, CompletionTokens: 1_000_000, TotalTokens: 51_000_000,
		Provider: "openrouter", ProfileRevision: "openrouter-route-a471cd87ae7df5b9-v1",
		Model: "openai/gpt-oss-20b",
	}
	mkReport := func() protocol.ScoreReport {
		return protocol.ScoreReport{
			Composite: 0.8, CompositeStderr: 0.02,
			Details: &protocol.RunDetails{BenchVersion: 7, RunSize: "full", TokenUsage: &hugeUsage},
		}
	}

	// v7: composite and stderr unchanged; audited usage + neutral record carried.
	got := applyTokenContract(mkReport(), protocol.BenchVersionV7, "full", hugeUsage)
	if got.Composite != 0.8 || got.RawComposite != 0.8 || got.CompositeStderr != 0.02 {
		t.Fatalf("v7 composite must ignore token usage: %+v", got)
	}
	if got.Details.TokenUsage == nil || got.Details.TokenUsage.TotalTokens != hugeUsage.TotalTokens {
		t.Fatalf("v7 report must still carry the audited usage block")
	}
	te := got.Details.TokenEfficiency
	if te == nil || te.Multiplier != 1 || te.PenaltyApplied ||
		te.FormulaVersion != efficiency.V7QualityOnlyFormula ||
		te.DecisionReason != efficiency.V7QualityOnlyReason ||
		te.ObservedTotalTokens != hugeUsage.TotalTokens {
		t.Fatalf("v7 token record must be the neutral quality-only audit record: %+v", te)
	}

	// v5: the absolute transform path runs (here fail-neutral on baseline
	// identity, but the record documents the absolute contract).
	v5 := applyTokenContract(mkReport(), protocol.BenchVersionV5, "full", hugeUsage)
	if v5.Details.TokenEfficiency == nil || v5.Details.TokenEfficiency.FormulaVersion != efficiency.FormulaVersion {
		t.Fatalf("v5 must keep the absolute transform record: %+v", v5.Details.TokenEfficiency)
	}

	// pre-v5: untouched, no token record.
	v3 := applyTokenContract(mkReport(), protocol.BenchVersionV3, "full", hugeUsage)
	if v3.Composite != 0.8 || v3.Details.TokenEfficiency != nil {
		t.Fatalf("pre-v5 reports must not carry a token transform: %+v", v3)
	}
}
