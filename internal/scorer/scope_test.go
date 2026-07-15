package scorer

import (
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// observableToolCase is a validator-served, observable tool case (its expected
// tool is a real external-world tool the mock endpoint serves).
func observableToolCase() protocol.ToolCase {
	return protocol.ToolCase{
		ID:            "web_search-9-0",
		Category:      "web_search",
		ExpectedTools: []protocol.ToolSpec{{Name: "search_web"}},
		MaxToolCalls:  1,
	}
}

// In SCORED scope an observable tool case the validator did NOT observe execute
// scores 0 — the free 0.5 self-report ceiling is gone. Practice still caps at 0.5.
func TestCapUnobservedScope_ScoredZeroPracticeCapped(t *testing.T) {
	c := observableToolCase()
	// Perfect self-report, but nothing observed.
	self := protocol.RunResponse{FinalText: "ok", ToolCalls: []protocol.ObservedToolCall{{Name: "search_web"}}}

	// Practice: capped at the 0.5 ceiling (unchanged behavior).
	pc := FinishTool(ScoreToolCaseObservedScope(c, self, true, nil, ScopePractice))
	pc = CapUnobservedScope(pc, ScopePractice)
	if pc.Score != UnobservedCeiling {
		t.Fatalf("practice unobserved observable should cap at %v, got %v", UnobservedCeiling, pc.Score)
	}

	// Scored: the same unobserved observable case scores 0.
	sc := FinishTool(ScoreToolCaseObservedScope(c, self, true, nil, ScopeScored))
	sc = CapUnobservedScope(sc, ScopeScored)
	if sc.Score != 0 {
		t.Fatalf("scored unobserved observable must score 0, got %v", sc.Score)
	}
}

// An OBSERVED case is unaffected by scope: the ceiling only bites on the
// unobserved-observable fallback, so a scored run that actually observed the
// right call still earns full trajectory credit.
func TestCapUnobservedScope_ObservedUnaffected(t *testing.T) {
	c := observableToolCase()
	observed := []protocol.ObservedToolCall{{Name: "search_web", Hop: 0}}
	cs := FinishTool(ScoreToolCaseObservedScope(c, protocol.RunResponse{}, true, observed, ScopeScored))
	if cs.ToolScore == 0 {
		t.Fatalf("observed correct call should score > 0 even scored, got %v", cs.ToolScore)
	}
	// The cap is only applied by the caller for the UNobserved branch; an observed
	// case never routes through CapUnobservedScope, but even if it did the full
	// score would be zeroed only when unobserved — assert the ceiling helper value.
	if UnobservedCeilingFor(ScopeScored) != 0 || UnobservedCeilingFor(ScopePractice) != UnobservedCeiling {
		t.Fatalf("ceiling helper wrong: scored=%v practice=%v",
			UnobservedCeilingFor(ScopeScored), UnobservedCeilingFor(ScopePractice))
	}
}

// Memory-routing free point: a memory-tool case answered with only a non-empty
// FinalText (no memory-tool call) earns 1.0 in practice but 0 when scored.
func TestMemoryRoutingScope_FreePointClosedWhenScored(t *testing.T) {
	c := protocol.ToolCase{
		ID:            "memq-1",
		Category:      "route_memory",
		ExpectedTools: []protocol.ToolSpec{{Name: "search_memories"}},
	}
	// Harness produced an answer string but called NO memory tool.
	textOnly := protocol.RunResponse{FinalText: "Your favorite color is blue."}

	practice := ScoreToolCaseScope(c, textOnly, true, ScopePractice)
	if practice.ToolScore != 1.0 {
		t.Fatalf("practice memory-routing with answer text should score 1.0, got %v", practice.ToolScore)
	}

	scored := ScoreToolCaseScope(c, textOnly, true, ScopeScored)
	if scored.ToolScore != 0 {
		t.Fatalf("scored memory-routing with answer-only (no memory-tool call) must score 0, got %v", scored.ToolScore)
	}

	// A real memory-tool call earns credit in BOTH scopes.
	withCall := protocol.RunResponse{
		FinalText: "Your favorite color is blue.",
		ToolCalls: []protocol.ObservedToolCall{{Name: "search_memories"}},
	}
	if got := ScoreToolCaseScope(c, withCall, true, ScopeScored); got.ToolScore != 1.0 {
		t.Fatalf("scored memory-routing WITH a memory-tool call should score 1.0, got %v", got.ToolScore)
	}
	// A misroute to a non-memory tool still zeroes in scored scope.
	misroute := protocol.RunResponse{ToolCalls: []protocol.ObservedToolCall{{Name: "search_web"}}}
	if got := ScoreToolCaseScope(c, misroute, true, ScopeScored); got.ToolScore != 0 {
		t.Fatalf("scored memory-routing misroute must score 0, got %v", got.ToolScore)
	}
}

// The exported practice wrappers must remain identical to the scoped-practice
// path (backward-compatible defaults).
func TestPracticeWrappersUnchanged(t *testing.T) {
	c := observableToolCase()
	self := protocol.RunResponse{ToolCalls: []protocol.ObservedToolCall{{Name: "search_web"}}}
	a := ScoreToolCase(c, self, true)
	b := ScoreToolCaseScope(c, self, true, ScopePractice)
	if a.ToolScore != b.ToolScore {
		t.Fatalf("ScoreToolCase must equal practice-scope: %v vs %v", a.ToolScore, b.ToolScore)
	}
	hi := protocol.CaseScore{Score: 1.0}
	if CapUnobserved(hi).Score != CapUnobservedScope(hi, ScopePractice).Score {
		t.Fatalf("CapUnobserved must equal practice-scope cap")
	}
}
