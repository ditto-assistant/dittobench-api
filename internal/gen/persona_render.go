package gen

import (
	"strconv"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/persona"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// Package note (surface realization, Layer 2):
// the persona plan (internal/persona) is pure ground truth; this file renders
// its session beats into natural user/assistant MemoryPairs from each beat's
// deterministic template surface. Generation is fully non-LLM: the anti-
// memorization surface variation comes from the plan's per-beat template
// variants (pickStr over phrasing sets) and the fresh per-submission seed, not a
// post-hoc paraphrase. The dataset is byte-reproducible from (seed,
// bench_version).

// beatSpacingMinutes is the fixed intra-session spacing between consecutive
// beats' timestamps (deterministic ordering within a session).
const beatSpacingMinutes = 7

// personaTimeSlackDays keeps every rendered timestamp strictly before the pinned
// dataset epoch (the haystack is "the past" as of the epoch).
const personaTimeSlackDays = 7

// RenderHaystack realizes a persona plan into a haystack of MemoryPairs (one per
// beat) and returns the fact→pair evidence map so the question-derivation layer
// can locate — or, for abstention, withhold — a fact's evidence. Every beat is
// rendered from its deterministic template surface (the plan already varied that
// surface via seeded phrasing variants). Timestamps derive from each session's
// seed-set DayOffset anchored backward from protocol.DatasetEpoch — never the
// wall clock.
func RenderHaystack(plan *persona.Plan) ([]protocol.MemoryPair, map[string]string) {
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
			ts := sessionStart.Add(time.Duration(bi*beatSpacingMinutes) * time.Minute)
			pairID := "p-" + strconv.Itoa(s.Index) + "-" + strconv.Itoa(bi)
			pairs = append(pairs, protocol.MemoryPair{
				PairID:    pairID,
				SessionID: sessionID,
				Timestamp: ts.Format(time.RFC3339),
				Prompt:    b.UserText,
				Response:  b.AsstText,
			})
			if b.Kind == persona.BeatFact {
				evidence[b.FactID] = pairID
			}
		}
	}
	return pairs, evidence
}
