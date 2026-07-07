package persona

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestDeriveQuestionsDeterministic(t *testing.T) {
	p := BuildPlan(42, DefaultOpts())
	a, _ := json.Marshal(DeriveQuestions(p))
	b, _ := json.Marshal(DeriveQuestions(p))
	if string(a) != string(b) {
		t.Fatal("DeriveQuestions is not deterministic for a fixed plan")
	}
}

// TestQuestionTypeCoverage checks every §5.1 question type is derivable from a
// default plan — the per-type discrimination gate (§8 gate 3) needs each present.
func TestQuestionTypeCoverage(t *testing.T) {
	p := BuildPlan(42, DefaultOpts())
	qs := DeriveQuestions(p)
	seen := map[string]int{}
	tiers := map[string]int{}
	for _, q := range qs {
		seen[q.Type]++
		tiers[q.Tier]++
	}
	for _, want := range []string{
		QTSingleSession, QTMultiSession, QTTemporal, QTKnowledgeUpdate,
		QTPreference, QTPreferenceApplication, QTContradiction, QTAbstention,
	} {
		if seen[want] == 0 {
			t.Errorf("no %s questions derived", want)
		}
	}
	for _, tier := range []string{TierEasy, TierMedium, TierHard} {
		if tiers[tier] == 0 {
			t.Errorf("no %s-tier questions derived", tier)
		}
	}
}

// TestAnswersMatchGroundTruth verifies each question's canonical answer is the
// actual plan ground truth: recall answers equal the current fact value; the
// count answer equals the number of list items; knowledge-update answers equal
// the CURRENT (not superseded) value.
func TestAnswersMatchGroundTruth(t *testing.T) {
	p := BuildPlan(7, DefaultOpts())
	qs := DeriveQuestions(p)
	for _, q := range qs {
		switch q.Type {
		case QTSingleSession, QTPreference, QTKnowledgeUpdate:
			f, ok := p.FactByID(q.Evidence[0])
			if !ok {
				t.Fatalf("%s: evidence fact %s missing", q.ID, q.Evidence[0])
			}
			// Evidence[0] is the current fact; its value is the answer.
			if q.Type == QTKnowledgeUpdate && !f.Current {
				t.Fatalf("%s: knowledge-update evidence[0] %s is not the current value", q.ID, f.ID)
			}
			if q.Answer != f.Value {
				t.Fatalf("%s: answer %q != fact value %q", q.ID, q.Answer, f.Value)
			}
		case QTMultiSession:
			if !q.Numeric {
				continue
			}
			if strconv.Itoa(len(q.Evidence)) != q.Answer {
				t.Fatalf("%s: count answer %q != evidence count %d", q.ID, q.Answer, len(q.Evidence))
			}
		case QTAbstention:
			if !q.Abstain {
				t.Fatalf("%s: abstention question not marked Abstain", q.ID)
			}
			if len(q.Evidence) != 0 {
				t.Fatalf("%s: abstention question has evidence (should be needle-absent)", q.ID)
			}
		}
	}
}

// TestDomainCoverage checks that every persona is assigned exactly one
// professional domain, that all three domains are reachable across seeds, and
// that a domain's specialist facts flow through DeriveQuestions as real
// questions with correct ground truth (the data-driven-derivation contract).
func TestDomainCoverage(t *testing.T) {
	domainAttrs := map[string][]string{
		"software": {"primary_language", "code_editor", "service"},
		"medical":  {"diagnosis", "medication", "allergy"},
		"legal":    {"practice_area", "bar_admission", "legal_matter"},
	}
	// attr → its domain, for the exclusivity check.
	attrDomain := map[string]string{}
	for d, attrs := range domainAttrs {
		for _, a := range attrs {
			attrDomain[a] = d
		}
	}

	seenDomain := map[string]bool{}
	for seed := int64(0); seed < 60; seed++ {
		p := BuildPlan(seed, DefaultOpts())

		// exactly one domain present in the plan's facts.
		present := map[string]bool{}
		for _, f := range p.Facts {
			if d, ok := attrDomain[f.Attribute]; ok {
				present[d] = true
			}
		}
		if len(present) != 1 {
			t.Fatalf("seed %d: expected exactly one domain, got %v", seed, present)
		}
		var dom string
		for d := range present {
			dom = d
		}
		seenDomain[dom] = true

		// the domain's scalar attributes must surface as a recall OR
		// knowledge-update question (evidence[0] == the fact) whose answer is the
		// canonical value.
		recallByFact := map[string]Question{}
		for _, q := range DeriveQuestions(p) {
			if (q.Type == QTSingleSession || q.Type == QTKnowledgeUpdate) && len(q.Evidence) > 0 {
				recallByFact[q.Evidence[0]] = q
			}
		}
		for _, f := range p.Facts {
			if attrDomain[f.Attribute] != dom || !f.Current || f.Kind != KindScalar {
				continue
			}
			q, ok := recallByFact[f.ID]
			if !ok {
				t.Fatalf("seed %d: domain scalar %s produced no recall/update question", seed, f.ID)
			}
			if q.Answer != f.Value {
				t.Fatalf("seed %d: %s answer %q != value %q", seed, q.ID, q.Answer, f.Value)
			}
		}
	}
	for d := range domainAttrs {
		if !seenDomain[d] {
			t.Errorf("domain %q never assigned across 60 seeds", d)
		}
	}
}

// TestKnowledgeUpdateAnswerIsLatest guards the latest-value-wins semantics: the
// update answer must be the current value and must differ from the superseded one.
func TestKnowledgeUpdateAnswerIsLatest(t *testing.T) {
	p := BuildPlan(3, DefaultOpts())
	found := false
	for _, q := range DeriveQuestions(p) {
		if q.Type != QTKnowledgeUpdate {
			continue
		}
		found = true
		cur, _ := p.FactByID(q.Evidence[0])
		old, _ := p.FactByID(q.Evidence[1])
		if cur.Value == old.Value {
			t.Fatalf("%s: current and superseded values identical (%q)", q.ID, cur.Value)
		}
		if q.Answer != cur.Value {
			t.Fatalf("%s: answer is not the latest value", q.ID)
		}
	}
	if !found {
		t.Skip("no knowledge-update question in this plan")
	}
}
