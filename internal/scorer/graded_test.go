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
		{"no", "I know nothing about your plans", false},     // common word → defer to judge
		{"may", "you may not have that information", false},  // common modal → defer
		{"Ann", "your planner is annoyingly complex", false}, // not inside "annoyingly"
		{"blue", "it was blue, actually", true},              // distinctive; trailing punct is a boundary
		{"James Webb", "the james webb telescope", true},     // multi-word phrase
		{"5", "about 3.5 miles", false},                      // decimal boundary: 5 != 3.5
		{"5", "you have 5 cats", true},
		{"5", "temperature dropped to -5 today", false},    // negation: -5 is not 5
		{"100", "you owe -100 dollars", false},             // negation: -100 is not 100
		{"42", "see ticket order-42-x for details", false}, // number inside a hyphenated token
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
	cs, _ := GradeMemory(context.Background(), f, JudgeConfig{Model: "m"}, mc, protocol.RunResponse{FinalText: "it was blue", LatencyMs: 7})
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
		cs, _ := GradeMemory(context.Background(), &fakeLLM{reply: tc.reply}, JudgeConfig{Model: "m"}, mc, protocol.RunResponse{FinalText: "some other answer"})
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
	cs, _ := GradeMemory(context.Background(), f, JudgeConfig{Model: "m"}, mc, protocol.RunResponse{FinalText: "I don't have that information."})
	if !approx(cs.Score, 1.0) || f.calls != 1 {
		t.Fatalf("clean decline should score 1.0 via the judge: score=%v calls=%d", cs.Score, f.calls)
	}
	// Fabrication → correct=no grounded=no → 0.0.
	cs, _ = GradeMemory(context.Background(), &fakeLLM{reply: `{"correct":"no","grounded":"no"}`}, JudgeConfig{Model: "m"}, mc, protocol.RunResponse{FinalText: "It's X1234567."})
	if !approx(cs.Score, 0.0) {
		t.Fatalf("fabrication should score 0.0, got %v", cs.Score)
	}
}

func TestGradeMemoryEmptyNoJudge(t *testing.T) {
	f := &fakeLLM{err: errors.New("no")}
	cs, _ := GradeMemory(context.Background(), f, JudgeConfig{Model: "m"},
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

func TestGradeMemoryReportsJudgeOutage(t *testing.T) {
	// A judge availability error must surface as Attempted+Errored (so the run
	// loop can fail the whole run) — retried once first (2 calls).
	f := &fakeLLM{err: errors.New("openrouter 503")}
	mc := protocol.MemoryCase{ID: "m1", QuestionType: "multi-session", ExpectedAnswer: "blue", Question: "q"}
	cs, jo := GradeMemory(context.Background(), f, JudgeConfig{Model: "m"}, mc, protocol.RunResponse{FinalText: "some answer"})
	if !jo.Attempted || !jo.Errored {
		t.Fatalf("judge error should surface as Attempted+Errored: %+v", jo)
	}
	if cs.Score != 0 { // fail-closed per case still holds
		t.Fatalf("errored judge should fail-closed to 0 per case, got %v", cs.Score)
	}
	if f.calls != 2 { // one retry
		t.Fatalf("judge should retry once on error (2 calls), got %d", f.calls)
	}
}

func TestGradeMemoryIsolationLeakScoresZero(t *testing.T) {
	// A response that surfaces the OTHER user's conflicting value has leaked across
	// graphs and must score 0 deterministically — even though it also contains the
	// queried user's own correct answer ("blue"). The judge is not consulted.
	f := &fakeLLM{reply: `{"correct":"yes","grounded":"yes"}`}
	mc := protocol.MemoryCase{
		ID: "iso-a-0001-color", QuestionType: "isolation",
		Question: "what's my favorite color?", ExpectedAnswer: "blue", ForbiddenAnswer: "red",
	}
	cs, _ := GradeMemory(context.Background(), f, JudgeConfig{Model: "m"},
		mc, protocol.RunResponse{FinalText: "Your favorite color is blue (your colleague's is red)."})
	if cs.Score != 0 || cs.Correct {
		t.Fatalf("cross-graph leak should score 0, got %+v", cs)
	}
	if f.calls != 0 {
		t.Fatalf("deterministic leak check should not call the judge (calls=%d)", f.calls)
	}
}

func TestGradeMemoryIsolationCleanAnswerScores(t *testing.T) {
	// No forbidden value present: the isolation case falls through to the judge and
	// a correct, grounded answer scores full marks.
	f := &fakeLLM{reply: `{"correct":"yes","grounded":"yes"}`}
	mc := protocol.MemoryCase{
		ID: "iso-a-0001-color", QuestionType: "isolation",
		Question: "what's my favorite color?", ExpectedAnswer: "blue", ForbiddenAnswer: "red",
	}
	cs, _ := GradeMemory(context.Background(), f, JudgeConfig{Model: "m"},
		mc, protocol.RunResponse{FinalText: "Your favorite color is blue."})
	if !approx(cs.Score, 1.0) || !cs.Correct {
		t.Fatalf("clean isolation answer should score 1.0, got %+v", cs)
	}
	if f.calls == 0 {
		t.Fatalf("isolation case with no leak should consult the judge")
	}
}

func TestJudgeMemoryErrorEnvelopeIsOutage(t *testing.T) {
	// A 200 response whose body is valid JSON but carries no verdict (a provider
	// {"error":...} envelope) must surface as Errored, not a silent "incorrect" —
	// otherwise a persistent content-level judge failure records 0s as valid.
	f := &fakeLLM{reply: `{"error":{"message":"rate limited","code":429}}`}
	v := JudgeMemoryGraded(context.Background(), f, "m", "q", "blue", "some answer", "multi-session")
	if !v.Errored {
		t.Fatalf("verdict-less JSON envelope should be Errored, got %+v", v)
	}
}

func TestGradeMemoryNoJudgeOutcomeOnDeterministicHit(t *testing.T) {
	// Deterministic hit and empty response never invoke the judge → not Attempted,
	// so they never count toward the outage gate.
	f := &fakeLLM{err: errors.New("should not be called")}
	mc := protocol.MemoryCase{ID: "m1", QuestionType: "multi-session", ExpectedAnswer: "blue", Question: "q"}
	_, jo := GradeMemory(context.Background(), f, JudgeConfig{Model: "m"}, mc, protocol.RunResponse{FinalText: "it was blue"})
	if jo.Attempted {
		t.Fatalf("deterministic hit must not mark the judge as attempted: %+v", jo)
	}
}

func TestGradeToolQualityReportsJudgeOutage(t *testing.T) {
	f := &fakeLLM{err: errors.New("openrouter 503")}
	_, _, jo := GradeToolQuality(context.Background(), f, JudgeConfig{Model: "m"}, "c1", "p", []string{"search_web"}, "b", "resp")
	if !jo.Attempted || !jo.Errored {
		t.Fatalf("tool judge error should surface as Attempted+Errored: %+v", jo)
	}
	// empty response → no judge call, not attempted
	_, _, jo2 := GradeToolQuality(context.Background(), f, JudgeConfig{Model: "m"}, "c2", "p", nil, "b", "  ")
	if jo2.Attempted {
		t.Fatalf("empty response must not attempt the judge: %+v", jo2)
	}
}
