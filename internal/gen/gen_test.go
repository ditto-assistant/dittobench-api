package gen

import (
	"context"
	"strings"
	"testing"
)

// upperLLM uppercases the user message — a deterministic stand-in for the
// paraphrase model that always "rewrites".
type upperLLM struct{ calls int }

func (u *upperLLM) Complete(_ context.Context, _ string, _ string, user string) (string, error) {
	u.calls++
	return strings.ToUpper(user), nil
}

func TestProfiles(t *testing.T) {
	for _, size := range []string{"small", "medium", "full"} {
		p, ok := ProfileFor(size)
		if !ok {
			t.Fatalf("profile %q should exist", size)
		}
		if p.Tools <= 0 || p.Mem <= 0 || p.Distractors <= 0 {
			t.Fatalf("profile %q has non-positive counts: %+v", size, p)
		}
		if p.ParaphraseFrac < 0 || p.ParaphraseFrac > 1 {
			t.Fatalf("profile %q paraphrase frac out of range: %v", size, p.ParaphraseFrac)
		}
	}
	// small must be the cheapest
	s, _ := ProfileFor("small")
	f, _ := ProfileFor("full")
	if s.Tools >= f.Tools || s.Mem >= f.Mem {
		t.Fatalf("small should be cheaper than full")
	}
	// unknown size falls back to small
	if p, ok := ProfileFor("xl"); ok || p != Profiles["small"] {
		t.Fatalf("unknown size should fall back to small (ok=false)")
	}
}

func TestFreshSeedUnique(t *testing.T) {
	seen := map[int64]bool{}
	for i := 0; i < 1000; i++ {
		s := FreshSeed()
		if s < 0 {
			t.Fatalf("seed should be non-negative, got %d", s)
		}
		if seen[s] {
			t.Fatalf("duplicate seed %d in 1000 draws", s)
		}
		seen[s] = true
	}
}

func TestGenerateToolsNilLLM(t *testing.T) {
	r := NewRNG(7)
	cases, _ := GenerateTools(context.Background(), r, 7, 10, 0.5, nil, "model")
	if len(cases) != 10 {
		t.Fatalf("expected 10 cases, got %d", len(cases))
	}
	for _, c := range cases {
		if c.ID == "" || c.Prompt == "" || c.Category == "" {
			t.Fatalf("malformed case: %+v", c)
		}
	}
}

func TestGenerateToolsParaphrasePreservesGroundTruth(t *testing.T) {
	// frac=1 → every prompt rewritten; ground truth (expected_tools/behavior)
	// must be unchanged vs the nil-LLM baseline.
	base, _ := GenerateTools(context.Background(), NewRNG(99), 99, 12, 0, nil, "m")
	llm := &upperLLM{}
	para, _ := GenerateTools(context.Background(), NewRNG(99), 99, 12, 1.0, llm, "m")

	if len(base) != len(para) {
		t.Fatalf("length mismatch")
	}
	if llm.calls == 0 {
		t.Fatal("paraphrase LLM was never called with frac=1")
	}
	changed := 0
	for i := range base {
		if base[i].ID != para[i].ID || base[i].Category != para[i].Category {
			t.Fatalf("case %d identity changed", i)
		}
		// expected tools preserved
		if len(base[i].ExpectedTools) != len(para[i].ExpectedTools) {
			t.Fatalf("case %d expected tools changed", i)
		}
		for j := range base[i].ExpectedTools {
			if base[i].ExpectedTools[j].Name != para[i].ExpectedTools[j].Name {
				t.Fatalf("case %d tool %d changed", i, j)
			}
		}
		if base[i].ExpectedBehavior != para[i].ExpectedBehavior {
			t.Fatalf("case %d expected behavior changed", i)
		}
		if base[i].Prompt != para[i].Prompt {
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("frac=1 should have changed at least some prompts")
	}
}

func TestSanitizeParaphrase(t *testing.T) {
	if got := sanitizeParaphrase(`"hello there"`); got != "hello there" {
		t.Fatalf("quotes not stripped: %q", got)
	}
	if got := sanitizeParaphrase("  spaced  "); got != "spaced" {
		t.Fatalf("not trimmed: %q", got)
	}
	if got := sanitizeParaphrase("x"); got != "" {
		t.Fatalf("too short should be empty, got %q", got)
	}
}

// TestGenerateMemory exercises the embedded seed bundle (always present, so the
// test is hermetic — no dependency on local on-disk assets).
func TestGenerateMemory(t *testing.T) {
	// Empty seedDir/oracle => embedded bundle.
	r := NewRNG(12345)
	seedReq, cases, _, err := GenerateMemory(context.Background(), r, 5, 20, 0, nil, "m", "", "")
	if err != nil {
		t.Fatalf("GenerateMemory: %v", err)
	}
	if len(cases) != 5 {
		t.Fatalf("expected 5 memory cases, got %d", len(cases))
	}
	if len(seedReq.Pairs) == 0 {
		t.Fatal("expected a non-empty haystack")
	}
	if seedReq.UserID != "miner" {
		t.Fatalf("expected default user 'miner', got %q", seedReq.UserID)
	}
	for _, c := range cases {
		if c.ID == "" || c.QuestionID == "" || c.Question == "" {
			t.Fatalf("malformed memory case: %+v", c)
		}
	}
	// pairs carry fresh RFC3339 timestamps
	for _, p := range seedReq.Pairs {
		if p.Timestamp == "" || p.PairID == "" {
			t.Fatalf("malformed pair: %+v", p)
		}
	}
}

func TestGenerateMemoryMissingAssets(t *testing.T) {
	_, _, _, err := GenerateMemory(context.Background(), NewRNG(1), 5, 10, 0, nil, "m", "/no/such/seeddir", "/no/such/oracle.json")
	if err == nil {
		t.Fatal("expected a clear error for missing assets, got nil")
	}
	if !strings.Contains(err.Error(), "seed dir") {
		t.Fatalf("error should mention the missing seed dir, got %v", err)
	}
}
