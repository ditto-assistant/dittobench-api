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

func scoreCase(c protocol.ToolCase, resp protocol.RunResponse, ok bool) protocol.CaseScore {
	cs := protocol.CaseScore{
		CaseID:    c.ID,
		Category:  c.Category,
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
