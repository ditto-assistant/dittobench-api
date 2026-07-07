package gen

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// errLLM always fails — models a persistent generator outage.
type errLLM struct{ calls int }

func (e *errLLM) Complete(_ context.Context, _ string, _ string, _ string) (string, error) {
	e.calls++
	return "", errors.New("generator down")
}

// flakyUpper fails on every odd call and uppercases on every even call. Since
// completeWithRetry always makes exactly two calls when the first errors, each
// paraphrase attempt sees (err, then UPPER) — exercising the retry-once path.
type flakyUpper struct{ calls int }

func (f *flakyUpper) Complete(_ context.Context, _ string, _ string, user string) (string, error) {
	f.calls++
	if f.calls%2 == 1 {
		return "", errors.New("transient")
	}
	return strings.ToUpper(user), nil
}

func TestPreservesNumbers(t *testing.T) {
	cases := []struct {
		orig, rewritten string
		want            bool
	}{
		{"I have 3 cats since 2019", "in 2019 I got 3 cats", true},
		{"I have 3 cats since 2019", "since 2019 I keep three cats", false}, // 3 dropped
		{"no digits at all here", "a totally reworded sentence", true},      // nothing to preserve
		{"flight AA100 at 10:30", "catch AA 100 at 10 30", true},            // runs preserved, punctuation irrelevant
		{"paid $1500 rent", "rent was 150 bucks", false},                    // 1500 mutated
	}
	for _, c := range cases {
		if got := preservesNumbers(c.orig, c.rewritten); got != c.want {
			t.Errorf("preservesNumbers(%q,%q)=%v want %v", c.orig, c.rewritten, got, c.want)
		}
	}
}

func TestPreservesEntity(t *testing.T) {
	cases := []struct {
		filler, rewritten string
		want              bool
	}{
		{"my flight to Tokyo", "the Tokyo flight I mentioned", true},
		{"my flight to Tokyo", "my trip abroad", false}, // flight, tokyo dropped
		{"quantum computing", "tell me about QUANTUM COMPUTING", true},
		{"https://example.com/article", "open https://example.com/article please", true},
		{"https://example.com/article", "open that link please", false},
		{"dark", "switch to dark mode", true},
		{"the 2024 Olympics", "news on the 2024 olympics", true},
		{"the 2024 Olympics", "news on the olympics", false}, // 2024 dropped
	}
	for _, c := range cases {
		if got := preservesEntity(c.filler, c.rewritten); got != c.want {
			t.Errorf("preservesEntity(%q,%q)=%v want %v", c.filler, c.rewritten, got, c.want)
		}
	}
}

// TestGenerateToolsErrLLMFallsBackCounted: a persistent generator error must not
// silently collapse to verbatim. Every prompt stays the template AND
// every skip is counted as a retried fallback.
func TestGenerateToolsErrLLMFallsBackCounted(t *testing.T) {
	base, _ := GenerateTools(context.Background(), NewRNG(99), 99, 20, 0, nil, "m")
	e := &errLLM{}
	out, stats := GenerateTools(context.Background(), NewRNG(99), 99, 20, 1.0, e, "m")

	if len(base) != len(out) {
		t.Fatalf("case count changed: %d vs %d", len(base), len(out))
	}
	for i := range base {
		if base[i].Prompt != out[i].Prompt {
			t.Fatalf("case %d prompt changed despite total LLM failure: %q -> %q", i, base[i].Prompt, out[i].Prompt)
		}
	}
	if stats.Attempted != 20 || stats.Fallback != 20 || stats.Applied != 0 || stats.Retried != 20 {
		t.Fatalf("unexpected stats on total failure: %+v", stats)
	}
	if e.calls != 40 { // two calls per attempt (call + one retry)
		t.Fatalf("expected 40 LLM calls (retry once each), got %d", e.calls)
	}
}

// TestGenerateToolsRetrySucceeds: an error followed by a valid rewrite is
// retried and, when the entity survives, applied — never lost to the transient.
func TestGenerateToolsRetrySucceeds(t *testing.T) {
	out, stats := GenerateTools(context.Background(), NewRNG(99), 99, 20, 1.0, &flakyUpper{}, "m")
	if stats.Attempted != 20 || stats.Retried != 20 || stats.Applied != 20 || stats.Fallback != 0 {
		t.Fatalf("expected all retried+applied, got %+v", stats)
	}
	if stats.Attempted != stats.Applied+stats.Fallback {
		t.Fatalf("stats invariant broken: %+v", stats)
	}
	// uppercase actually changed the prompts (paraphrase took effect)
	base, _ := GenerateTools(context.Background(), NewRNG(99), 99, 20, 0, nil, "m")
	changed := 0
	for i := range out {
		if out[i].Prompt != base[i].Prompt {
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("expected retried paraphrases to change prompts")
	}
}

// TestGenerateMemoryErrLLMFallbacksCounted: the memory pair + question paraphrase
// paths also count fallbacks and retry, rather than silently keeping verbatim.
func TestGenerateMemoryErrLLMFallbacksCounted(t *testing.T) {
	_, _, stats, err := GenerateMemory(context.Background(), NewRNG(7), 6, 20, 1.0, &errLLM{}, "m", "", "")
	if err != nil {
		t.Fatalf("GenerateMemory: %v", err)
	}
	if stats.Attempted == 0 {
		t.Fatal("expected paraphrase attempts at frac=1.0")
	}
	if stats.Applied != 0 || stats.Fallback != stats.Attempted || stats.Retried != stats.Attempted {
		t.Fatalf("expected all attempts to retry then fall back, got %+v", stats)
	}
}
