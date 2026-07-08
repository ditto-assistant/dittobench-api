package gen

import (
	"context"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/persona"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// Package note (surface realization, Layer 2):
// the persona plan (internal/persona) is pure ground truth; this file renders
// its session beats into natural user/assistant MemoryPairs with the pinned
// generator LLM, VERIFYING that each fact's canonical value survives realization
// before accepting it. On LLM error or failed verification it keeps the beat's
// deterministic template surface — never a silent drop. Because
// the scoring-relevant content (the canonical value) is always present whether
// the LLM rendering or the template is used, the dataset stays reproducible from
// (seed, bench_version) at the plan level even when exact wording varies.

// beatSpacingMinutes is the fixed intra-session spacing between consecutive
// beats' timestamps (deterministic ordering within a session).
const beatSpacingMinutes = 7

// personaTimeSlackDays keeps every rendered timestamp strictly before the pinned
// dataset epoch (the haystack is "the past" as of the epoch).
const personaTimeSlackDays = 7

// RenderHaystack realizes a persona plan into a haystack of MemoryPairs (one per
// beat) and returns the fact→pair evidence map so the question-derivation layer
// can locate — or, for abstention, withhold — a fact's evidence. A fraction
// `frac` of beats are LLM-realized (fact beats verified against their canonical
// value; noise beats have no value to preserve); the rest keep their template
// surface. Timestamps derive from each session's seed-set DayOffset anchored
// backward from protocol.DatasetEpoch — never the wall clock. A nil llm or
// frac<=0 renders every beat from its template (deterministic, used in tests).
func RenderHaystack(ctx context.Context, r *rand.Rand, plan *persona.Plan, frac float64, llm LLM, model string) ([]protocol.MemoryPair, map[string]string, protocol.ParaphraseStats) {
	var stats protocol.ParaphraseStats
	evidence := make(map[string]string)

	maxDay := 0
	for _, s := range plan.Sessions {
		if s.DayOffset > maxDay {
			maxDay = s.DayOffset
		}
	}
	anchor := protocol.DatasetEpoch.Add(-time.Duration(maxDay+personaTimeSlackDays) * 24 * time.Hour)

	pairs := make([]protocol.MemoryPair, 0)
	for _, s := range plan.Sessions {
		sessionID := "sess-" + strconv.Itoa(s.Index)
		sessionStart := anchor.Add(time.Duration(s.DayOffset) * 24 * time.Hour)
		for bi, b := range s.Beats {
			user, asst := b.UserText, b.AsstText
			// The canonical value that must survive realization (empty for noise).
			canonical := ""
			// asstRec keeps the beat's fact value ONLY in the assistant turn; skip LLM
			// realization for it so a rewrite can't relocate the value into the user
			// turn (which would let a harness answer without recalling the assistant).
			asstSide := false
			if b.Kind == persona.BeatFact {
				if f, ok := plan.FactByID(b.FactID); ok {
					canonical = f.Value
					asstSide = f.Kind == persona.KindAsstRec
				}
			}
			if llm != nil && frac > 0 && !asstSide && r.Float64() < frac {
				stats.Attempted++
				u, a, retried, ok := realizeBeat(ctx, llm, model, user, asst, canonical)
				if retried {
					stats.Retried++
				}
				if ok {
					user, asst = u, a
					stats.Applied++
				} else {
					stats.Fallback++ // keep the template surface; never a silent skip
				}
			}
			ts := sessionStart.Add(time.Duration(bi*beatSpacingMinutes) * time.Minute)
			pairID := "p-" + strconv.Itoa(s.Index) + "-" + strconv.Itoa(bi)
			pairs = append(pairs, protocol.MemoryPair{
				PairID:    pairID,
				SessionID: sessionID,
				Timestamp: ts.Format(time.RFC3339),
				Prompt:    user,
				Response:  asst,
			})
			if b.Kind == persona.BeatFact {
				evidence[b.FactID] = pairID
			}
		}
	}
	return pairs, evidence, stats
}

// realizeBeat LLM-rewrites one beat's user/assistant turn, preserving all facts.
// It reuses the pair-paraphrase machinery (retry-once, JSON extraction,
// number/date preservation) and ADDS the persona-plan canonical-value check: the
// fact's exact value token must survive verbatim (case-insensitive) or the
// rewrite is rejected and the template kept. canonicalValue "" (noise beats)
// skips the value check. Returns (user, asst, retried, ok).
func realizeBeat(ctx context.Context, llm LLM, model, user, asst, canonicalValue string) (u, a string, retried, ok bool) {
	p, rsp, retried, ok := paraphrasePair(ctx, llm, model, user, asst)
	if !ok {
		return "", "", retried, false
	}
	if !preservesValue(p+" "+rsp, canonicalValue) {
		return "", "", retried, false
	}
	return p, rsp, retried, true
}

// preservesValue reports whether the canonical value token survives a rewrite
// (case-insensitive containment). An empty value is trivially preserved. This is
// the plan-level fact check that makes the reproducibility contract hold: the
// answer token a memory case is graded on is guaranteed present in the seeded
// pair regardless of surface wording.
func preservesValue(rewritten, canonicalValue string) bool {
	v := strings.TrimSpace(canonicalValue)
	if v == "" {
		return true
	}
	return strings.Contains(strings.ToLower(rewritten), strings.ToLower(v))
}
