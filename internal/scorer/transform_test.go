package scorer

import (
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/persona"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func auditPair(group string, baseCorrect, xfCorrect bool) []protocol.CaseScore {
	return []protocol.CaseScore{
		{CaseID: group + "-base", Kind: protocol.KindMemory, TwinGroup: persona.AuditTwinPrefix + group, Correct: baseCorrect},
		{CaseID: group + "-xf", Kind: protocol.KindMemory, TwinGroup: persona.AuditTwinPrefix + group, Correct: xfCorrect},
	}
}

// TestTransformRobustnessMeasuresSplits pins the core of the audit metric: it
// scores CONSISTENCY, not correctness. A pair the harness got wrong twice is
// already penalized by accuracy and must not be charged again here; only the
// split -- right on the phrasing it was built for, wrong on the one it was not --
// is the brittleness signature.
func TestTransformRobustnessMeasuresSplits(t *testing.T) {
	cases := []struct {
		name  string
		in    []protocol.CaseScore
		want  float64
		pairs int
	}{
		{"both correct", auditPair("a", true, true), 1.0, 1},
		{"both wrong is consistent, not brittle", auditPair("a", false, false), 1.0, 1},
		{"split is brittle", auditPair("a", true, false), 0.0, 1},
		{"split the other way", auditPair("a", false, true), 0.0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, pairs := TransformRobustness(tc.in)
			if got == nil {
				t.Fatal("want a robustness value, got nil")
			}
			if *got != tc.want || pairs != tc.pairs {
				t.Fatalf("got %.3f over %d pairs, want %.3f over %d", *got, pairs, tc.want, tc.pairs)
			}
		})
	}
}

// TestTransformRobustnessRate checks the aggregate over several pairs.
func TestTransformRobustnessRate(t *testing.T) {
	var in []protocol.CaseScore
	in = append(in, auditPair("a", true, true)...)   // consistent
	in = append(in, auditPair("b", true, true)...)   // consistent
	in = append(in, auditPair("c", true, false)...)  // split
	in = append(in, auditPair("d", false, false)...) // consistent
	got, pairs := TransformRobustness(in)
	if got == nil || *got != 0.75 || pairs != 4 {
		t.Fatalf("got %v over %d pairs, want 0.75 over 4", got, pairs)
	}
}

// TestTransformRobustnessIgnoresHalfPairs pins the transport-failure guard: a
// pair whose second half never came back (a dropped or timed-out case) is
// DROPPED, not scored. Scoring it would read a delivery failure as brittleness
// and cost an honest miner.
func TestTransformRobustnessIgnoresHalfPairs(t *testing.T) {
	in := []protocol.CaseScore{
		{CaseID: "lonely", Kind: protocol.KindMemory, TwinGroup: persona.AuditTwinPrefix + "a", Correct: true},
	}
	if got, pairs := TransformRobustness(in); got != nil || pairs != 0 {
		t.Fatalf("a half-delivered pair must not be scored; got %v over %d pairs", got, pairs)
	}
}

// TestTransformRobustnessNilWithoutAudits keeps the metric absent rather than
// defaulting to a number when no audit ran; a hard-coded 1.0 would look like a
// passing audit that never happened.
func TestTransformRobustnessNilWithoutAudits(t *testing.T) {
	in := []protocol.CaseScore{
		{CaseID: "x", Kind: protocol.KindMemory, TwinGroup: "twin-city", Correct: true},
		{CaseID: "y", Kind: protocol.KindMemory, TwinGroup: "twin-city", Correct: false},
	}
	if got, pairs := TransformRobustness(in); got != nil || pairs != 0 {
		t.Fatalf("want nil robustness with no audit groups, got %v over %d", got, pairs)
	}
}

// TestMetamorphicExcludesAuditGroups is the double-charge guard. Audit pairs
// share a TwinGroup, so without an explicit exclusion they would also feed the
// metamorphic-consistency factor and a single split would be penalized twice --
// a silent extra cost to a miner rather than a stronger defense.
func TestMetamorphicExcludesAuditGroups(t *testing.T) {
	in := []protocol.CaseScore{
		// One ordinary twin family, answered consistently.
		{CaseID: "t1", Kind: protocol.KindMemory, TwinGroup: "twin-city", Correct: true},
		{CaseID: "t2", Kind: protocol.KindMemory, TwinGroup: "twin-city", Correct: true},
	}
	in = append(in, auditPair("a", true, false)...) // a SPLIT audit pair
	rate := MetamorphicConsistency(in)
	if rate == nil || *rate != 1.0 {
		t.Fatalf("metamorphic consistency should ignore the split audit pair, got %v", rate)
	}
	if f := MetamorphicConsistencyFactor(in); f != 1.0 {
		t.Fatalf("metamorphic factor should be unaffected by audit pairs, got %.3f", f)
	}
	// The audit signal is still measured -- just in its own metric.
	if got, _ := TransformRobustness(in); got == nil || *got != 0.0 {
		t.Fatalf("the split audit pair should score 0 transform robustness, got %v", got)
	}
}

// TestMemoryOverCallFactor pins B4: taking a NON-memory action on a pure-memory
// question is penalized, a legitimate memory retrieval call is not, and an
// unobserved trajectory is never penalized (the validator cannot see it, and a
// self-report must not be able to create or hide the signal).
func TestMemoryOverCallFactor(t *testing.T) {
	mem := func(observed bool, called ...string) protocol.CaseScore {
		return protocol.CaseScore{Kind: protocol.KindMemory, Observed: observed, Called: called}
	}
	cases := []struct {
		name string
		in   []protocol.CaseScore
		want float64
	}{
		{"no observed memory cases", []protocol.CaseScore{mem(false, "gmail_send")}, 1.0},
		{"memory retrieval is not an over-call", []protocol.CaseScore{mem(true, "search_memories")}, 1.0},
		{"no calls at all", []protocol.CaseScore{mem(true)}, 1.0},
		{"every case over-calls", []protocol.CaseScore{mem(true, "gmail_send")}, 1.0 - memoryOverCallMaxPenalty},
		{"mixed retrieval and action still over-calls", []protocol.CaseScore{mem(true, "search_memories", "gmail_send")}, 1.0 - memoryOverCallMaxPenalty},
		{"half the cases over-call", []protocol.CaseScore{mem(true, "gmail_send"), mem(true, "search_memories")}, 1.0 - memoryOverCallMaxPenalty/2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MemoryOverCallFactor(tc.in); got != round6(tc.want) {
				t.Fatalf("got %.6f, want %.6f", got, round6(tc.want))
			}
		})
	}
}

// TestMemoryOverCallIgnoresToolCases guards the scope: a tool case is SUPPOSED
// to call tools, and tool-call discipline is already scored by
// ToolEfficiencyFactor. Counting tool cases here would penalize a harness twice
// for doing its job.
func TestMemoryOverCallIgnoresToolCases(t *testing.T) {
	in := []protocol.CaseScore{
		{Kind: protocol.KindTool, Observed: true, Called: []string{"gmail_send"}},
	}
	if got := MemoryOverCallFactor(in); got != 1.0 {
		t.Fatalf("tool cases must not feed the memory over-call factor, got %.3f", got)
	}
}
