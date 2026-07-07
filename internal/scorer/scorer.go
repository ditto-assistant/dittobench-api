// Package scorer turns harness RunResponses into a DittoBench ScoreReport.
//
// Tool accuracy per case is deterministic trajectory + argument scoring (see
// trajectory.go): 0.4·name-F1 + 0.4·arg-F1 + 0.2·(order × extra-call
// discipline). Cases with no expected tool score 1.0 iff the harness called
// nothing, else 0.0. The per-case tool composite is 0.5·this + 0.5·quality
// judge (ComposeTool).
//
// Memory credit is graded: 0.7·correctness + 0.3·grounding, with a
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

// ResultUsageTrajectoryWeight / ResultUsageAnswerWeight split a result-usage tool
// case's score between calling the right tool(s) and actually USING the returned
// content. The answer half dominates: the whole point (capability 13) is
// that the answer incorporates a value obtainable only by executing the tool.
const (
	resultUsageTrajectoryWeight = 0.4
	resultUsageAnswerWeight     = 0.6
)

// ComposeResultUsage finishes a result-usage tool case (Phase C). Instead of the
// LLM quality judge, it scores deterministically: 0.4·trajectory (did it call the
// right tool) + 0.6·(answer carries the served needle value). Because the needle
// is a fabricated per-seed value that exists only in the tool's returned content,
// the answer half is unachievable without actually executing the tool and reading
// the result — self-report and base-model knowledge both score 0 on it.
func ComposeResultUsage(cs protocol.CaseScore, answer, needleValue string) protocol.CaseScore {
	usage := 0.0
	if answerCarriesValue(answer, needleValue) {
		usage = 1.0
	}
	cs.ResultUsage = usage
	cs.Score = round6(resultUsageTrajectoryWeight*cs.ToolScore + resultUsageAnswerWeight*usage)
	if usage == 1.0 {
		cs.Notes = append(cs.Notes, "answer incorporated the served tool result")
	} else {
		cs.Notes = append(cs.Notes, "answer did NOT incorporate the served tool result")
	}
	return cs
}

// answerCarriesValue reports whether a numeric needle value (e.g. "3,418")
// appears in the answer, tolerant of thousands separators — "3418", "3,418", and
// "3 418" all count, but not a coincidental substring inside a longer number.
func answerCarriesValue(answer, needleValue string) bool {
	want := stripSeparators(needleValue)
	if want == "" {
		return false
	}
	got := stripSeparators(normalizeAnswer(answer))
	return containsNumberToken(got, want)
}

// stripSeparators removes thousands separators (commas, spaces) that fall between
// digits so "3,418" / "3 418" / "3418" all normalize to "3418", without merging
// two genuinely separate numbers ("call 5, order 9" stays "call 5, order 9").
func stripSeparators(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c == ',' || c == ' ') && i > 0 && isDigit(s[i-1]) && i+1 < len(s) && isDigit(s[i+1]) {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// UnobservedCeiling caps the composite of an OBSERVABLE tool case (every expected
// tool is validator-served) that the harness did NOT execute through the mock
// tool_endpoint (Phase C). Its self-reported trajectory is untrusted:
// we cannot verify the tools actually ran, so the case is scored selection-only
// and cannot reach full marks. Set at 0.5 so a right-tool self-report still earns
// meaningful-but-capped credit, and a harness is materially rewarded for
// executing through the endpoint (where the full, verified score is reachable).
const UnobservedCeiling = 0.5

// ScoreToolCaseObserved computes the deterministic tool-accuracy half of a case
// under Phase C observed execution. When observed is non-empty (the harness
// routed its calls through the validator's mock endpoint) it REPLACES the
// harness's self-reported tool_calls as the authoritative trajectory — the
// validator grades what it actually saw execute, not what the harness claims.
// When observed is nil it degrades to the pre-Phase-C self-report path
// (ScoreToolCase); the caller applies CapUnobserved for an observable case so an
// unverifiable self-report cannot score full marks.
func ScoreToolCaseObserved(c protocol.ToolCase, resp protocol.RunResponse, ok bool, observed []protocol.ObservedToolCall) protocol.CaseScore {
	if len(observed) == 0 {
		return ScoreToolCase(c, resp, ok)
	}
	auth := resp
	auth.ToolCalls = observed
	cs := ScoreToolCase(c, auth, ok)
	cs.Observed = true
	cs.Notes = append(cs.Notes, "trajectory observed via tool_endpoint (authoritative)")
	return cs
}

// Tool-efficiency term. Among tool cases the validator OBSERVED executing (so the
// call count is authoritative, not self-reported), a harness that reached a
// correct answer within its expected tool budget scores full efficiency; one that
// overshot the budget is penalized on a bounded, dead-zone curve. The factor
// multiplies the composite (never the per-suite means), so it can only separate
// otherwise-comparable harnesses — accuracy stays dominant. Only cases scoring at
// or above effGateScore count, so efficiency differentiates competent responses
// rather than rewarding a lean-but-wrong one.
const (
	effFreeOvershoot = 1    // extra calls beyond expected that incur no penalty
	effFullOvershoot = 5    // overshoot at/above which the penalty saturates
	effMaxPenalty    = 0.15 // deepest the factor can drop (matches the composite floor)
	effGateScore     = 0.5  // only cases scoring >= this contribute
)

// overshootEfficiency maps how far an observed trajectory exceeded the expected
// tool count to a factor in [1-effMaxPenalty, 1].
func overshootEfficiency(expected, actual int) float64 {
	over := actual - expected
	if over <= effFreeOvershoot {
		return 1.0
	}
	frac := float64(over-effFreeOvershoot) / float64(effFullOvershoot-effFreeOvershoot)
	if frac > 1 {
		frac = 1
	}
	return 1 - frac*effMaxPenalty
}

// ToolEfficiencyFactor is the run's observed tool-efficiency multiplier for the
// composite: the mean per-case efficiency over observed, competently-answered
// tool cases, or 1.0 (no effect) when none ran under observed execution — e.g. a
// remote harness that never routed through the mock endpoint, where the call
// count is unverifiable and so is not scored.
func ToolEfficiencyFactor(perCase []protocol.CaseScore) float64 {
	var sum float64
	var n int
	for _, cs := range perCase {
		if cs.Kind != protocol.KindTool || !cs.Observed {
			continue
		}
		expected := len(cs.Expected)
		if expected < 1 || cs.Score < effGateScore {
			continue
		}
		sum += overshootEfficiency(expected, len(cs.Called))
		n++
	}
	if n == 0 {
		return 1.0
	}
	return sum / float64(n)
}

// CapUnobserved applies UnobservedCeiling to a fully-composed tool CaseScore when
// the case was observable but the harness did not execute through the endpoint.
// Call it AFTER ComposeTool (the ceiling is on the final composite, not just the
// deterministic half). No-op when the score is already at or below the ceiling.
func CapUnobserved(cs protocol.CaseScore) protocol.CaseScore {
	if cs.Score > UnobservedCeiling {
		cs.Score = UnobservedCeiling
		cs.Notes = append(cs.Notes, "capped: observable case not executed via tool_endpoint (self-report untrusted)")
	}
	return cs
}

// Memory grading weights: graded credit = 0.7*correctness +
// 0.3*grounding, replacing v1's binary yes/no (which maximized variance and hid
// partial competence).
const (
	memCorrectnessWeight = 0.7
	memGroundingWeight   = 0.3
)

// memoryCaseScore assembles a graded memory CaseScore from its correctness and
// grounding components (each in [0,1]).
func memoryCaseScore(mc protocol.MemoryCase, resp protocol.RunResponse, correctness, grounding float64) protocol.CaseScore {
	cs := protocol.CaseScore{
		CaseID:   mc.ID,
		Category: mc.QuestionType,
		Kind:     protocol.KindMemory,
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
// judge. Judging is temperature-0 deterministic, so literal k=3
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

// JudgeOutcome reports whether a case actually invoked the judge LLM and whether
// that call failed — so the run loop can distinguish a persistent judge outage
// (fail the run) from legitimate low scores.
type JudgeOutcome struct {
	Attempted bool
	Errored   bool
}

// GradeMemory scores one memory case in [0,1] using deterministic-first grading:
// a normalized containment/value check resolves correctness with NO judge call
// on a hit (the judge-call reduction); on a miss — or for abstention,
// where the "answer" is a decline — the graded judge supplies correctness and
// grounding. A flagged injection scores the case 0. On the audit slice a second
// judge model cross-checks the injection signal. score = 0.7*corr + 0.3*ground.
// The JudgeOutcome reports whether the judge was called and whether it errored.
func GradeMemory(ctx context.Context, judge LLM, cfg JudgeConfig, mc protocol.MemoryCase, resp protocol.RunResponse) (protocol.CaseScore, JudgeOutcome) {
	if strings.TrimSpace(resp.FinalText) == "" {
		return memoryCaseScore(mc, resp, 0, 0), JudgeOutcome{}
	}
	qt := strings.ToLower(mc.QuestionType)
	isAbstention := strings.Contains(qt, "abstention")
	// Isolation cases: don't let a positive containment short-circuit the
	// judge. A cross-graph leak that dumps BOTH users' values can incidentally
	// contain the right token, so correctness+grounding must be judged rather than
	// credited on raw containment. Force the graded judge (like abstention).
	alwaysJudge := isAbstention || strings.Contains(qt, "isolation")
	// Isolation leak (deterministic, authoritative): a response that surfaces the
	// value another user's graph holds for this attribute has crossed graphs —
	// score 0 even if it also contains the queried user's own answer. This catches
	// the "dump both users' values" evasion that a containment-based positive
	// check credits, and it does not depend on the judge noticing the leak.
	if mc.ForbiddenAnswer != "" && deterministicMemoryHit(mc.ForbiddenAnswer, resp.FinalText) {
		cs := memoryCaseScore(mc, resp, 0, 0)
		cs.Notes = append(cs.Notes, "cross-graph leak: response surfaced another user's value (scored 0)")
		return cs, JudgeOutcome{}
	}
	if !alwaysJudge && deterministicMemoryHit(mc.ExpectedAnswer, resp.FinalText) {
		cs := memoryCaseScore(mc, resp, 1, 1)
		cs.Notes = append(cs.Notes, "deterministic answer match (no judge call)")
		return cs, JudgeOutcome{}
	}
	v := JudgeMemoryGraded(ctx, judge, cfg.Model, mc.Question, mc.ExpectedAnswer, resp.FinalText, mc.QuestionType)
	out := JudgeOutcome{Attempted: true, Errored: v.Errored}
	if cfg.audits(mc.ID) {
		vb := JudgeMemoryGraded(ctx, judge, cfg.ModelB, mc.Question, mc.ExpectedAnswer, resp.FinalText, mc.QuestionType)
		v.InjectionAttempt = v.InjectionAttempt || vb.InjectionAttempt // either judge catching it counts
	}
	if v.InjectionAttempt {
		cs := memoryCaseScore(mc, resp, 0, 0)
		cs.Injection = true
		cs.Notes = append(cs.Notes, "judge flagged prompt-injection attempt (case scored 0)")
		return cs, out
	}
	cs := memoryCaseScore(mc, resp, b2f(v.Correct), b2f(v.Grounded))
	cs.Notes = append(cs.Notes, fmt.Sprintf("judged correct=%t grounded=%t", v.Correct, v.Grounded))
	return cs, out
}

// GradeToolQuality runs the tool response-quality judge (with the audit-slice
// second judge) and returns the quality, whether an injection was flagged, and
// the JudgeOutcome (attempted/errored) for the run-level outage gate.
func GradeToolQuality(ctx context.Context, judge LLM, cfg JudgeConfig, caseID, prompt string, toolsCalled []string, expectedBehavior, response string) (float64, bool, JudgeOutcome) {
	if strings.TrimSpace(response) == "" {
		return 0, false, JudgeOutcome{} // no judge call on an empty response
	}
	v := JudgeToolQualityGraded(ctx, judge, cfg.Model, prompt, toolsCalled, expectedBehavior, response)
	out := JudgeOutcome{Attempted: true, Errored: v.Errored}
	if cfg.audits(caseID) {
		vb := JudgeToolQualityGraded(ctx, judge, cfg.ModelB, prompt, toolsCalled, expectedBehavior, response)
		v.InjectionAttempt = v.InjectionAttempt || vb.InjectionAttempt
	}
	if v.InjectionAttempt {
		return 0, true, out
	}
	return v.Quality, false, out
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

// stdErrOfMean returns the standard error of the mean of n per-case scores given
// their sum and sum-of-squares, using the sample variance (Bessel-corrected). It
// is 0 for n<2 (a single case has no spread estimate). Guards tiny negative
// variance from float rounding.
func stdErrOfMean(sum, sumSq float64, n int) float64 {
	if n < 2 {
		return 0
	}
	fn := float64(n)
	variance := (sumSq - sum*sum/fn) / (fn - 1)
	if variance <= 0 {
		return 0
	}
	return math.Sqrt(variance / fn)
}

// Aggregate folds per-case scores into a ScoreReport. The composite is the v2
// 0.5*tool_mean + 0.5*memory_mean (see below); per-category breakdown and
// median latency included.
func Aggregate(runID string, perCase []protocol.CaseScore) protocol.ScoreReport {
	var toolSum, toolN, memSum, memN float64
	latencies := make([]int64, 0, len(perCase))
	catSum := map[string]float64{}
	catSumSq := map[string]float64{}
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
		catSumSq[cs.Category] += cs.Score * cs.Score
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
	// DittoBench v2 composite (bench_version 2): 0.5*tool_mean + 0.5*memory_mean
	// when both kinds are present; falls back to the single present mean for
	// tool-only or memory-only runs. Rebalanced from v1's 0.6/0.4:
	// memory is the core product value, memory-coupled tool cases already push
	// memory competence into tool_mean, and the raw-pairs tier (Tier B) makes
	// memory_mean the harder axis. Latency/cost stay OUT of the composite.
	composite := 0.0
	switch {
	case toolN > 0 && memN > 0:
		composite = 0.5*toolMean + 0.5*memMean
	case toolN > 0:
		composite = toolMean
	case memN > 0:
		composite = memMean
	}
	// Fold in observed tool-efficiency as a bounded multiplier (1.0 when no case
	// ran under observed execution). Applied to the composite only — tool_mean,
	// memory_mean, and per_category stay pure accuracy.
	composite = round6(composite * ToolEfficiencyFactor(perCase))

	perCat := make([]protocol.CategoryStat, 0, len(catOrder))
	for _, cat := range catOrder {
		n := catCount[cat]
		mean := catSum[cat] / float64(n)
		perCat = append(perCat, protocol.CategoryStat{
			Category: cat,
			Count:    n,
			Mean:     round6(mean),
			StdErr:   round6(stdErrOfMean(catSum[cat], catSumSq[cat], n)),
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

	// Memory-tool cases: the expected tool is a memory-retrieval tool the mock
	// endpoint never serves, so the harness answers from its OWN seeded memory —
	// legitimately via internal retrieval rather than a catalog tool call. Score on
	// ROUTING, not the exact call: credit unless the harness misroutes the memory
	// request to a non-memory tool (e.g. search_web). This stops the suite from
	// penalizing a competent memory harness for how it retrieves; retrieval
	// accuracy itself is the memory suite's job.
	if allMemoryTools(c.ExpectedTools) {
		for _, call := range resp.ToolCalls {
			if !memoryTools[call.Name] {
				cs.ToolScore = 0
				cs.Notes = append(cs.Notes, "misrouted a memory request to a non-memory tool: "+call.Name)
				return cs
			}
		}
		// Credit retrieval only with EVIDENCE of it — a memory tool call or a
		// substantive answer. A pure no-op (no call and no answer) did not attempt
		// to retrieve, so a routing trap still catches a harness that just ignores
		// the memory request.
		if len(resp.ToolCalls) > 0 || strings.TrimSpace(resp.FinalText) != "" {
			cs.ToolScore = 1.0
			cs.Notes = append(cs.Notes, "memory request handled via memory retrieval (internal or memory tool)")
		} else {
			cs.ToolScore = 0
			cs.Notes = append(cs.Notes, "no memory retrieval attempted (no memory tool call and no answer)")
		}
		return cs
	}

	// Deterministic trajectory + argument scoring: name-F1, arg-F1, and a
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

// memoryTools are the catalog's memory-retrieval tools. The mock endpoint never
// serves them (that would leak seeded answers), so a harness satisfies a
// memory-tool case by retrieving from its own memory — by whatever mechanism.
var memoryTools = map[string]bool{
	"search_memories":             true,
	"search_subjects":             true,
	"fetch_memories":              true,
	"search_memories_in_subjects": true,
}

// allMemoryTools reports whether every expected tool is a memory-retrieval tool
// (so the case is answered from the harness's own memory, not a served tool).
func allMemoryTools(specs []protocol.ToolSpec) bool {
	if len(specs) == 0 {
		return false
	}
	for _, s := range specs {
		if !memoryTools[s.Name] {
			return false
		}
	}
	return true
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

// numAttached reports whether b would make an adjacent digit run part of a
// DIFFERENT number: another digit, a decimal/thousands separator, or a leading
// sign. Counting '-'/'+' as attached stops the expected "5" from matching the
// negation "-5" (a genuinely wrong answer) and stops a number embedded in a
// hyphenated token ("order-42") from being credited.
func numAttached(b byte) bool {
	return isDigit(b) || b == '.' || b == ',' || b == '-' || b == '+'
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
