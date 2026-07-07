package gen

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"github.com/ditto-assistant/dittobench-api/internal/persona"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// GenerateMemoryV2 is the DittoBench v2 memory generator (BENCHMARK-V2.md §5.1).
// It replaces the static LongMemEval fixture (GenerateMemory) with a procedural
// persona universe: build the plan (§4.1 Layer 1, seed-only), realize its beats
// into a fresh haystack (§4.1 Layer 2, LLM + verify), derive the question suite
// from the plan's ground truth (persona.DeriveQuestions), then stratify-sample
// to the run's memory-case quota with a guaranteed abstention share.
//
// The plan is a pure function of `seed` (the run's master seed), independent of
// how many draws the tool suite consumed — so the memory dataset is reproducible
// from (seed, bench_version). The rendering + selection stream is the shared
// rng `r`, kept deterministic. Subjects/links are synthesized from the plan
// (Tier A prepared seeding); WP B4 gates the pairs-only Tier B and staged Tier C.
func GenerateMemoryV2(ctx context.Context, r *rand.Rand, seed int64, n int, frac float64, llm LLM, model string) (protocol.SeedRequest, []protocol.MemoryCase, protocol.ParaphraseStats, error) {
	var stats protocol.ParaphraseStats
	if n <= 0 {
		return protocol.SeedRequest{UserID: "miner"}, nil, stats, nil
	}

	plan := persona.BuildPlan(seed, personaOptsFor(n))
	pairs, evidence, renderStats := RenderHaystack(ctx, r, plan, frac, llm, model)
	stats.Add(renderStats)

	questions := persona.DeriveQuestions(plan)

	// Split abstention out; guarantee it a fixed share of the run (revives the
	// judge's needle-absent clause, W6), stratify the remainder by question type.
	var absPool, mainPool []persona.Question
	for _, q := range questions {
		if q.Abstain {
			absPool = append(absPool, q)
		} else {
			mainPool = append(mainPool, q)
		}
	}
	nAbs := abstentionQuota(n)
	if nAbs > len(absPool) {
		nAbs = len(absPool)
	}
	nMain := n - nAbs

	selected := stratifyByType(r, mainPool, nMain)
	// Draw the abstention questions (order within pool is stable/plan-derived).
	if nAbs > 0 {
		perm := r.Perm(len(absPool))
		for i := 0; i < nAbs; i++ {
			selected = append(selected, absPool[perm[i]])
		}
	}

	maybeParaphraseQ := func(question string) string {
		if llm != nil && frac > 0 && r.Float64() < frac {
			stats.Attempted++
			pq, retried, ok := paraphraseQuestion(ctx, llm, model, question)
			if retried {
				stats.Retried++
			}
			if ok {
				stats.Applied++
				return pq
			}
			stats.Fallback++
		}
		return question
	}

	cases := make([]protocol.MemoryCase, 0, len(selected))
	for i, q := range selected {
		expected := q.Answer
		if q.Abstain {
			expected = abstentionExpectedAnswer
		}
		cases = append(cases, protocol.MemoryCase{
			ID:             fmt.Sprintf("mem-%04d-%s", i, q.ID),
			QuestionID:     q.ID,
			QuestionType:   q.Type,
			Question:       maybeParaphraseQ(q.Text),
			ExpectedAnswer: expected,
		})
	}
	// Order carries no signal; shuffle so same-type cases aren't adjacent.
	r.Shuffle(len(cases), func(i, j int) { cases[i], cases[j] = cases[j], cases[i] })

	subjects, links := synthesizeSubjects(plan, evidence)
	seedReq := protocol.SeedRequest{
		UserID:   "miner",
		Pairs:    pairs,
		Subjects: subjects,
		Links:    links,
	}
	return seedReq, cases, stats, nil
}

// personaOptsFor scales the plan size to the memory-case quota so the question
// pool comfortably exceeds n at every run_size.
func personaOptsFor(n int) persona.Opts {
	switch {
	case n <= 8:
		return persona.Opts{Sessions: 5, Projects: 4, Trips: 3, Pets: 2, UpdateChains: 2, Reversals: 1, DecoyPeople: 4}
	case n <= 25:
		return persona.DefaultOpts()
	default:
		return persona.Opts{Sessions: 9, Projects: 10, Trips: 8, Pets: 5, UpdateChains: 4, Reversals: 3, DecoyPeople: 10}
	}
}

// stratifyByType selects up to n questions with a fixed, seed-independent
// per-type quota (like the tool suite's category stratification and v1's
// stratifiedTypeQuota): WHICH questions within a type varies by seed; HOW MANY
// of each type does not — removing question-type-mix variance (a W1 lever) and
// guaranteeing every type is exercised for the §8 gate-3 discrimination check.
func stratifyByType(r *rand.Rand, pool []persona.Question, n int) []persona.Question {
	if n <= 0 || len(pool) == 0 {
		return nil
	}
	byType := map[string][]persona.Question{}
	var typeOrder []string
	for _, q := range pool {
		if _, seen := byType[q.Type]; !seen {
			typeOrder = append(typeOrder, q.Type)
		}
		byType[q.Type] = append(byType[q.Type], q)
	}
	sort.Strings(typeOrder)
	avail := make(map[string]int, len(typeOrder))
	for t, qs := range byType {
		avail[t] = len(qs)
	}
	quota := stratifiedTypeQuota(typeOrder, avail, n)

	var out []persona.Question
	for _, t := range typeOrder {
		qs := byType[t] // stable (pool follows DeriveQuestions order)
		perm := r.Perm(len(qs))
		for i := 0; i < quota[t] && i < len(qs); i++ {
			out = append(out, qs[perm[i]])
		}
	}
	return out
}

// synthesizeSubjects builds Tier-A prepared seeding: one subject per persona
// attribute, linking the evidence pairs of the (non-distractor) self facts of
// that attribute. Distractor and noise pairs are left unlinked — the harness's
// own indexing must cope with them. Deterministic: attributes appear in the
// facts' timeline order.
func synthesizeSubjects(plan *persona.Plan, evidence map[string]string) ([]protocol.Subject, []protocol.SubjectLink) {
	var attrOrder []string
	seenAttr := map[string]bool{}
	linksByAttr := map[string][]protocol.SubjectLink{}

	for _, f := range plan.Facts {
		if f.Entity != "self" || f.Kind == persona.KindDistractor {
			continue
		}
		pid, ok := evidence[f.ID]
		if !ok {
			continue
		}
		if !seenAttr[f.Attribute] {
			seenAttr[f.Attribute] = true
			attrOrder = append(attrOrder, f.Attribute)
		}
		sid := "subj-" + f.Attribute
		linksByAttr[f.Attribute] = append(linksByAttr[f.Attribute], protocol.SubjectLink{SubjectID: sid, PairID: pid})
	}

	subjects := make([]protocol.Subject, 0, len(attrOrder))
	var links []protocol.SubjectLink
	for _, attr := range attrOrder {
		label := titleAttr(attr)
		subjects = append(subjects, protocol.Subject{
			ID:              "subj-" + attr,
			SubjectText:     label,
			DescriptionText: "The user's recorded facts about their " + strings.ToLower(label) + ".",
		})
		links = append(links, linksByAttr[attr]...)
	}
	return subjects, links
}

// titleAttr renders an attribute key ("favorite_cuisine") as a subject label
// ("Favorite Cuisine").
func titleAttr(attr string) string {
	words := strings.Split(attr, "_")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
