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

// Score builds the aggregate report for a set of cases and their responses.
// Missing responses (harness error / timeout) are scored as zero.
func Score(runID string, cases []protocol.ToolCase, resps map[string]protocol.RunResponse) protocol.ScoreReport {
	perCase := make([]protocol.CaseScore, 0, len(cases))
	var toolSum float64
	latencies := make([]int64, 0, len(cases))

	for _, c := range cases {
		resp, ok := resps[c.ID]
		cs := scoreCase(c, resp, ok)
		cs.Score = cs.ToolScore // direct/legacy mode: composite == tool accuracy
		perCase = append(perCase, cs)
		toolSum += cs.ToolScore
		latencies = append(latencies, cs.LatencyMs)
	}

	n := len(cases)
	toolMean := 0.0
	if n > 0 {
		toolMean = toolSum / float64(n)
	}

	report := protocol.ScoreReport{
		RunID:       runID,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Composite:   toolMean, // tool-mean is the composite for the practice scope
		ToolMean:    toolMean,
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
	}
	if mc.QuestionType == "" {
		cs.Category = "memory"
	}
	return cs
}

// Aggregate folds per-case scores into a ScoreReport. The composite weights tool
// and memory means by their case counts (the published DittoBench composite is
// the mean over all cases); per-category breakdown and median latency included.
func Aggregate(runID string, perCase []protocol.CaseScore) protocol.ScoreReport {
	var toolSum, toolN, memSum, memN float64
	latencies := make([]int64, 0, len(perCase))
	catSum := map[string]float64{}
	catCount := map[string]int{}
	catOrder := make([]string, 0)

	for _, cs := range perCase {
		latencies = append(latencies, cs.LatencyMs)
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
	composite := 0.0
	if n := toolN + memN; n > 0 {
		composite = (toolSum + memSum) / n
	}

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
