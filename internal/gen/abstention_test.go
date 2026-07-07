package gen

import (
	"context"
	"strings"
	"testing"
)

// TestAbstentionNeedleAbsent checks the core property: abstention cases are
// present at the fixed quota, and NONE of their evidence pairs appear in the
// seeded haystack (nor as distractors) — so the only correct behavior is a
// grounded decline.
func TestAbstentionNeedleAbsent(t *testing.T) {
	assets, err := loadSeedAssets("", "")
	if err != nil {
		t.Fatalf("loadSeedAssets: %v", err)
	}
	const n = 30
	seedReq, cases, _, err := GenerateMemory(context.Background(), NewRNG(42), n, 60, 0, nil, "m", "", "")
	if err != nil {
		t.Fatalf("GenerateMemory: %v", err)
	}
	inHaystack := map[string]bool{}
	for _, p := range seedReq.Pairs {
		inHaystack[p.PairID] = true
	}

	absCount := 0
	for _, c := range cases {
		if c.QuestionType != abstentionType {
			continue
		}
		absCount++
		if !strings.Contains(strings.ToLower(c.ExpectedAnswer), "decline") {
			t.Fatalf("abstention case %s should carry the decline marker, got %q", c.ID, c.ExpectedAnswer)
		}
		mc := assets.manifest[c.QuestionID]
		for _, pids := range mc.SessionToPairs {
			for _, pid := range pids {
				if inHaystack[pid] {
					t.Fatalf("abstention question %s has evidence pair %s seeded — needle not absent", c.QuestionID, pid)
				}
			}
		}
	}
	if want := abstentionQuota(n); absCount != want {
		t.Fatalf("abstention quota: got %d want %d", absCount, want)
	}
	if absCount == 0 {
		t.Fatalf("expected abstention cases at n=%d", n)
	}
}

func TestAbstentionQuota(t *testing.T) {
	// ~1/12 of a run (see abstentionDenom): 0 below the denom, then n/12.
	cases := map[int]int{0: 0, 3: 0, 11: 0, 12: 1, 20: 1, 50: 4}
	for n, want := range cases {
		if got := abstentionQuota(n); got != want {
			t.Errorf("abstentionQuota(%d)=%d want %d", n, got, want)
		}
	}
}
