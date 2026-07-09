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

// GenerateTools is now LLM-free and deterministic (its prompts are template
// phrasing variants chosen by the seeded rng), so the former tool-paraphrase
// tests (retry/fallback/applied) were removed with the paraphrase pass. Its
// determinism is covered by TestGenerateToolsDeterministic; the pair/question
// paraphrase machinery below still backs the memory path.

// TestGenerateToolsDeterministic: the same seed yields byte-identical tool
// prompts across runs, with no LLM involved.
func TestGenerateToolsDeterministic(t *testing.T) {
	a, _ := GenerateTools(NewRNG(99), 99, 20)
	b, _ := GenerateTools(NewRNG(99), 99, 20)
	if len(a) != len(b) {
		t.Fatalf("case count differs across runs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Prompt != b[i].Prompt {
			t.Fatalf("case %d prompt not deterministic: %q vs %q", i, a[i].Prompt, b[i].Prompt)
		}
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
