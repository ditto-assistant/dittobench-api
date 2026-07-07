package gen

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// marshal renders a value to canonical JSON for byte-level comparison.
func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestMemoryPlanByteIdenticalPerSeed is the golden test: with wall-clock
// time removed (protocol.DatasetEpoch) and map iteration sorted, the plan layer
// (the /seed haystack + the memory cases) is a pure function of the seed. Two
// GenerateMemory runs at the same seed must be byte-identical. nil LLM keeps
// this to the plan layer (no surface-realization LLM) per the reproducibility
// contract.
func TestMemoryPlanByteIdenticalPerSeed(t *testing.T) {
	const seed = 20260706
	seedA, casesA, _, err := GenerateMemory(context.Background(), NewRNG(seed), 8, 25, 0, nil, "m", "", "")
	if err != nil {
		t.Fatalf("GenerateMemory A: %v", err)
	}
	seedB, casesB, _, err := GenerateMemory(context.Background(), NewRNG(seed), 8, 25, 0, nil, "m", "", "")
	if err != nil {
		t.Fatalf("GenerateMemory B: %v", err)
	}

	if got, want := marshal(t, seedA), marshal(t, seedB); got != want {
		t.Fatalf("SeedRequest not byte-identical for the same seed:\n a=%s\n b=%s", got, want)
	}
	if got, want := marshal(t, casesA), marshal(t, casesB); got != want {
		t.Fatalf("MemoryCases not byte-identical for the same seed:\n a=%s\n b=%s", got, want)
	}

	// The haystack must be non-trivial, otherwise byte-identity proves nothing.
	// (Absolute timestamps legitimately span a window around the epoch — the
	// session offset advances past it — so the anti-wall-clock guarantee comes
	// from the byte-identity above, not from an upper bound here.)
	if len(seedA.Pairs) == 0 {
		t.Fatal("expected a non-empty haystack")
	}
}

// TestMemoryPlanVariesAcrossSeeds guards the flip side: determinism must not
// collapse the datasets to a constant — different seeds still differ.
func TestMemoryPlanVariesAcrossSeeds(t *testing.T) {
	seedA, _, _, err := GenerateMemory(context.Background(), NewRNG(1), 8, 25, 0, nil, "m", "", "")
	if err != nil {
		t.Fatalf("GenerateMemory seed 1: %v", err)
	}
	seedB, _, _, err := GenerateMemory(context.Background(), NewRNG(2), 8, 25, 0, nil, "m", "", "")
	if err != nil {
		t.Fatalf("GenerateMemory seed 2: %v", err)
	}
	if marshal(t, seedA) == marshal(t, seedB) {
		t.Fatal("seeds 1 and 2 produced identical haystacks")
	}
}

// TestDatasetEpochPinned pins the epoch's rendered form so an accidental change
// (which would silently break score comparability across the ledger) trips a
// test rather than shipping.
func TestDatasetEpochPinned(t *testing.T) {
	if !strings.HasPrefix(protocol.DatasetEpochRFC3339, "2026-01-01T00:00:00") {
		t.Fatalf("DatasetEpoch changed unexpectedly: %q — bump bench_version deliberately if intended",
			protocol.DatasetEpochRFC3339)
	}
}
