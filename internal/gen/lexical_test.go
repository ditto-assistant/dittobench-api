package gen

import (
	"context"
	"testing"
)

func TestLexicalOverlap(t *testing.T) {
	ev := "My favorite color is teal." // stored fact
	// High-overlap question: shares "favorite" + "color".
	hi := lexicalOverlap("What is my favorite color?", ev)
	// Low-overlap question: shares nothing content-bearing with the fact.
	lo := lexicalOverlap("Which shade do I gravitate toward?", ev)
	if !(hi > lo) {
		t.Fatalf("expected high-overlap (%v) > low-overlap (%v)", hi, lo)
	}
	if hi <= 0 {
		t.Fatalf("high-overlap question should share content words, got %v", hi)
	}
	// Empty inputs are 0, not a divide-by-zero.
	if lexicalOverlap("", ev) != 0 || lexicalOverlap("anything", "") != 0 {
		t.Fatal("empty inputs must yield 0 overlap")
	}
}

func TestContentTokensDropsScaffolding(t *testing.T) {
	// Only "color" survives — question words + articles + "favorite"? favorite is
	// content (not a stopword). So expect {favorite, color}.
	toks := contentTokens("What is my favorite color?")
	if toks["what"] || toks["is"] || toks["my"] {
		t.Fatalf("scaffolding words leaked: %v", toks)
	}
	if !toks["favorite"] || !toks["color"] {
		t.Fatalf("content words missing: %v", toks)
	}
}

// synonymLLM rewrites any question to a fixed low-overlap phrasing that shares no
// content words with the evidence, modeling a successful NoLiMa rewrite.
type synonymLLM struct{ reply string }

func (s synonymLLM) Complete(_ context.Context, _ string, _ string, _ string) (string, error) {
	return s.reply, nil
}

func TestRealizeQuestionLowOverlapAcceptsReduction(t *testing.T) {
	ev := "My favorite color is teal."
	orig := "What is my favorite color?"
	llm := synonymLLM{reply: "Which shade do I gravitate toward?"}
	out, _, applied := realizeQuestionLowOverlap(context.Background(), llm, "m", orig, ev, "teal")
	if !applied {
		t.Fatalf("expected a strict overlap reduction to be applied; got %q", out)
	}
	if lexicalOverlap(out, ev) >= lexicalOverlap(orig, ev) {
		t.Fatal("applied rewrite did not reduce overlap")
	}
}

func TestRealizeQuestionLowOverlapRejectsLeak(t *testing.T) {
	ev := "My favorite color is teal."
	orig := "What is my favorite color?"
	// The rewrite leaks the answer token "teal" — must be rejected, original kept.
	llm := synonymLLM{reply: "Do I prefer the teal shade?"}
	out, _, applied := realizeQuestionLowOverlap(context.Background(), llm, "m", orig, ev, "teal")
	if applied || out != orig {
		t.Fatalf("answer-leaking rewrite must be rejected; got applied=%v out=%q", applied, out)
	}
}

func TestRealizeQuestionLowOverlapRejectsNoReduction(t *testing.T) {
	ev := "My favorite color is teal."
	orig := "What is my favorite color?"
	// Same content words → no reduction → keep original.
	llm := synonymLLM{reply: "What's my favorite color, again?"}
	_, _, applied := realizeQuestionLowOverlap(context.Background(), llm, "m", orig, ev, "teal")
	if applied {
		t.Fatal("a rewrite that does not reduce overlap must not be applied")
	}
}
