package gen

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"slices"
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

	// Uniform random selection without replacement → C(candidates, n).
	perm := r.Perm(len(candidates))
	selected := make(map[string]bool, n)
	selectedOrder := make([]string, 0, n)
	for i := 0; i < n; i++ {
		qid := candidates[perm[i]]
		selected[qid] = true
		selectedOrder = append(selectedOrder, qid)
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
				if _, ok := assets.pairsByID[pid]; !ok || usedPairs[pid] {
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
		if selected[qid] {
			continue
		}
		mc := assets.manifest[qid]
		for _, sessIdx := range slices.Sorted(maps.Keys(mc.SessionToPairs)) {
			for _, pid := range mc.SessionToPairs[sessIdx] {
				if _, ok := assets.pairsByID[pid]; ok && !usedPairs[pid] {
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

	// Build the memory cases (paraphrase question text, keep the answer).
	cases := make([]protocol.MemoryCase, 0, len(selectedOrder))
	for i, qid := range selectedOrder {
		q := assets.oracle[qid]
		question := q.Question
		if llm != nil && frac > 0 && r.Float64() < frac {
			stats.Attempted++
			pq, retried, ok := paraphraseQuestion(ctx, llm, model, question)
			if retried {
				stats.Retried++
			}
			if ok {
				question = pq
				stats.Applied++
			} else {
				stats.Fallback++
			}
		}
		cases = append(cases, protocol.MemoryCase{
			ID:             fmt.Sprintf("mem-%04d-%s", i, qid),
			QuestionID:     qid,
			QuestionType:   q.QuestionType,
			Question:       question,
			ExpectedAnswer: answerText(q.Answer),
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
// dataset — are a pure function of the seed (reproducibility contract; W5).
func randomBaseDate(r *rand.Rand) time.Time {
	back := time.Duration(r.Intn(baseWindowDays)) * 24 * time.Hour
	return protocol.DatasetEpoch.Add(-back).Truncate(24 * time.Hour)
}
