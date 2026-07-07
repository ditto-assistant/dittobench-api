// Package scorer turns harness RunResponses into a DittoBench ScoreReport.
//
// Practice scope: TOOL-CALLING correctness + SPEED only. (Memory store /
// embedding recall is evaluated on-chain, not here.)
//
// Tool accuracy per case:
//   - matched = sum over expected tools of min(expected_count, observed_count)
//   - base    = matched / total_expected
//   - penalty = 0.1 per unexpected/extra call (skipped if AllowExtraTools)
//   - score   = clamp(base - penalty, 0, 1)
//   - cases with no expected tool score 1.0 iff the harness called nothing,
//     else 0.0 (a single unexpected call zeroes a no-tool case).
package scorer

import (
	"fmt"
	"sort"
	"time"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// Latency (wall-clock) scoring. A per-case latency is mapped to a 0..1 reward:
// full credit at/below LatencyTargetMs, zero at/above LatencyCeilingMs, linear
// in between. The per-case rewards are averaged into a latency_mean that takes
// LatencyWeight of the final composite; correctness keeps the remaining
// (1-LatencyWeight). Correctness therefore stays primary — speed can lift a
// correct-but-slow harness but can never rescue a wrong one (a zero-accuracy
// case still contributes ~0 to the correctness term).
//
// These are the sole policy knobs for latency scoring; tune them here.
const (
	LatencyTargetMs  int64   = 1000  // at/below this: full latency credit
	LatencyCeilingMs int64   = 10000 // at/above this: zero latency credit
	LatencyWeight    float64 = 0.10  // latency's share of the composite
)

// latencyScore maps a per-case wall-clock latency (ms) to a 0..1 reward using
// the linear target→ceiling curve documented above.
func latencyScore(ms int64) float64 {
	if ms <= LatencyTargetMs {
		return 1.0
	}
	if ms >= LatencyCeilingMs {
		return 0.0
	}
	return float64(LatencyCeilingMs-ms) / float64(LatencyCeilingMs-LatencyTargetMs)
}

// blendLatency folds a correctness score (0..1) and a latency_mean (0..1) into
// the final composite. With no cases (latN == 0) the correctness score passes
// through unchanged.
func blendLatency(correctness, latencyMean float64, latN int) float64 {
	if latN == 0 {
		return correctness
	}
	return (1-LatencyWeight)*correctness + LatencyWeight*latencyMean
}

// Score builds the aggregate report for a set of cases and their responses.
// Missing responses (harness error / timeout) are scored as zero.
func Score(runID string, cases []protocol.ToolCase, resps map[string]protocol.RunResponse) protocol.ScoreReport {
	perCase := make([]protocol.CaseScore, 0, len(cases))
	var toolSum, latSum float64
	latencies := make([]int64, 0, len(cases))

	for _, c := range cases {
		resp, ok := resps[c.ID]
		cs := scoreCase(c, resp, ok)
		cs.Score = cs.ToolScore // direct/legacy mode: correctness == tool accuracy
		cs.LatencyScore = latencyScore(cs.LatencyMs)
		perCase = append(perCase, cs)
		toolSum += cs.ToolScore
		latSum += cs.LatencyScore
		latencies = append(latencies, cs.LatencyMs)
	}

	n := len(cases)
	toolMean := 0.0
	latencyMean := 0.0
	if n > 0 {
		toolMean = toolSum / float64(n)
		latencyMean = latSum / float64(n)
	}

	report := protocol.ScoreReport{
		RunID:       runID,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Composite:   blendLatency(toolMean, latencyMean, n), // tool accuracy + latency
		ToolMean:    toolMean,
		LatencyMean: latencyMean,
		MedianMs:    median(latencies),
		N:           n,
		PerCase:     perCase,
	}
	return report
}

// ScoreToolCase computes the DETERMINISTIC tool-accuracy half of a tool case.
// ToolScore (0..1) is the accuracy; Score is left for the caller to compose with
// the LLM quality judge (tool case = 0.5*tool_accuracy + 0.5*quality). ok=false
// means the harness gave no usable response (scored as a miss).
func ScoreToolCase(c protocol.ToolCase, resp protocol.RunResponse, ok bool) protocol.CaseScore {
	cs := scoreCase(c, resp, ok)
	cs.Kind = protocol.KindTool
	cs.Score = cs.ToolScore // overwritten once quality is judged
	return cs
}

// ComposeTool finishes a tool CaseScore with the LLM quality judge result.
func ComposeTool(cs protocol.CaseScore, quality float64) protocol.CaseScore {
	cs.Quality = quality
	cs.Score = 0.5*cs.ToolScore + 0.5*quality
	return cs
}

// ScoreMemoryCase builds a memory CaseScore from the LongMemEval yes/no judge.
func ScoreMemoryCase(mc protocol.MemoryCase, resp protocol.RunResponse, correct bool) protocol.CaseScore {
	score := 0.0
	if correct {
		score = 1.0
	}
	cs := protocol.CaseScore{
		CaseID:    mc.ID,
		Category:  mc.QuestionType,
		Kind:      protocol.KindMemory,
		Score:     score,
		Correct:   correct,
		LatencyMs: resp.LatencyMs,
		Called:    calledNames(resp.ToolCalls),
		Expected:  []string{}, // non-nil so the JSON is [] not null (memory has no expected tools)
	}
	if mc.QuestionType == "" {
		cs.Category = "memory"
	}
	return cs
}

// Aggregate folds per-case scores into a ScoreReport. The composite is the
// canonical 0.6*tool_mean + 0.4*memory_mean (see below); per-category breakdown
// and median latency included.
func Aggregate(runID string, perCase []protocol.CaseScore) protocol.ScoreReport {
	var toolSum, toolN, memSum, memN, latSum float64
	latencies := make([]int64, 0, len(perCase))
	catSum := map[string]float64{}
	catCount := map[string]int{}
	catOrder := make([]string, 0)

	for i := range perCase {
		cs := &perCase[i]
		cs.LatencyScore = latencyScore(cs.LatencyMs)
		latencies = append(latencies, cs.LatencyMs)
		latSum += cs.LatencyScore
		switch cs.Kind {
		case protocol.KindMemory:
			memSum += cs.Score
			memN++
		default:
			toolSum += cs.Score
			toolN++
		}
		if _, seen := catCount[cs.Category]; !seen {
			catOrder = append(catOrder, cs.Category)
		}
		catSum[cs.Category] += cs.Score
		catCount[cs.Category]++
	}

	toolMean := 0.0
	if toolN > 0 {
		toolMean = toolSum / toolN
	}
	memMean := 0.0
	if memN > 0 {
		memMean = memSum / memN
	}
	// Canonical DittoBench correctness score: 0.6*tool_mean + 0.4*memory_mean when
	// both kinds are present; falls back to the single present mean for tool-only
	// or memory-only runs.
	correctness := 0.0
	switch {
	case toolN > 0 && memN > 0:
		correctness = 0.6*toolMean + 0.4*memMean
	case toolN > 0:
		correctness = toolMean
	case memN > 0:
		correctness = memMean
	}
	latencyMean := 0.0
	if len(perCase) > 0 {
		latencyMean = latSum / float64(len(perCase))
	}
	// Blend wall-clock into the composite: correctness keeps (1-LatencyWeight),
	// latency takes LatencyWeight. Correctness stays primary.
	composite := blendLatency(correctness, latencyMean, len(perCase))

	perCat := make([]protocol.CategoryStat, 0, len(catOrder))
	for _, cat := range catOrder {
		perCat = append(perCat, protocol.CategoryStat{
			Category: cat,
			Count:    catCount[cat],
			Mean:     catSum[cat] / float64(catCount[cat]),
		})
	}

	return protocol.ScoreReport{
		RunID:       runID,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Composite:   composite,
		ToolMean:    toolMean,
		MemoryMean:  memMean,
		LatencyMean: latencyMean,
		MedianMs:    median(latencies),
		N:           len(perCase),
		PerCase:     perCase,
		PerCategory: perCat,
	}
}

func scoreCase(c protocol.ToolCase, resp protocol.RunResponse, ok bool) protocol.CaseScore {
	cs := protocol.CaseScore{
		CaseID:    c.ID,
		Category:  c.Category,
		Kind:      protocol.KindTool,
		LatencyMs: resp.LatencyMs,
		Called:    calledNames(resp.ToolCalls),
		Expected:  expectedNames(c.ExpectedTools),
	}

	if !ok {
		cs.ToolScore = 0
		cs.Notes = append(cs.Notes, "no response from harness (error or timeout)")
		return cs
	}

	// Count observed calls by name.
	observed := map[string]int{}
	for _, tc := range resp.ToolCalls {
		observed[tc.Name]++
	}

	// No-expected-tool cases: perfect only if nothing was called.
	if len(c.ExpectedTools) == 0 {
		if len(resp.ToolCalls) == 0 {
			cs.ToolScore = 1.0
		} else {
			cs.ToolScore = 0.0
			cs.Notes = append(cs.Notes, fmt.Sprintf("expected no tools but harness called %d", len(resp.ToolCalls)))
		}
		return cs
	}

	// Count expected calls by name.
	expected := map[string]int{}
	for _, ts := range c.ExpectedTools {
		expected[ts.Name]++
	}

	totalExpected := 0
	matched := 0
	for name, want := range expected {
		totalExpected += want
		got := observed[name]
		if got < want {
			matched += got
		} else {
			matched += want
		}
	}

	base := 0.0
	if totalExpected > 0 {
		base = float64(matched) / float64(totalExpected)
	}

	// Count extra/unexpected calls (anything beyond what's expected).
	extra := 0
	for name, got := range observed {
		want := expected[name]
		if got > want {
			extra += got - want
		}
	}

	score := base
	if extra > 0 && !c.AllowExtraTools {
		score -= 0.1 * float64(extra)
		cs.Notes = append(cs.Notes, fmt.Sprintf("%d extra/unexpected tool call(s) (-%.1f)", extra, 0.1*float64(extra)))
	}

	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	cs.ToolScore = score
	return cs
}

func calledNames(calls []protocol.ObservedToolCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}

func expectedNames(specs []protocol.ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// median returns the median of latency values (0 for empty input).
func median(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	cp := make([]int64, len(vals))
	copy(cp, vals)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}
