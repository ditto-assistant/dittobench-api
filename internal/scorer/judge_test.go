package scorer

import (
	"context"
	"strings"
	"testing"
)

// fakeLLM returns a canned reply (or error), records the last system/user, and
// counts calls (so tests can assert the judge was or wasn't invoked).
type fakeLLM struct {
	reply      string
	err        error
	lastSystem string
	lastUser   string
	calls      int
}

func (f *fakeLLM) Complete(_ context.Context, _ string, system, user string) (string, error) {
	f.calls++
	f.lastSystem, f.lastUser = system, user
	return f.reply, f.err
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		in   string
		want bool // whether a map should be returned
	}{
		{`{"correct":"yes"}`, true},
		{"```json\n{\"correct\":\"no\"}\n```", true},
		{"sure, here: {\"helpfulness\":5,\"accuracy\":4} done", true},
		{"no json here", false},
		{"}{", false},
	}
	for _, c := range cases {
		got := extractJSON(c.in)
		if (got != nil) != c.want {
			t.Errorf("extractJSON(%q): got %v want present=%v", c.in, got, c.want)
		}
	}
}

func TestDim(t *testing.T) {
	if v, ok := dim(map[string]any{"helpfulness": float64(4)}, "helpfulness"); !ok || v != 4 {
		t.Fatalf("number dim: got %v %v", v, ok)
	}
	if v, ok := dim(map[string]any{"accuracy": "5"}, "accuracy"); !ok || v != 5 {
		t.Fatalf("string dim: got %v %v", v, ok)
	}
	if _, ok := dim(map[string]any{"x": float64(0)}, "x"); ok {
		t.Fatalf("zero dim should be absent")
	}
	if _, ok := dim(map[string]any{}, "missing"); ok {
		t.Fatalf("missing dim should be absent")
	}
}

func TestBuildLMEJudgeSystemClauses(t *testing.T) {
	if !strings.Contains(buildLMEJudgeSystem("temporal-reasoning", false), "off-by-one") {
		t.Fatal("temporal clause missing")
	}
	if !strings.Contains(buildLMEJudgeSystem("knowledge-update", false), "outdated and updated") {
		t.Fatal("knowledge-update clause missing")
	}
	if !strings.Contains(buildLMEJudgeSystem("single-session-preference", false), "preference") {
		t.Fatal("preference clause missing")
	}
	if !strings.Contains(buildLMEJudgeSystem("multi-session", true), "ABSTENTION") {
		t.Fatal("abstention clause missing")
	}
	// plain type, not abstention → base prompt + the untrusted-data guard.
	if got := buildLMEJudgeSystem("multi-session", false); got != lmeJudgeBase+untrustedGuard {
		t.Fatalf("plain type should be base prompt + guard, got %q", got)
	}
}

func TestJudgeMemory(t *testing.T) {
	ctx := context.Background()
	// empty response → false without calling the LLM
	if JudgeMemory(ctx, &fakeLLM{reply: `{"correct":"yes"}`}, "m", "q", "a", "", "multi-session") {
		t.Fatal("empty response must be false")
	}
	// yes verdict
	if !JudgeMemory(ctx, &fakeLLM{reply: `{"correct":"yes"}`}, "m", "q", "a", "resp", "multi-session") {
		t.Fatal("yes verdict should be true")
	}
	// no verdict
	if JudgeMemory(ctx, &fakeLLM{reply: `{"correct":"no"}`}, "m", "q", "a", "resp", "multi-session") {
		t.Fatal("no verdict should be false")
	}
	// judge error → false
	if JudgeMemory(ctx, &fakeLLM{err: context.Canceled}, "m", "q", "a", "resp", "multi-session") {
		t.Fatal("judge error should be false")
	}
}

func TestJudgeMemoryAbstentionRouting(t *testing.T) {
	ctx := context.Background()
	// A clean decline, judged under the abstention clause, is correct.
	f := &fakeLLM{reply: `{"correct":"yes"}`}
	if !JudgeMemory(ctx, f, "m", "what is my passport number?", "(no info)", "I don't have that information.", "abstention") {
		t.Fatal("clean decline should be judged correct")
	}
	if !strings.Contains(f.lastSystem, "ABSTENTION") {
		t.Fatalf("abstention question_type must route to the abstention clause; system=%q", f.lastSystem)
	}
	// A fabricated answer is judged incorrect.
	f2 := &fakeLLM{reply: `{"correct":"no"}`}
	if JudgeMemory(ctx, f2, "m", "what is my passport number?", "(no info)", "It's X1234567.", "abstention") {
		t.Fatal("fabricated answer should be judged incorrect")
	}
}

func TestJudgeToolQuality(t *testing.T) {
	ctx := context.Background()
	// empty response → 0 without calling the LLM
	if q := JudgeToolQuality(ctx, &fakeLLM{reply: `{"helpfulness":5,"accuracy":5}`}, "m", "p", nil, "b", ""); q != 0 {
		t.Fatalf("empty response should be 0, got %v", q)
	}
	// 4 + 4 → mean 4 / 5 = 0.8
	if q := JudgeToolQuality(ctx, &fakeLLM{reply: `{"helpfulness":4,"accuracy":4}`}, "m", "p", []string{"search_web"}, "b", "resp"); q != 0.8 {
		t.Fatalf("expected 0.8, got %v", q)
	}
	// missing dim → uses only present one (5 → 1.0)
	if q := JudgeToolQuality(ctx, &fakeLLM{reply: `{"helpfulness":5}`}, "m", "p", nil, "b", "resp"); q != 1.0 {
		t.Fatalf("single dim 5 should be 1.0, got %v", q)
	}
	// judge error → 0
	if q := JudgeToolQuality(ctx, &fakeLLM{err: context.Canceled}, "m", "p", nil, "b", "resp"); q != 0 {
		t.Fatalf("judge error should be 0, got %v", q)
	}
}

func TestToolJudgeFormatsToolsNone(t *testing.T) {
	f := &fakeLLM{reply: `{"helpfulness":3,"accuracy":3}`}
	JudgeToolQuality(context.Background(), f, "m", "p", nil, "b", "resp")
	if !strings.Contains(f.lastUser, "TOOLS CALLED (self-reported)") || !strings.Contains(f.lastUser, "none") {
		t.Fatalf("no tools should render a fenced 'none', got user=%q", f.lastUser)
	}
	// Untrusted harness output must be fenced, not inlined as bare instructions.
	if !strings.Contains(f.lastUser, "UNTRUSTED ASSISTANT RESPONSE") {
		t.Fatalf("assistant response should be fenced as untrusted, got %q", f.lastUser)
	}
}
