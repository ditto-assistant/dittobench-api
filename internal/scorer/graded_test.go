package scorer

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDeterministicMemoryHit(t *testing.T) {
	cases := []struct {
		exp, resp string
		want      bool
	}{
		{"Sarah", "your friend Sarah called yesterday", true},
		{"Sarah", "no one called", false},
		{"Tokyo", "you flew to tokyo in may", true}, // case-insensitive
		{"5", "you have 5 cats", true},
		{"5", "you have 500 dollars", false}, // number-token boundary
		{"3.5", "about 3.5 miles", true},
		{"", "anything at all", false},
	}
	for _, c := range cases {
		if got := deterministicMemoryHit(c.exp, c.resp); got != c.want {
			t.Errorf("deterministicMemoryHit(%q,%q)=%v want %v", c.exp, c.resp, got, c.want)
		}
	}
}

func TestGradeMemoryDeterministicNoJudge(t *testing.T) {
	f := &fakeLLM{err: errors.New("judge must not be called")}
	mc := protocol.MemoryCase{ID: "m1", QuestionType: "multi-session", ExpectedAnswer: "blue", Question: "what color?"}
	cs := GradeMemory(context.Background(), f, "m", mc, protocol.RunResponse{FinalText: "it was blue", LatencyMs: 7})
	if !approx(cs.Score, 1.0) || !cs.Correct {
		t.Fatalf("deterministic hit should score 1.0 correct: %+v", cs)
	}
	if f.calls != 0 {
		t.Fatalf("judge should not be called on a deterministic hit (calls=%d)", f.calls)
	}
}

func TestGradeMemoryGradedCredit(t *testing.T) {
	mc := protocol.MemoryCase{ID: "m1", QuestionType: "multi-session", ExpectedAnswer: "blue", Question: "q"}
	tests := []struct {
		reply string
		want  float64
	}{
		{`{"correct":"yes","grounded":"yes"}`, 1.0},
		{`{"correct":"yes","grounded":"no"}`, 0.7},
		{`{"correct":"no","grounded":"yes"}`, 0.3},
		{`{"correct":"no","grounded":"no"}`, 0.0},
	}
	for _, tc := range tests {
		// FinalText that does NOT deterministically contain "blue" → judge path.
		cs := GradeMemory(context.Background(), &fakeLLM{reply: tc.reply}, "m", mc, protocol.RunResponse{FinalText: "some other answer"})
		if !approx(cs.Score, tc.want) {
			t.Fatalf("reply %s: score %v want %v", tc.reply, cs.Score, tc.want)
		}
	}
}

func TestGradeMemoryAbstentionAlwaysJudged(t *testing.T) {
	mc := protocol.MemoryCase{ID: "a1", QuestionType: "abstention", ExpectedAnswer: abstentionMarker(), Question: "what is my passport number?"}

	// Clean decline → judged correct+grounded → 1.0. Judge IS called even though
	// the expected marker text is not present in the response.
	f := &fakeLLM{reply: `{"correct":"yes","grounded":"yes"}`}
	cs := GradeMemory(context.Background(), f, "m", mc, protocol.RunResponse{FinalText: "I don't have that information."})
	if !approx(cs.Score, 1.0) || f.calls != 1 {
		t.Fatalf("clean decline should score 1.0 via the judge: score=%v calls=%d", cs.Score, f.calls)
	}
	// Fabrication → correct=no grounded=no → 0.0.
	cs = GradeMemory(context.Background(), &fakeLLM{reply: `{"correct":"no","grounded":"no"}`}, "m", mc, protocol.RunResponse{FinalText: "It's X1234567."})
	if !approx(cs.Score, 0.0) {
		t.Fatalf("fabrication should score 0.0, got %v", cs.Score)
	}
}

func TestGradeMemoryEmptyNoJudge(t *testing.T) {
	f := &fakeLLM{err: errors.New("no")}
	cs := GradeMemory(context.Background(), f, "m",
		protocol.MemoryCase{ID: "m", QuestionType: "multi-session", ExpectedAnswer: "x"},
		protocol.RunResponse{FinalText: "   "})
	if cs.Score != 0 || cs.Correct {
		t.Fatalf("empty response should score 0: %+v", cs)
	}
	if f.calls != 0 {
		t.Fatalf("empty response should not call the judge (calls=%d)", f.calls)
	}
}

// abstentionMarker is a stand-in for the gen-side marker (any text the response
// won't contain, so the deterministic check misses and the judge decides).
func abstentionMarker() string {
	return "(no information about this was ever provided)"
}
