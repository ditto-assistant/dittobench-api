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
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

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
		GeneratedAt: protocol.DatasetEpochRFC3339,
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

// Memory grading weights (A5, §5.1): graded credit = 0.7*correctness +
// 0.3*grounding, replacing v1's binary yes/no (which maximized variance and hid
// partial competence, W8).
const (
	memCorrectnessWeight = 0.7
	memGroundingWeight    = 0.3
)

// memoryCaseScore assembles a graded memory CaseScore from its correctness and
// grounding components (each in [0,1]).
func memoryCaseScore(mc protocol.MemoryCase, resp protocol.RunResponse, correctness, grounding float64) protocol.CaseScore {
	cs := protocol.CaseScore{
		CaseID:    mc.ID,
		Category:  mc.QuestionType,
		Kind:      protocol.KindMemory,
		// Round to 6 dp so the exact endpoints are clean (0.7+0.3 is not exactly
		// 1.0 in float64) without affecting scoring resolution.
		Score:     clamp01(round6(memCorrectnessWeight*correctness + memGroundingWeight*grounding)),
		Correct:   correctness >= 0.5,
		LatencyMs: resp.LatencyMs,
		Called:    calledNames(resp.ToolCalls),
		Expected:  []string{}, // non-nil so the JSON is [] not null (memory has no expected tools)
	}
	if mc.QuestionType == "" {
		cs.Category = "memory"
	}
	return cs
}

// ScoreMemoryCase builds a memory CaseScore from a binary correctness verdict
// (correct ⇒ correctness=grounding=1). Retained for the legacy yes/no callers;
// the graded path is GradeMemory.
func ScoreMemoryCase(mc protocol.MemoryCase, resp protocol.RunResponse, correct bool) protocol.CaseScore {
	c := 0.0
	if correct {
		c = 1.0
	}
	return memoryCaseScore(mc, resp, c, c)
}

// GradeMemory scores one memory case in [0,1] using deterministic-first grading:
// a normalized containment/value check resolves correctness with NO judge call
// on a hit (the judge-call reduction in A5); on a miss — or for abstention,
// where the "answer" is a decline — the graded judge supplies both correctness
// and grounding. score = 0.7*correctness + 0.3*grounding.
func GradeMemory(ctx context.Context, judge LLM, modelID string, mc protocol.MemoryCase, resp protocol.RunResponse) protocol.CaseScore {
	if strings.TrimSpace(resp.FinalText) == "" {
		return memoryCaseScore(mc, resp, 0, 0)
	}
	isAbstention := strings.Contains(strings.ToLower(mc.QuestionType), "abstention")
	if !isAbstention && deterministicMemoryHit(mc.ExpectedAnswer, resp.FinalText) {
		cs := memoryCaseScore(mc, resp, 1, 1)
		cs.Notes = append(cs.Notes, "deterministic answer match (no judge call)")
		return cs
	}
	v := JudgeMemoryGraded(ctx, judge, modelID, mc.Question, mc.ExpectedAnswer, resp.FinalText, mc.QuestionType)
	cs := memoryCaseScore(mc, resp, b2f(v.Correct), b2f(v.Grounded))
	cs.Notes = append(cs.Notes, fmt.Sprintf("judged correct=%t grounded=%t", v.Correct, v.Grounded))
	return cs
}

func b2f(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func round6(x float64) float64 { return math.Round(x*1e6) / 1e6 }

// Aggregate folds per-case scores into a ScoreReport. The composite is the
// canonical 0.6*tool_mean + 0.4*memory_mean (see below); per-category breakdown
// and median latency included.
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
	// Canonical DittoBench composite (matches the platform's recorded formula):
	// 0.6*tool_mean + 0.4*memory_mean when both kinds are present; falls back to
	// the single present mean for tool-only or memory-only runs.
	composite := 0.0
	switch {
	case toolN > 0 && memN > 0:
		composite = 0.6*toolMean + 0.4*memMean
	case toolN > 0:
		composite = toolMean
	case memN > 0:
		composite = memMean
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
		GeneratedAt: protocol.DatasetEpochRFC3339,
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

// deterministicMemoryHit reports whether the expected answer is present in the
// response by normalized containment (or, for a purely numeric answer, an exact
// number-token match to avoid "5" matching inside "500"). Conclusive only in the
// POSITIVE direction: a miss defers to the LLM judge, so a false negative is
// harmless but a false positive (crediting a wrong answer) is avoided.
func deterministicMemoryHit(expected, response string) bool {
	e := normalizeAnswer(expected)
	if e == "" {
		return false
	}
	r := normalizeAnswer(response)
	if isPureNumber(e) {
		return containsNumberToken(r, e)
	}
	return strings.Contains(r, e)
}

// normalizeAnswer lowercases, trims surrounding punctuation/quotes, and collapses
// internal whitespace so containment ignores incidental formatting.
func normalizeAnswer(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, `"'.,!?;:`)
	return strings.Join(strings.Fields(s), " ")
}

// isPureNumber reports whether s is only digits with at most one interior
// decimal/thousands separator (e.g. "42", "3.5", "1,000").
func isPureNumber(s string) bool {
	if s == "" {
		return false
	}
	seenSep := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			continue
		}
		if (c == '.' || c == ',') && !seenSep && i > 0 && i < len(s)-1 {
			seenSep = true
			continue
		}
		return false
	}
	return true
}

// containsNumberToken reports whether num appears in text bounded by non-digits
// (so "5" matches "have 5 cats" but not "500").
func containsNumberToken(text, num string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], num)
		if j < 0 {
			return false
		}
		j += i
		before := j == 0 || !isDigit(text[j-1])
		after := j+len(num) >= len(text) || !isDigit(text[j+len(num)])
		if before && after {
			return true
		}
		i = j + 1
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

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
