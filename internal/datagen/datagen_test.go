package datagen

import (
	"testing"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// TestDeterministicPerSeed: same seed yields byte-identical datasets. Since A1
// (seed-derived time) the GeneratedAt envelope is the pinned dataset epoch, not
// a wall clock, so it is part of the reproducible dataset too.
func TestDeterministicPerSeed(t *testing.T) {
	a := Generate(42, 30)
	b := Generate(42, 30)

	if a.GeneratedAt != b.GeneratedAt || a.GeneratedAt != protocol.DatasetEpochRFC3339 {
		t.Fatalf("GeneratedAt must be the pinned epoch, deterministic per seed: a=%q b=%q want=%q",
			a.GeneratedAt, b.GeneratedAt, protocol.DatasetEpochRFC3339)
	}
	if len(a.ToolCases) != len(b.ToolCases) {
		t.Fatalf("len mismatch: %d vs %d", len(a.ToolCases), len(b.ToolCases))
	}
	for i := range a.ToolCases {
		ca, cb := a.ToolCases[i], b.ToolCases[i]
		if ca.ID != cb.ID || ca.Category != cb.Category || ca.Prompt != cb.Prompt {
			t.Fatalf("case %d differs for same seed:\n  a=%+v\n  b=%+v", i, ca, cb)
		}
		if len(ca.ExpectedTools) != len(cb.ExpectedTools) {
			t.Fatalf("case %d expected-tools differ", i)
		}
		for j := range ca.ExpectedTools {
			if ca.ExpectedTools[j].Name != cb.ExpectedTools[j].Name {
				t.Fatalf("case %d tool %d differs", i, j)
			}
		}
	}
}

// TestVarietyAcrossSeeds: different seeds produce different datasets.
func TestVarietyAcrossSeeds(t *testing.T) {
	a := Generate(1, 30)
	b := Generate(2, 30)

	same := 0
	for i := range a.ToolCases {
		if a.ToolCases[i].Prompt == b.ToolCases[i].Prompt &&
			a.ToolCases[i].Category == b.ToolCases[i].Category {
			same++
		}
	}
	if same == len(a.ToolCases) {
		t.Fatalf("seeds 1 and 2 produced identical datasets")
	}
}

// TestClampAndShape: n is clamped and cases are well-formed.
func TestClampAndShape(t *testing.T) {
	if got := len(Generate(7, 0).ToolCases); got != 1 {
		t.Fatalf("n=0 should clamp to 1, got %d", got)
	}
	if got := len(Generate(7, 500).ToolCases); got != 200 {
		t.Fatalf("n=500 should clamp to 200, got %d", got)
	}

	ds := Generate(99, 60)
	for _, c := range ds.ToolCases {
		if c.ID == "" || c.Category == "" || c.Prompt == "" {
			t.Fatalf("malformed case: %+v", c)
		}
		// no_tool / abstention categories must have no expected tools
		if (c.Category == "no_tool" || c.Category == "abstention") && len(c.ExpectedTools) != 0 {
			t.Fatalf("category %s should expect no tools: %+v", c.Category, c)
		}
		// tool categories expect >=1 tool, and MaxToolCalls tracks the sequence
		// length (1 for single-hop, >1 for multi-hop trajectories).
		if c.Category != "no_tool" && c.Category != "abstention" {
			if len(c.ExpectedTools) < 1 {
				t.Fatalf("category %s should expect a tool: %+v", c.Category, c)
			}
			if c.MaxToolCalls != len(c.ExpectedTools) {
				t.Fatalf("category %s: MaxToolCalls %d != len(ExpectedTools) %d", c.Category, c.MaxToolCalls, len(c.ExpectedTools))
			}
		}
	}
}

// TestCoversCategories: across a decent n, multiple categories appear.
func TestCoversCategories(t *testing.T) {
	ds := Generate(123, 120)
	seen := map[string]bool{}
	for _, c := range ds.ToolCases {
		seen[c.Category] = true
	}
	if len(seen) < 5 {
		t.Fatalf("expected variety of categories, only saw %d: %v", len(seen), seen)
	}
}

// TestGenerateHasMultiHopAndArgCases: A6 added multi-hop trajectories and
// exact-value argument ground truth; both must actually appear in a dataset.
func TestGenerateHasMultiHopAndArgCases(t *testing.T) {
	ds := Generate(7, 120)
	multiHop, argScored := 0, 0
	for _, c := range ds.ToolCases {
		if len(c.ExpectedTools) > 1 {
			multiHop++
		}
		for _, ts := range c.ExpectedTools {
			if len(ts.RequiredArgs) > 0 {
				argScored++
			}
		}
	}
	if multiHop == 0 {
		t.Fatal("expected some multi-hop (sequence) tool cases")
	}
	if argScored == 0 {
		t.Fatal("expected some cases with required-arg ground truth")
	}
}

// TestStratifiedBalance pins the stratification invariant: with a fixed mix, the
// per-category counts of any dataset differ by at most one (floor/ceil of n/C),
// regardless of seed. This is what removes the multinomial category-draw variance.
func TestStratifiedBalance(t *testing.T) {
	for _, seed := range []int64{1, 2, 999, 123456} {
		ds := Generate(seed, len(categories)*4) // multiple of C → perfectly balanced
		counts := map[string]int{}
		for _, c := range ds.ToolCases {
			counts[c.Category]++
		}
		lo, hi := 1<<30, 0
		for _, n := range counts {
			if n < lo {
				lo = n
			}
			if n > hi {
				hi = n
			}
		}
		if hi-lo > 1 {
			t.Fatalf("seed %d: category counts unbalanced (min=%d max=%d): %v", seed, lo, hi, counts)
		}
	}
}
