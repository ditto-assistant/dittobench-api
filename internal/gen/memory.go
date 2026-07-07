package gen

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"time"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

const pairParaphraseSystem = "You rewrite ONE turn of a past conversation (a user message and the assistant's reply) using different wording while preserving EVERY fact, name, number, date, and preference exactly. " +
	"Do not add, remove, or change any information — only rephrase. " +
	"Return ONLY valid JSON: {\"prompt\":\"<rewritten user message>\",\"response\":\"<rewritten assistant reply>\"}."

const questionParaphraseSystem = "You rewrite a single benchmark question using different wording while preserving its exact meaning and what it asks for. " +
	"Keep every concrete entity. Do not answer it. " +
	"Return ONLY the rewritten question, no quotes, no preamble."

// timestamp-space knobs (documented in package doc combinatorics).
const (
	baseWindowDays = 3 * 365 // random base date drawn over a ~3-year window
	sessionGapMax  = 14      // up to 14 days between sessions within a case
)

// Abstention knobs. abstentionType must contain the substring the memory judge
// keys on (JudgeMemory / buildLMEJudgeSystem) so the abstention clause fires.
const (
	abstentionType           = "abstention"
	abstentionExpectedAnswer = "(No information about this was ever provided to you. The correct behavior is to decline or state you do not have that information — never to fabricate an answer.)"
	// abstentionDenom sets the abstention share of a run (1/N of the cases). Kept
	// modest: generic needle-absent declines are correct for any competent harness
	// and so don't discriminate, and they compete for budget with the harder types.
	abstentionDenom = 12
)

// abstentionQuota returns the seed-independent number of abstention cases for a
// run of n memory cases (~1/abstentionDenom, at least one once a run is large
// enough to spare it).
func abstentionQuota(n int) int {
	if n < abstentionDenom {
		return 0
	}
	return n / abstentionDenom
}

// GenerateMemory builds a fresh LongMemEval memory benchmark for a submission:
//   - randomly selects n oracle questions (that have usable manifest pairs),
//   - assembles a combined haystack from their pairs,
//   - injects `distractors` random pairs from NON-selected questions,
//   - assigns FRESH timestamps and shuffles the haystack,
//   - LLM-paraphrases a random `frac` of pairs (facts preserved) and the selected
//     questions' text (answer preserved).
//
// It returns the SeedRequest to POST to <harness>/seed and the MemoryCases to
// run. A nil llm (or frac<=0) skips paraphrasing. Errors clearly (no crash) if
// the seed assets are absent.
func GenerateMemory(ctx context.Context, r *rand.Rand, n, distractors int, frac float64, llm LLM, model, seedDir, oraclePath string) (protocol.SeedRequest, []protocol.MemoryCase, protocol.ParaphraseStats, error) {
	var stats protocol.ParaphraseStats
	assets, err := loadSeedAssets(seedDir, oraclePath)
	if err != nil {
		return protocol.SeedRequest{}, nil, stats, fmtErr("memory", err)
	}

	// Candidate questions: have a manifest case with at least one resolvable pair.
	candidates := make([]string, 0, len(assets.oracleOrder))
	for _, qid := range assets.oracleOrder {
		mc, ok := assets.manifest[qid]
		if !ok {
			continue
		}
		if countResolvablePairs(assets, mc) > 0 {
			candidates = append(candidates, qid)
		}
	}
	if len(candidates) == 0 {
		return protocol.SeedRequest{}, nil, stats, fmtErr("memory", fmt.Errorf("no usable oracle questions with manifest pairs"))
	}
	if n > len(candidates) {
		n = len(candidates)
	}

	// Carve out an abstention quota: needle-absent questions whose evidence is
	// deliberately withheld from the haystack (and the distractor pool), so the
	// correct behavior is a grounded decline rather than an answer. This revives
	// v1's dead judge abstention clause. The count is a fixed function of n
	// (seed-independent); the recall suite is stratified over the remainder.
	nAbs := abstentionQuota(n)
	nRecall := n - nAbs

	// Stratify the recall selection by question_type with a fixed, seed-independent
	// per-type quota (like the tool suite's stratifiedCategoryOrder). WHICH
	// questions are drawn within a type varies by seed; HOW MANY of each type
	// does not — removing the multinomial question-type-draw variance that v1's
	// single uniform draw incurred (a variance lever, and it guarantees
	// every type is exercised for the per-type discrimination gate).
	byType := map[string][]string{}
	typeOrder := make([]string, 0)
	for _, qid := range candidates {
		qt := memQuestionType(assets.oracle[qid].QuestionType)
		if _, seen := byType[qt]; !seen {
			typeOrder = append(typeOrder, qt)
		}
		byType[qt] = append(byType[qt], qid)
	}
	slices.Sort(typeOrder)
	avail := make(map[string]int, len(typeOrder))
	for qt, qs := range byType {
		avail[qt] = len(qs)
	}
	quota := stratifiedTypeQuota(typeOrder, avail, nRecall)

	selected := make(map[string]bool, nRecall)
	selectedOrder := make([]string, 0, nRecall)
	for _, qt := range typeOrder {
		pool := byType[qt] // stable order (candidates follow assets.oracleOrder)
		perm := r.Perm(len(pool))
		for i := 0; i < quota[qt] && i < len(pool); i++ {
			qid := pool[perm[i]]
			selected[qid] = true
			selectedOrder = append(selectedOrder, qid)
		}
	}

	// Select abstention questions disjoint from the recall set. Their evidence
	// pairs (absPairs) are withheld from the haystack AND the distractor pool so
	// the needle is genuinely absent.
	absSelected := make(map[string]bool, nAbs)
	absOrder := make([]string, 0, nAbs)
	absPairs := map[string]bool{}
	if nAbs > 0 {
		remaining := make([]string, 0, len(candidates))
		for _, qid := range candidates {
			if !selected[qid] {
				remaining = append(remaining, qid)
			}
		}
		aperm := r.Perm(len(remaining))
		for i := 0; i < nAbs && i < len(remaining); i++ {
			qid := remaining[aperm[i]]
			absSelected[qid] = true
			absOrder = append(absOrder, qid)
			mc := assets.manifest[qid]
			for _, sessIdx := range slices.Sorted(maps.Keys(mc.SessionToPairs)) {
				for _, pid := range mc.SessionToPairs[sessIdx] {
					absPairs[pid] = true
				}
			}
		}
	}

	// Collect the haystack pairs of the selected questions (dedup pair_ids).
	usedPairs := map[string]bool{}
	type pairWithSession struct {
		pairID  string
		session string // synthetic session key (qid#sessionIdx)
	}
	var haystack []pairWithSession
	for _, qid := range selectedOrder {
		mc := assets.manifest[qid]
		// Sort session keys before ranging: Go map iteration order is randomized,
		// which would make the plan layer non-reproducible from the seed.
		for _, sessIdx := range slices.Sorted(maps.Keys(mc.SessionToPairs)) {
			for _, pid := range mc.SessionToPairs[sessIdx] {
				// Never seed a pair that is evidence for an abstention question,
				// even when a recall question also references it — otherwise the
				// needle is present and the abstention case is no longer answer-absent.
				if _, ok := assets.pairsByID[pid]; !ok || usedPairs[pid] || absPairs[pid] {
					continue
				}
				usedPairs[pid] = true
				haystack = append(haystack, pairWithSession{pairID: pid, session: qid + "#" + sessIdx})
			}
		}
	}

	// Inject distractor pairs drawn from NON-selected questions' pairs.
	distractorPool := make([]string, 0)
	for _, qid := range candidates {
		if selected[qid] || absSelected[qid] {
			continue
		}
		mc := assets.manifest[qid]
		for _, sessIdx := range slices.Sorted(maps.Keys(mc.SessionToPairs)) {
			for _, pid := range mc.SessionToPairs[sessIdx] {
				// Never seed a pair that is evidence for an abstention question,
				// even if some other question also references it.
				if _, ok := assets.pairsByID[pid]; ok && !usedPairs[pid] && !absPairs[pid] {
					distractorPool = append(distractorPool, pid)
				}
			}
		}
	}
	dperm := r.Perm(len(distractorPool))
	for i := 0; i < distractors && i < len(distractorPool); i++ {
		pid := distractorPool[dperm[i]]
		if usedPairs[pid] {
			continue
		}
		usedPairs[pid] = true
		haystack = append(haystack, pairWithSession{pairID: pid, session: "distractor#" + pid})
	}

	// Assign fresh timestamps: random base date + per-session day offset + jitter.
	base := randomBaseDate(r)
	sessionBase := map[string]time.Time{}
	for _, h := range haystack {
		if _, ok := sessionBase[h.session]; !ok {
			off := time.Duration(r.Intn(baseWindowDays)) * 24 * time.Hour
			sessionBase[h.session] = base.Add(off)
		}
	}

	// Build protocol pairs with fresh timestamps + optional paraphrase.
	outPairs := make([]protocol.MemoryPair, 0, len(haystack))
	subjectIDs := map[string]bool{}
	var links []protocol.SubjectLink
	for _, h := range haystack {
		sp := assets.pairsByID[h.pairID]
		prompt, response := sp.Prompt, sp.Response
		if llm != nil && frac > 0 && r.Float64() < frac {
			stats.Attempted++
			p, rsp, retried, ok := paraphrasePair(ctx, llm, model, prompt, response)
			if retried {
				stats.Retried++
			}
			if ok {
				prompt, response = p, rsp
				stats.Applied++
			} else {
				stats.Fallback++ // keep the verbatim pair; never a silent skip
			}
		}
		// per-pair timestamp: session base + intra-session jitter (minutes..hours)
		jitter := time.Duration(r.Intn(sessionGapMax*24*60)) * time.Minute
		ts := sessionBase[h.session].Add(jitter)
		outPairs = append(outPairs, protocol.MemoryPair{
			PairID:    h.pairID,
			SessionID: h.session,
			Timestamp: ts.Format(time.RFC3339),
			Prompt:    prompt,
			Response:  response,
		})
		for _, sid := range assets.linksByPair[h.pairID] {
			subjectIDs[sid] = true
			links = append(links, protocol.SubjectLink{SubjectID: sid, PairID: h.pairID})
		}
	}

	// Shuffle haystack ordering so position carries no signal.
	r.Shuffle(len(outPairs), func(i, j int) { outPairs[i], outPairs[j] = outPairs[j], outPairs[i] })

	// Collect the subjects referenced by the haystack.
	outSubjects := make([]protocol.Subject, 0, len(subjectIDs))
	for _, sid := range slices.Sorted(maps.Keys(subjectIDs)) {
		s, ok := assets.subjectsByID[sid]
		if !ok {
			continue
		}
		outSubjects = append(outSubjects, protocol.Subject{
			ID:              s.ID,
			SubjectText:     s.SubjectText,
			DescriptionText: s.DescriptionText,
		})
	}

	// maybeParaphraseQ rewrites a question with probability frac (retry+verify
	// via paraphraseQuestion), accumulating the same fallback telemetry as the
	// pair path. Shared by recall and abstention cases so the r.Float64() draw
	// stays in one place.
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

	// Build the memory cases (paraphrase question text, keep the answer).
	cases := make([]protocol.MemoryCase, 0, len(selectedOrder)+len(absOrder))
	for i, qid := range selectedOrder {
		q := assets.oracle[qid]
		cases = append(cases, protocol.MemoryCase{
			ID:             fmt.Sprintf("mem-%04d-%s", i, qid),
			QuestionID:     qid,
			QuestionType:   memQuestionType(q.QuestionType),
			Question:       maybeParaphraseQ(q.Question),
			ExpectedAnswer: answerText(q.Answer),
		})
	}
	// Abstention cases: the question is asked but its evidence was withheld, so
	// the graded-correct behavior is a decline (judged by the abstention clause).
	for i, qid := range absOrder {
		q := assets.oracle[qid]
		cases = append(cases, protocol.MemoryCase{
			ID:             fmt.Sprintf("abs-%04d-%s", i, qid),
			QuestionID:     qid,
			QuestionType:   abstentionType,
			Question:       maybeParaphraseQ(q.Question),
			ExpectedAnswer: abstentionExpectedAnswer,
		})
	}

	seed := protocol.SeedRequest{
		UserID:   "miner",
		Pairs:    outPairs,
		Subjects: outSubjects,
		Links:    links,
	}
	return seed, cases, stats, nil
}

// memQuestionType normalizes an oracle question_type for bucketing (empty →
// "unknown" so it never collides with a real, scorable type).
func memQuestionType(qt string) string {
	if strings.TrimSpace(qt) == "" {
		return "unknown"
	}
	return qt
}

// stratifiedTypeQuota splits n across the given types with a balanced,
// seed-independent quota (floor/ceil of n/len(types)), capped by each type's
// availability; any shortfall from a scarce type is redistributed round-robin
// over types with spare capacity. Deterministic for a given (types, avail, n)
// regardless of seed — so the per-type case mix is identical across seeds.
func stratifiedTypeQuota(types []string, avail map[string]int, n int) map[string]int {
	quota := make(map[string]int, len(types))
	t := len(types)
	if t == 0 || n <= 0 {
		return quota
	}
	base, rem := n/t, n%t
	for i, qt := range types {
		q := base
		if i < rem {
			q++
		}
		if q > avail[qt] {
			q = avail[qt]
		}
		quota[qt] = q
	}
	assigned := 0
	for _, qt := range types {
		assigned += quota[qt]
	}
	for assigned < n {
		progressed := false
		for _, qt := range types {
			if assigned >= n {
				break
			}
			if quota[qt] < avail[qt] {
				quota[qt]++
				assigned++
				progressed = true
			}
		}
		if !progressed {
			break // no spare capacity anywhere
		}
	}
	return quota
}

func countResolvablePairs(a *seedAssets, mc manifestCase) int {
	count := 0
	for _, pids := range mc.SessionToPairs {
		for _, pid := range pids {
			if _, ok := a.pairsByID[pid]; ok {
				count++
			}
		}
	}
	return count
}

// randomBaseDate draws a base date over a multi-year window ending at the pinned
// dataset epoch. It is deliberately anchored to protocol.DatasetEpoch rather
// than time.Now() so the haystack timestamps — and hence the whole plan-layer
// dataset — are a pure function of the seed (reproducibility contract).
func randomBaseDate(r *rand.Rand) time.Time {
	back := time.Duration(r.Intn(baseWindowDays)) * 24 * time.Hour
	return protocol.DatasetEpoch.Add(-back).Truncate(24 * time.Hour)
}
