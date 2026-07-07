// Package scorer turns harness RunResponses into a DittoBench ScoreReport.
//
// Tool accuracy per case is deterministic trajectory + argument scoring (A6,
// see trajectory.go): 0.4·name-F1 + 0.4·arg-F1 + 0.2·(order × extra-call
// discipline). Cases with no expected tool score 1.0 iff the harness called
// nothing, else 0.0. The per-case tool composite is 0.5·this + 0.5·quality
// judge (ComposeTool).
//
// Memory credit is graded (A5): 0.7·correctness + 0.3·grounding, with a
// deterministic containment check resolving correctness before the LLM judge.
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

// defaultAuditEvery audits 1-in-N judged cases with the second judge model when
// SCORER_MODEL_B is configured (~20%).
const defaultAuditEvery = 5

// JudgeConfig holds the judge model ids and the audit policy for a run. ModelB
// is optional (""); when set, an audit slice of cases is cross-checked by both
// judges so a judge-specific manipulation is caught by the de-correlated second
// judge (A8, §6.1). Judging is temperature-0 deterministic, so literal k=3
// self-consistency would be a no-op — a second *model* is the meaningful
// de-correlation here.
type JudgeConfig struct {
	Model      string
	ModelB     string
	AuditEvery int // 1-in-N; <=0 → defaultAuditEvery
}

// audits reports whether a case id falls in the second-judge audit slice.
func (jc JudgeConfig) audits(id string) bool {
	if jc.ModelB == "" {
		return false
	}
	n := jc.AuditEvery
	if n <= 0 {
		n = defaultAuditEvery
	}
	return fnv1a(id)%uint32(n) == 0
}

// GradeMemory scores one memory case in [0,1] using deterministic-first grading:
// a normalized containment/value check resolves correctness with NO judge call
// on a hit (the judge-call reduction in A5); on a miss — or for abstention,
// where the "answer" is a decline — the graded judge supplies correctness and
// grounding. A flagged injection scores the case 0. On the audit slice a second
// judge model cross-checks the injection signal. score = 0.7*corr + 0.3*ground.
func GradeMemory(ctx context.Context, judge LLM, cfg JudgeConfig, mc protocol.MemoryCase, resp protocol.RunResponse) protocol.CaseScore {
	if strings.TrimSpace(resp.FinalText) == "" {
		return memoryCaseScore(mc, resp, 0, 0)
	}
	isAbstention := strings.Contains(strings.ToLower(mc.QuestionType), "abstention")
	if !isAbstention && deterministicMemoryHit(mc.ExpectedAnswer, resp.FinalText) {
		cs := memoryCaseScore(mc, resp, 1, 1)
		cs.Notes = append(cs.Notes, "deterministic answer match (no judge call)")
		return cs
	}
	v := JudgeMemoryGraded(ctx, judge, cfg.Model, mc.Question, mc.ExpectedAnswer, resp.FinalText, mc.QuestionType)
	if cfg.audits(mc.ID) {
		vb := JudgeMemoryGraded(ctx, judge, cfg.ModelB, mc.Question, mc.ExpectedAnswer, resp.FinalText, mc.QuestionType)
		v.InjectionAttempt = v.InjectionAttempt || vb.InjectionAttempt // either judge catching it counts
	}
	if v.InjectionAttempt {
		cs := memoryCaseScore(mc, resp, 0, 0)
		cs.Injection = true
		cs.Notes = append(cs.Notes, "judge flagged prompt-injection attempt (case scored 0)")
		return cs
	}
	cs := memoryCaseScore(mc, resp, b2f(v.Correct), b2f(v.Grounded))
	cs.Notes = append(cs.Notes, fmt.Sprintf("judged correct=%t grounded=%t", v.Correct, v.Grounded))
	return cs
}

// GradeToolQuality runs the tool response-quality judge (with the audit-slice
// second judge) and returns the quality plus whether an injection was flagged.
func GradeToolQuality(ctx context.Context, judge LLM, cfg JudgeConfig, caseID, prompt string, toolsCalled []string, expectedBehavior, response string) (float64, bool) {
	v := JudgeToolQualityGraded(ctx, judge, cfg.Model, prompt, toolsCalled, expectedBehavior, response)
	if cfg.audits(caseID) {
		vb := JudgeToolQualityGraded(ctx, judge, cfg.ModelB, prompt, toolsCalled, expectedBehavior, response)
		v.InjectionAttempt = v.InjectionAttempt || vb.InjectionAttempt
	}
	if v.InjectionAttempt {
		return 0, true
	}
	return v.Quality, false
}

// fnv1a is a tiny deterministic string hash for stable audit-slice selection.
func fnv1a(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
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

	// No-expected-tool cases (chit-chat / abstention): perfect only if nothing
	// was called. A single unexpected call zeroes the case.
	if len(c.ExpectedTools) == 0 {
		if len(resp.ToolCalls) == 0 {
			cs.ToolScore = 1.0
		} else {
			cs.ToolScore = 0.0
			cs.Notes = append(cs.Notes, fmt.Sprintf("expected no tools but harness called %d", len(resp.ToolCalls)))
		}
		return cs
	}

	// Deterministic trajectory + argument scoring (A6): name-F1, arg-F1, and a
	// trajectory term (order credit × extra-call discipline).
	score, notes := deterministicToolScore(c, resp.ToolCalls)
	cs.ToolScore = score
	cs.Notes = append(cs.Notes, notes...)
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
	// A single-word answer that is a common English function/modal word (e.g.
	// "no", "may", "will") occurs incidentally in unrelated or declining
	// responses, so don't short-circuit on it — defer to the judge. Distinctive
	// answers ("blue", "tokyo", multi-word phrases) are trusted.
	if !strings.Contains(e, " ") && commonAnswerWords[e] {
		return false
	}
	// Whole-word/phrase match (bounded by non-alphanumerics) so a short answer
	// like "no" does not spuriously match inside "know", nor "Ann" inside
	// "annoyingly" — a raw substring check would credit a wrong answer.
	return containsBoundedPhrase(r, e)
}

// containsBoundedPhrase reports whether phrase appears in text bounded on both
// sides by a non-alphanumeric char (or the string edge). Interior spaces in a
// multi-word phrase are fine; only the outer boundaries are checked.
func containsBoundedPhrase(text, phrase string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], phrase)
		if j < 0 {
			return false
		}
		j += i
		before := j == 0 || !isAlnum(text[j-1])
		after := j+len(phrase) >= len(text) || !isAlnum(text[j+len(phrase)])
		if before && after {
			return true
		}
		i = j + 1
	}
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// commonAnswerWords are single words too generic to trust as a deterministic
// answer match — a containing response is likely incidental (esp. a decline like
// "you may not have that"), so these defer to the LLM judge.
var commonAnswerWords = map[string]bool{
	"no": true, "yes": true, "may": true, "can": true, "will": true,
	"is": true, "are": true, "was": true, "were": true, "be": true,
	"do": true, "did": true, "has": true, "had": true, "not": true,
	"the": true, "and": true, "or": true, "one": true, "two": true,
	"it": true, "to": true, "of": true, "in": true, "on": true, "at": true,
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

// containsNumberToken reports whether num appears in text bounded by non-numeric
// characters — a digit OR a decimal/thousands separator counts as "attached", so
// "5" matches "have 5 cats" but neither "500" nor "3.5".
func containsNumberToken(text, num string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], num)
		if j < 0 {
			return false
		}
		j += i
		before := j == 0 || !numAttached(text[j-1])
		after := j+len(num) >= len(text) || !numAttached(text[j+len(num)])
		if before && after {
			return true
		}
		i = j + 1
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// numAttached reports whether b would make an adjacent digit run part of a larger
// number (another digit, or a decimal/thousands separator).
func numAttached(b byte) bool { return isDigit(b) || b == '.' || b == ',' }

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
