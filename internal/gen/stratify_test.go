package gen

import (
	"fmt"
	"testing"
)

// typeMix counts memory cases by question_type.
func typeMix(t *testing.T, seed int64, n int) map[string]int {
	t.Helper()
	_, cases, _, err := GenerateMemory(NewRNG(seed), n, 40, "", "")
	if err != nil {
		t.Fatalf("GenerateMemory seed %d: %v", seed, err)
	}
	mix := map[string]int{}
	for _, c := range cases {
		mix[c.QuestionType]++
	}
	return mix
}

// TestMemoryTypeMixSeedIndependent checks the property: the per-question-type
// case mix is identical across seeds at a given run size (only WHICH questions
// are drawn varies). This removes the multinomial type-draw variance of v1.
func TestMemoryTypeMixSeedIndependent(t *testing.T) {
	for _, n := range []int{18, 30} {
		ref := typeMix(t, 1, n)
		total := 0
		for _, v := range ref {
			total += v
		}
		if total != n {
			t.Fatalf("n=%d: cases summed to %d, want %d (mix=%v)", n, total, n, ref)
		}
		if len(ref) < 3 {
			t.Fatalf("n=%d: expected several question types, got %v", n, ref)
		}
		for _, seed := range []int64{2, 7, 12345, 999999} {
			got := typeMix(t, seed, n)
			if fmt.Sprint(got) != fmt.Sprint(ref) {
				t.Fatalf("n=%d: type mix differs between seeds:\n seed1=%v\n seed%d=%v", n, ref, seed, got)
			}
		}
	}
}

// TestStratifiedTypeQuota checks the balanced/capped/redistributed allocation.
func TestStratifiedTypeQuota(t *testing.T) {
	types := []string{"a", "b", "c"}

	// Plenty of capacity → balanced floor/ceil, sums to n.
	q := stratifiedTypeQuota(types, map[string]int{"a": 100, "b": 100, "c": 100}, 10)
	if q["a"]+q["b"]+q["c"] != 10 {
		t.Fatalf("balanced quota should sum to 10: %v", q)
	}
	if q["a"] != 4 || q["b"] != 3 || q["c"] != 3 { // rem=1 → first type +1
		t.Fatalf("unexpected balanced split: %v", q)
	}

	// Scarce type → its shortfall redistributes to types with spare capacity.
	q = stratifiedTypeQuota(types, map[string]int{"a": 1, "b": 100, "c": 100}, 12)
	if q["a"] != 1 {
		t.Fatalf("scarce type should be capped at availability: %v", q)
	}
	if q["a"]+q["b"]+q["c"] != 12 {
		t.Fatalf("shortfall should redistribute to sum 12: %v", q)
	}

	// Total availability below n → assign everything available, no more.
	q = stratifiedTypeQuota(types, map[string]int{"a": 2, "b": 2, "c": 2}, 100)
	if q["a"]+q["b"]+q["c"] != 6 {
		t.Fatalf("capped total should be 6: %v", q)
	}
}
