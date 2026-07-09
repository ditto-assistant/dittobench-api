package persona

import (
	"encoding/json"
	"strconv"
	"strings"
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

// TestAskVariantDeterministicAndSeedVarying pins the deterministic
// phrasing-variant contract: a fixed (seed, key) always picks the same variant,
// distinct seeds vary it, and single/empty option sets are handled.
func TestAskVariantDeterministicAndSeedVarying(t *testing.T) {
	opts := []string{"a", "b", "c", "d"}
	if askVariant(7, "rec:city", opts) != askVariant(7, "rec:city", opts) {
		t.Fatal("askVariant not stable for a fixed (seed,key)")
	}
	// Across many seeds the same key should land on more than one variant
	// (surface anti-memorization), not a constant.
	seen := map[string]bool{}
	for s := int64(0); s < 200; s++ {
		seen[askVariant(s, "rec:city", opts)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("askVariant never varied across seeds: %v", seen)
	}
	if askVariant(1, "k", nil) != "" {
		t.Fatal("empty options must yield empty string")
	}
	if askVariant(1, "k", []string{"only"}) != "only" {
		t.Fatal("single option must be returned as-is")
	}
}

// TestQuestionPhrasingVariesAcrossSeeds: two different submission seeds should
// produce at least some differently-worded questions for the same attribute
// (the LLM paraphrase's anti-memorization role is now carried by the templates).
func TestQuestionPhrasingVariesAcrossSeeds(t *testing.T) {
	textByID := func(seed int64) map[string]string {
		m := map[string]string{}
		for _, q := range DeriveQuestions(BuildPlan(seed, DefaultOpts())) {
			m[q.ID] = q.Text
		}
		return m
	}
	a, b := textByID(1), textByID(2)
	diff := 0
	for id, ta := range a {
		if tb, ok := b[id]; ok && ta != tb {
			diff++
		}
	}
	if diff == 0 {
		t.Fatal("no question rephrased between seeds — phrasing variants not seed-varying")
	}
}

// TestQuestionTypeCoverage checks every question type is derivable from a
// default plan — the per-type discrimination gate needs each present.
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
		"finance":  {"risk_tolerance", "brokerage", "holding"},
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

// TestTrajectoryAndMultiHop verifies the N-state sequence questions against the
// plan's ground truth: the ordered-history answer is the chain values in order,
// the previous-value answer is the second-to-last, and a multi-hop state-at-event
// answer is the chain value that held at the event's session — a genuinely PAST
// (superseded) value, not the current one.
func TestTrajectoryAndMultiHop(t *testing.T) {
	sawTraj, sawMH := false, false
	for seed := int64(0); seed < 40; seed++ {
		p := BuildPlan(seed, DefaultOpts())
		qs := DeriveQuestions(p)
		for _, q := range qs {
			switch {
			case len(q.ID) >= 6 && q.ID[:6] == "q-hist":
				sawTraj = true
				ch := scalarChain(p, chainAttrOf(p, q.Evidence))
				want := make([]string, 0, len(ch))
				for _, f := range ch {
					want = append(want, f.Value)
				}
				if q.Answer != joinComma(want) {
					t.Fatalf("seed %d %s: history %q != chain %v", seed, q.ID, q.Answer, want)
				}
			case len(q.ID) >= 6 && q.ID[:6] == "q-prev":
				ch := scalarChain(p, chainAttrOf(p, q.Evidence))
				if len(ch) < 2 || q.Answer != ch[len(ch)-2].Value {
					t.Fatalf("seed %d %s: previous %q != chain[-2]", seed, q.ID, q.Answer)
				}
			case len(q.ID) >= 4 && q.ID[:4] == "q-mh":
				sawMH = true
				// Evidence = [listFact, stateFact]; stateFact must be non-current.
				sf, ok := p.FactByID(q.Evidence[1])
				if !ok || sf.Current {
					t.Fatalf("seed %d %s: multi-hop answer is not a past value", seed, q.ID)
				}
				if q.Answer != sf.Value {
					t.Fatalf("seed %d %s: answer %q != state value %q", seed, q.ID, q.Answer, sf.Value)
				}
				// Recompute the state at the event session independently.
				lf, _ := p.FactByID(q.Evidence[0])
				ch := scalarChain(p, sf.Attribute)
				var recomputed string
				best := -1
				for _, f := range ch {
					if f.Session <= lf.Session && f.Session > best {
						best, recomputed = f.Session, f.Value
					}
				}
				if recomputed != q.Answer {
					t.Fatalf("seed %d %s: recomputed state %q != answer %q", seed, q.ID, recomputed, q.Answer)
				}
			}
		}
	}
	if !sawTraj {
		t.Error("no trajectory (N-state) questions derived across 40 seeds")
	}
	if !sawMH {
		t.Error("no multi-hop questions derived across 40 seeds")
	}
}

// chainAttrOf returns the attribute of the first evidence fact (a chain member).
func chainAttrOf(p *Plan, evidence []string) string {
	if len(evidence) == 0 {
		return ""
	}
	f, _ := p.FactByID(evidence[0])
	return f.Attribute
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

// TestAssistantRecallIsAssistantSide: every assistant-recall question's answer
// lives in the assistant turn of its evidence beat and NOT the user turn — so it
// genuinely tests recalling what the assistant said.
func TestAssistantRecallIsAssistantSide(t *testing.T) {
	p := BuildPlan(7, DefaultOpts())
	beat := map[string]Beat{}
	for _, s := range p.Sessions {
		for _, b := range s.Beats {
			if b.Kind == BeatFact {
				beat[b.FactID] = b
			}
		}
	}
	n := 0
	for _, q := range DeriveQuestions(p) {
		if q.Type != QTAssistantRecall {
			continue
		}
		n++
		b := beat[q.Evidence[0]]
		if !strings.Contains(b.AsstText, q.Answer) {
			t.Fatalf("%s: answer %q not in assistant text %q", q.ID, q.Answer, b.AsstText)
		}
		if strings.Contains(b.UserText, q.Answer) {
			t.Fatalf("%s: answer %q leaked into user text %q", q.ID, q.Answer, b.UserText)
		}
	}
	if n == 0 {
		t.Fatal("expected assistant-recall questions to be derived")
	}
}

// TestAggregationCountMatchesMentions: the aggregation question's answer equals
// the number of recurring-topic mentions, and those mentions span distinct
// sessions (so it is genuinely a cross-session count).
func TestAggregationCountMatchesMentions(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 100} {
		p := BuildPlan(seed, DefaultOpts())
		sessOf := map[string]int{}
		mentions := 0
		for _, f := range p.Facts {
			if f.Kind == KindRecurring {
				mentions++
				sessOf[f.ID] = f.Session
			}
		}
		var q *Question
		qs := DeriveQuestions(p)
		for i := range qs {
			if qs[i].Type == QTAggregation {
				qq := qs[i]
				q = &qq
				break
			}
		}
		if mentions < 2 {
			t.Fatalf("seed %d: expected >=2 recurring mentions, got %d", seed, mentions)
		}
		if q == nil {
			t.Fatalf("seed %d: no aggregation question derived", seed)
		}
		if q.Answer != strconv.Itoa(mentions) || len(q.Evidence) != mentions {
			t.Fatalf("seed %d: answer %q / evidence %d != mentions %d", seed, q.Answer, len(q.Evidence), mentions)
		}
		distinct := map[int]bool{}
		for _, id := range q.Evidence {
			distinct[sessOf[id]] = true
		}
		if len(distinct) < 2 {
			t.Fatalf("seed %d: mentions must span >=2 sessions, got %d", seed, len(distinct))
		}
	}
}
