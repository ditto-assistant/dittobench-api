package gen

import (
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/persona"
)

func TestGenerateIsolationStructure(t *testing.T) {
	const seed = 778899
	iso := GenerateIsolation(seed, 20, 2, 4)

	if len(iso.Cases) == 0 {
		t.Fatal("expected isolation cases")
	}
	if len(iso.SecondaryWave.Pairs) == 0 {
		t.Fatal("secondary graph should have seeded pairs")
	}
	if iso.SecondaryWave.UserID != SecondaryUser {
		t.Fatalf("secondary graph must be seeded under %q, got %q", SecondaryUser, iso.SecondaryWave.UserID)
	}

	sawA, sawB := false, false
	for _, sc := range iso.Cases {
		if sc.Case.QuestionType != "isolation" {
			t.Fatalf("case %s: want type isolation, got %q", sc.Case.ID, sc.Case.QuestionType)
		}
		if sc.Case.ExpectedAnswer == "" {
			t.Fatalf("case %s: empty expected answer", sc.Case.ID)
		}
		switch sc.UserID {
		case PrimaryUser:
			sawA = true
		case SecondaryUser:
			sawB = true
		default:
			t.Fatalf("case %s: unexpected user_id %q", sc.Case.ID, sc.UserID)
		}
	}
	if !sawA || !sawB {
		t.Fatalf("expected both A-scoped and B-scoped isolation cases (A=%v B=%v)", sawA, sawB)
	}
}

// The crux of isolation: for each case, the OTHER user's graph must hold a
// DIFFERENT value for the queried attribute — otherwise there is no leak to catch.
func TestIsolationCasesHaveConflictingValue(t *testing.T) {
	const seed = 4242
	iso := GenerateIsolation(seed, 20, 2, 4)

	pCur := currentScalars(persona.BuildPlan(seed, personaOptsFor(20)))
	sCur := currentScalars(persona.BuildPlan(seed^isolationSalt, isolationOpts()))

	for _, sc := range iso.Cases {
		attr := attrFromISOID(sc.Case.ID)
		if attr == "" {
			t.Fatalf("could not parse attr from %s", sc.Case.ID)
		}
		pv, sv := pCur[attr], sCur[attr]
		if pv == "" || sv == "" || pv == sv {
			t.Fatalf("case %s attr %q: both graphs must hold a distinct value (A=%q B=%q)", sc.Case.ID, attr, pv, sv)
		}
		// The expected answer is the QUERIED user's value, never the other's.
		want := pv
		if sc.UserID == SecondaryUser {
			want = sv
		}
		if sc.Case.ExpectedAnswer != want {
			t.Fatalf("case %s (user %s): expected %q (its own graph), got %q", sc.Case.ID, sc.UserID, want, sc.Case.ExpectedAnswer)
		}
	}
}

func TestGenerateIsolationDeterministic(t *testing.T) {
	a := GenerateIsolation(31337, 20, 2, 4)
	b := GenerateIsolation(31337, 20, 2, 4)
	if len(a.Cases) != len(b.Cases) || len(a.SecondaryWave.Pairs) != len(b.SecondaryWave.Pairs) {
		t.Fatal("isolation generation not deterministic (counts differ)")
	}
	for i := range a.Cases {
		if a.Cases[i].Case.ID != b.Cases[i].Case.ID ||
			a.Cases[i].Case.ExpectedAnswer != b.Cases[i].Case.ExpectedAnswer ||
			a.Cases[i].UserID != b.Cases[i].UserID {
			t.Fatalf("case %d differs across identical (seed, rng)", i)
		}
	}
}

func TestGenerateIsolationDisabled(t *testing.T) {
	iso := GenerateIsolation(5, 20, 2, 0)
	if len(iso.Cases) != 0 || len(iso.SecondaryWave.Pairs) != 0 {
		t.Fatal("isoCases=0 should produce no isolation content")
	}
}

// attrFromISOID pulls the attribute suffix out of an "iso-a-0001-<attr>" ID.
func attrFromISOID(id string) string {
	parts := strings.SplitN(id, "-", 4)
	if len(parts) < 4 {
		return ""
	}
	return parts[3]
}
