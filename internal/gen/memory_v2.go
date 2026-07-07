package gen

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"github.com/ditto-assistant/dittobench-api/internal/persona"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// StagedCase pairs a memory case with the seeding wave after which it becomes
// answerable (the max wave among its evidence facts). Abstention cases carry
// wave 0 (a grounded decline is correct regardless of what has been seeded).
type StagedCase struct {
	Case         protocol.MemoryCase
	RunAfterWave int
}

// MemorySuite is the full v2 memory dataset: the ordered seeding waves (Tier C),
// the cases with their unlock wave, paraphrase telemetry, and the Tier-B count.
type MemorySuite struct {
	Waves        []protocol.SeedRequest
	Cases        []StagedCase
	Stats        protocol.ParaphraseStats
	TierBCases   int
	SeedingWaves int
}

// GenerateMemorySuite is the DittoBench v2 memory generator (BENCHMARK-V2.md
// §4–5). It builds a procedural persona universe (seed-only plan → LLM-realized
// haystack → questions derived from ground truth), stratify-samples to the case
// quota, and lays the result out across seeding tiers:
//
//   - Tier A (prepared seeding): a synthesized subject per persona attribute
//     links the pairs, so retrieval is tested in isolation.
//   - Tier B (raw-pairs seeding): for a rawPairsFrac slice of questions, the
//     evidence pairs are seeded WITHOUT prepared subjects — the harness must
//     build its own subject index (the memory-construction test, §5.1).
//   - Tier C (staged ingestion): the haystack is split into nWaves waves by
//     attribute cluster; the pipeline seeds a wave, runs the cases it unlocks,
//     then seeds the next — memory built incrementally. nWaves=1 → single seed.
//
// The plan is a pure function of the master `seed`; realization + sampling use
// the shared rng `r`. Reproducible from (seed, bench_version).
func GenerateMemorySuite(ctx context.Context, r *rand.Rand, seed int64, n int, frac float64, nWaves int, rawPairsFrac float64, llm LLM, model string) MemorySuite {
	if nWaves < 1 {
		nWaves = 1
	}
	suite := MemorySuite{SeedingWaves: nWaves}
	if n <= 0 {
		suite.Waves = []protocol.SeedRequest{{UserID: "miner"}}
		return suite
	}

	plan := persona.BuildPlan(seed, personaOptsFor(n))
	pairs, evidence, renderStats := RenderHaystack(ctx, r, plan, frac, llm, model)
	suite.Stats.Add(renderStats)

	questions := persona.DeriveQuestions(plan)

	// Guarantee an abstention share (W6), stratify the remainder by type.
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
	selected := stratifyByType(r, mainPool, n-nAbs)
	if nAbs > 0 {
		perm := r.Perm(len(absPool))
		for i := 0; i < nAbs; i++ {
			selected = append(selected, absPool[perm[i]])
		}
	}

	// Tier-B selection (§5.1): a rawPairsFrac slice of the non-abstention
	// questions is chosen; the evidence of each is seeded WITHOUT a prepared
	// subject, so a subjects-first retriever cannot route to it — the harness
	// must have built its own index. Pass 1 marks every fact claimed by a
	// NON-Tier-B (Tier A) question; pass 2 promotes a rolled-B question to Tier B
	// only if ALL its evidence is free of Tier-A claims, so Tier-A retrieval never
	// loses its scaffolding to a fact shared with a Tier-B question.
	rollB := map[string]bool{}
	tierAFacts := map[string]bool{}
	for _, q := range selected {
		isB := !q.Abstain && rawPairsFrac > 0 && r.Float64() < rawPairsFrac
		rollB[q.ID] = isB
		if !isB {
			for _, fid := range q.Evidence {
				tierAFacts[fid] = true
			}
		}
	}
	rawFacts := map[string]bool{}
	for _, q := range selected {
		if !rollB[q.ID] || len(q.Evidence) == 0 {
			continue
		}
		free := true
		for _, fid := range q.Evidence {
			if tierAFacts[fid] {
				free = false
				break
			}
		}
		if free {
			suite.TierBCases++
			for _, fid := range q.Evidence {
				rawFacts[fid] = true
			}
		}
	}

	// Build cases (paraphrase question text; abstention → decline sentinel).
	maybeParaphraseQ := func(question string) string {
		if llm != nil && frac > 0 && r.Float64() < frac {
			suite.Stats.Attempted++
			pq, retried, ok := paraphraseQuestion(ctx, llm, model, question)
			if retried {
				suite.Stats.Retried++
			}
			if ok {
				suite.Stats.Applied++
				return pq
			}
			suite.Stats.Fallback++
		}
		return question
	}

	fw := factWaves(plan, nWaves)
	staged := make([]StagedCase, 0, len(selected))
	for i, q := range selected {
		expected := q.Answer
		if q.Abstain {
			expected = abstentionExpectedAnswer
		}
		staged = append(staged, StagedCase{
			Case: protocol.MemoryCase{
				ID:             fmt.Sprintf("mem-%04d-%s", i, q.ID),
				QuestionID:     q.ID,
				QuestionType:   q.Type,
				Question:       maybeParaphraseQ(q.Text),
				ExpectedAnswer: expected,
			},
			RunAfterWave: caseUnlockWave(q, fw),
		})
	}
	r.Shuffle(len(staged), func(i, j int) { staged[i], staged[j] = staged[j], staged[i] })
	suite.Cases = staged

	// Subjects: synthesize for every self fact EXCEPT the raw-pairs slice.
	subjects, links := synthesizeSubjects(plan, evidence, rawFacts)
	suite.Waves = partitionWaves(plan, pairs, subjects, links, evidence, fw, nWaves)
	return suite
}

// GenerateMemoryV2 is the single-wave, all-Tier-A view of the suite, retained
// for callers/tests that want a flat SeedRequest + cases (no staging).
func GenerateMemoryV2(ctx context.Context, r *rand.Rand, seed int64, n int, frac float64, llm LLM, model string) (protocol.SeedRequest, []protocol.MemoryCase, protocol.ParaphraseStats, error) {
	suite := GenerateMemorySuite(ctx, r, seed, n, frac, 1, 0, llm, model)
	cases := make([]protocol.MemoryCase, 0, len(suite.Cases))
	for _, sc := range suite.Cases {
		cases = append(cases, sc.Case)
	}
	seedReq := protocol.SeedRequest{UserID: "miner"}
	if len(suite.Waves) > 0 {
		seedReq = suite.Waves[0]
	}
	return seedReq, cases, suite.Stats, nil
}

// caseUnlockWave is the wave after which a case is answerable: the max wave among
// its evidence facts (abstention / evidence-free → 0).
func caseUnlockWave(q persona.Question, fw map[string]int) int {
	w := 0
	for _, fid := range q.Evidence {
		if v, ok := fw[fid]; ok && v > w {
			w = v
		}
	}
	return w
}

// factWaves assigns each fact a staging wave BY ATTRIBUTE CLUSTER, so a subject
// and all its pairs (including an update chain or a reversal, which share the
// attribute) always land in one wave — no dangling cross-wave subject links, and
// ground truth stays exact (a knowledge-update question only runs once both
// values are seeded). Attribute order is the facts' timeline order (deterministic).
func factWaves(plan *persona.Plan, nWaves int) map[string]int {
	var attrOrder []string
	seen := map[string]bool{}
	for _, f := range plan.Facts {
		if !seen[f.Attribute] {
			seen[f.Attribute] = true
			attrOrder = append(attrOrder, f.Attribute)
		}
	}
	attrWave := map[string]int{}
	for i, a := range attrOrder {
		w := 0
		if len(attrOrder) > 0 {
			w = i * nWaves / len(attrOrder)
		}
		if w >= nWaves {
			w = nWaves - 1
		}
		attrWave[a] = w
	}
	fw := map[string]int{}
	for _, f := range plan.Facts {
		fw[f.ID] = attrWave[f.Attribute]
	}
	return fw
}

// partitionWaves splits the haystack into nWaves SeedRequests: each pair lands in
// its fact's wave (noise pairs by their session bucket); each subject + its links
// land in the subject's attribute wave (== all its pairs' wave). Empty leading
// waves are preserved as index placeholders so Wave numbers stay stable.
func partitionWaves(plan *persona.Plan, pairs []protocol.MemoryPair, subjects []protocol.Subject, links []protocol.SubjectLink, evidence map[string]string, fw map[string]int, nWaves int) []protocol.SeedRequest {
	nSessions := len(plan.Sessions)
	pairWave := map[string]int{}
	for _, f := range plan.Facts {
		if pid, ok := evidence[f.ID]; ok {
			pairWave[pid] = fw[f.ID]
		}
	}
	waves := make([]protocol.SeedRequest, nWaves)
	for i := range waves {
		waves[i] = protocol.SeedRequest{UserID: "miner", Wave: i}
	}
	for _, p := range pairs {
		w, ok := pairWave[p.PairID]
		if !ok { // noise pair: bucket by its session
			w = sessionBucket(sessionOf(p.SessionID), nSessions, nWaves)
		}
		waves[w].Pairs = append(waves[w].Pairs, p)
	}
	// Subjects: subj-<attr> → attribute wave (via any linked pair's wave).
	subjWave := map[string]int{}
	for _, l := range links {
		if w, ok := pairWave[l.PairID]; ok {
			subjWave[l.SubjectID] = w
		}
	}
	for _, s := range subjects {
		w := subjWave[s.ID] // defaults to 0 if unlinked (shouldn't happen)
		waves[w].Subjects = append(waves[w].Subjects, s)
	}
	for _, l := range links {
		w := pairWave[l.PairID]
		waves[w].Links = append(waves[w].Links, l)
	}
	return waves
}

// sessionOf parses the integer index out of a "sess-<i>" SessionID (−1 on
// malformed input, which buckets to wave 0).
func sessionOf(sessionID string) int {
	s := strings.TrimPrefix(sessionID, "sess-")
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

func sessionBucket(session, nSessions, nWaves int) int {
	if session < 0 || nSessions <= 0 || nWaves <= 1 {
		return 0
	}
	w := session * nWaves / nSessions
	if w >= nWaves {
		w = nWaves - 1
	}
	return w
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
// per-type quota (like the tool suite's category stratification): WHICH
// questions within a type varies by seed; HOW MANY of each type does not.
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
		qs := byType[t]
		perm := r.Perm(len(qs))
		for i := 0; i < quota[t] && i < len(qs); i++ {
			out = append(out, qs[perm[i]])
		}
	}
	return out
}

// synthesizeSubjects builds Tier-A prepared seeding: one subject per persona
// attribute, linking the evidence pairs of the (non-distractor) self facts of
// that attribute — EXCEPT facts in rawFacts (Tier B), whose pairs are seeded
// without any subject scaffolding. Deterministic (timeline attribute order).
func synthesizeSubjects(plan *persona.Plan, evidence map[string]string, rawFacts map[string]bool) ([]protocol.Subject, []protocol.SubjectLink) {
	var attrOrder []string
	seenAttr := map[string]bool{}
	linksByAttr := map[string][]protocol.SubjectLink{}

	for _, f := range plan.Facts {
		if f.Entity != "self" || f.Kind == persona.KindDistractor || rawFacts[f.ID] {
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
