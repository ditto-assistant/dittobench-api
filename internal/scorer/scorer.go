// Package scorer turns harness RunResponses into a DittoBench ScoreReport.
// Scoring is fully deterministic (judge-free): a pure function of the dataset
// and the transcript, reproducible by anyone from the public
// dittobench-datagen module.
//
// Tool accuracy per case is deterministic trajectory + argument scoring (see
// trajectory.go): 0.4·name-F1 + 0.4·arg-F1 + 0.2·(order × extra-call
// discipline), and that accuracy is the case score (FinishTool). Cases with no
// expected tool score 1.0 iff the harness called nothing, else 0.0.
// Result-usage cases additionally require the served needle in the answer.
//
// Memory cases are graded by the public per-AnswerKind grader
// (dittobench-datagen/grade).
package scorer

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ditto-assistant/dittobench-datagen/grade"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
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

// ScoreToolCase computes the deterministic tool accuracy of a tool case.
// ToolScore (0..1) is the accuracy; the caller finalizes Score with FinishTool
// or ComposeResultUsage. ok=false means the harness gave no usable response
// (scored as a miss).
func ScoreToolCase(c protocol.ToolCase, resp protocol.RunResponse, ok bool) protocol.CaseScore {
	cs := scoreCase(c, resp, ok)
	cs.Kind = protocol.KindTool
	cs.Score = cs.ToolScore
	return cs
}

// FinishTool finalizes a non-result-usage tool case: the deterministic
// trajectory + argument accuracy IS the score. Judge-free scoring has no
// quality half; response quality under the model lock is a property of the
// locked model, not the harness.
func FinishTool(cs protocol.CaseScore) protocol.CaseScore {
	cs.Score = cs.ToolScore
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

// ComposeResultUsage finishes a result-usage tool case (Phase C). It scores
// deterministically: 0.4·trajectory (did it call the
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

// Canary integrity gate. A per-run canary case asks for a seed-derived nonce
// seeded into the conversation; recalling it proves genuine in-context retrieval
// this run (the value cannot be cached across runs or known to a base model),
// and a bait decoy catches a harness that echoes any nonce-shaped token. The
// gate splits the two failure modes it can see: surfacing the bait (a leak, a
// cross-user boundary breach) stays a hard disqualifier, while an honest recall
// miss that neither leaks nor fabricates takes a bounded penalty. The split
// matters because on a nondeterministic model a single binary canary case is
// near a coin flip run to run; halving the composite on that coin flip injects
// noise, not signal, whereas a leak is a low-noise, deliberate-looking breach
// that a competent honest harness essentially never commits.
const (
	canaryPassThreshold  = 0.5  // canary case Score at/above this counts as passed
	canaryLeakPenalty    = 0.5  // hard multiplier for a canary LEAK (bait echo): an integrity breach
	canaryMissMaxPenalty = 0.15 // bounded drop for an honest canary MISS (matches effMaxPenalty / metamorphicMaxPenalty)
)

// canaryLeakNote is the canonical marker GradeMemory appends when a canary case
// surfaced its bait/forbidden nonce, so CanaryIntegrityFactor can tell a genuine
// leak from an honest recall miss without matching the grader's human-readable
// note wording. Produced and consumed entirely in this package.
const canaryLeakNote = "canary-leak"

// hasNote reports whether a case carries an exact marker note.
func hasNote(cs protocol.CaseScore, marker string) bool {
	for _, n := range cs.Notes {
		if n == marker {
			return true
		}
	}
	return false
}

// isCanaryCase reports whether a CaseScore is the integrity canary (its category
// carries the "canary" question type).
func isCanaryCase(cs protocol.CaseScore) bool {
	return cs.Kind == protocol.KindMemory && strings.Contains(strings.ToLower(cs.Category), "canary")
}

// CanaryIntegrityFactor returns the composite multiplier for canary integrity,
// 1.0 when every canary passed or none ran. A failed canary that leaked its bait
// (canaryLeakNote) applies the hard canaryLeakPenalty; an honest miss applies the
// bounded 1-canaryMissMaxPenalty. Each failed canary compounds, so a harness
// cannot dilute the signal by failing several.
func CanaryIntegrityFactor(perCase []protocol.CaseScore) float64 {
	factor := 1.0
	for _, cs := range perCase {
		if !isCanaryCase(cs) || cs.Score >= canaryPassThreshold {
			continue
		}
		if hasNote(cs, canaryLeakNote) {
			factor *= canaryLeakPenalty // genuine breach: hard disqualifier
		} else {
			factor *= 1.0 - canaryMissMaxPenalty // honest recall miss: bounded penalty
		}
	}
	return round6(factor)
}

// MetamorphicConsistency returns the fraction of invariance twin groups whose
// member cases the harness answered consistently — all correct or all incorrect
// (Ideas #3). A phrasing-brittle harness that gets one twin right and its
// reworded twin wrong scores below 1.0. Returns nil when no twin groups ran
// (nothing to measure). Advisory only — never folded into the composite.
func MetamorphicConsistency(perCase []protocol.CaseScore) *float64 {
	groups := map[string][]bool{}
	for _, cs := range perCase {
		if cs.TwinGroup == "" {
			continue
		}
		groups[cs.TwinGroup] = append(groups[cs.TwinGroup], cs.Correct)
	}
	if len(groups) == 0 {
		return nil
	}
	consistent := 0
	for _, verdicts := range groups {
		all, none := true, true
		for _, v := range verdicts {
			if v {
				none = false
			} else {
				all = false
			}
		}
		if all || none {
			consistent++
		}
	}
	v := round6(float64(consistent) / float64(len(groups)))
	return &v
}

// metamorphicMaxPenalty is the deepest the metamorphic-consistency factor can
// drop the composite (matches effMaxPenalty; accuracy stays dominant).
const metamorphicMaxPenalty = 0.15

// MetamorphicConsistencyFactor is the run's phrasing-robustness multiplier for
// the composite (anti-gaming addendum N2: promote invariance twins from an
// advisory fingerprint to a first-class score). It penalizes ONLY split twin
// groups: a group where the harness got at least one phrasing right and at
// least one reworded phrasing of the SAME fact wrong. That split is the
// template-matcher signature the survey targets (SCORE/PromptEval/CheckList
// INV): a grounded reader answers every sibling alike, a pattern-matcher rides
// one surface and fails the rest.
//
// A uniformly-wrong group is not a brittleness signal (accuracy already
// penalizes it), and a uniformly-correct group is ideal, so both leave the
// factor at 1.0; only the split fraction bites. The reported advisory
// metamorphic_consistency rate is exactly 1 - splitRate, so the applied factor
// is a pure function of that already-published value and stays auditable
// without a new wire field. Returns 1.0 (no effect) when no twin groups ran.
func MetamorphicConsistencyFactor(perCase []protocol.CaseScore) float64 {
	rate := MetamorphicConsistency(perCase)
	if rate == nil {
		return 1.0
	}
	split := 1.0 - *rate // fraction of twin groups the harness answered inconsistently
	return round6(1.0 - metamorphicMaxPenalty*split)
}

// CalibrationBrier returns the mean Brier score over cases whose harness
// reported a confidence (Ideas #6): mean((confidence - outcome)^2) where outcome
// is 1 for a correct case and 0 otherwise. Lower is better; a well-calibrated
// harness minimizes it, and always-claiming-1.0 is punished on its wrong cases.
// Returns (nil, 0) when no case carried a confidence. Advisory only.
func CalibrationBrier(perCase []protocol.CaseScore) (*float64, int) {
	var sum float64
	var n int
	for _, cs := range perCase {
		if cs.Confidence == nil {
			continue
		}
		c := clamp01(*cs.Confidence)
		outcome := 0.0
		if cs.Correct {
			outcome = 1.0
		}
		d := c - outcome
		sum += d * d
		n++
	}
	if n == 0 {
		return nil, 0
	}
	v := round6(sum / float64(n))
	return &v, n
}

// memoryCaseScore assembles a memory CaseScore from its graded score in [0,1].
func memoryCaseScore(mc protocol.MemoryCase, resp protocol.RunResponse, score float64) protocol.CaseScore {
	cs := protocol.CaseScore{
		CaseID:     mc.ID,
		Category:   mc.QuestionType,
		Kind:       protocol.KindMemory,
		Score:      clamp01(round6(score)),
		Correct:    score >= 0.5,
		TwinGroup:  mc.TwinGroup,
		Confidence: resp.Confidence,
		LatencyMs:  resp.LatencyMs,
		Called:     calledNames(resp.ToolCalls),
		Expected:   []string{}, // non-nil so the JSON is [] not null (memory has no expected tools)
	}
	if mc.QuestionType == "" {
		cs.Category = "memory"
	}
	return cs
}

// ScoreMemoryCase builds a memory CaseScore from a binary correctness verdict.
// Retained for legacy yes/no callers; the graded path is GradeMemory.
func ScoreMemoryCase(mc protocol.MemoryCase, resp protocol.RunResponse, correct bool) protocol.CaseScore {
	c := 0.0
	if correct {
		c = 1.0
	}
	return memoryCaseScore(mc, resp, c)
}

// GradeMemory scores one memory case with the public deterministic grader
// (dittobench-datagen/grade): a pure function of (case, response), no LLM.
// Anyone can re-grade a published transcript with the same module and
// reproduce the score byte-for-byte.
func GradeMemory(mc protocol.MemoryCase, resp protocol.RunResponse) protocol.CaseScore {
	v := grade.Memory(mc, resp)
	cs := memoryCaseScore(mc, resp, v.Score)
	cs.Injection = v.Injection
	cs.Notes = append(cs.Notes, v.Notes...)
	// Tag a canary that surfaced its bait/forbidden nonce so CanaryIntegrityFactor
	// treats a genuine leak as a hard disqualifier and spares an honest recall miss.
	// Recomputes the hit with the grader's own exported matcher over the same
	// slot+prose text; a canary is never an injection case, so any forbidden-value
	// hit is unconditionally a leak (no refuse-and-answer exception applies).
	if isCanaryCase(cs) && mc.ForbiddenAnswer != "" &&
		grade.Hit(mc.ForbiddenAnswer, strings.TrimSpace(resp.Answer)+"\n"+resp.FinalText) {
		cs.Notes = append(cs.Notes, canaryLeakNote)
	}
	return cs
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
	var toolSumSq, memSumSq float64
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
			memSumSq += cs.Score * cs.Score
			memN++
		default:
			toolSum += cs.Score
			toolSumSq += cs.Score * cs.Score
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
	// Canary integrity factor: leaking the bait (a cross-user boundary breach)
	// hard-caps the composite and cannot be bought back with easy recall, while an
	// honest recall miss takes a bounded penalty so single-case inference noise on
	// a nondeterministic model does not halve the score. See CanaryIntegrityFactor.
	composite = round6(composite * CanaryIntegrityFactor(perCase))
	// Phrasing-robustness factor (N2): a bounded penalty for answering invariance
	// twins of the same fact inconsistently across surface rewordings. Applied to
	// the composite only, like efficiency and canary — tool_mean, memory_mean, and
	// per_category stay pure accuracy. 1.0 (no effect) when no twin groups ran.
	composite = round6(composite * MetamorphicConsistencyFactor(perCase))

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

	// Composite standard error (v3 #2): combine the tool-half and memory-half SEs.
	// The two halves are independent samples and the composite is 0.5*tool +
	// 0.5*mem, so Var(composite) = 0.25*Var(toolMean) + 0.25*Var(memMean) and
	// SE = 0.5*sqrt(se_tool^2 + se_mem^2). When only one half is present the
	// composite is that half's mean, so its SE is that half's SE.
	seTool := stdErrOfMean(toolSum, toolSumSq, int(toolN))
	seMem := stdErrOfMean(memSum, memSumSq, int(memN))
	var compositeStderr float64
	switch {
	case toolN > 0 && memN > 0:
		compositeStderr = 0.5 * math.Sqrt(seTool*seTool+seMem*seMem)
	case toolN > 0:
		compositeStderr = seTool
	case memN > 0:
		compositeStderr = seMem
	}

	return protocol.ScoreReport{
		RunID:           runID,
		GeneratedAt:     protocol.DatasetEpochRFC3339,
		Composite:       composite,
		CompositeStderr: round6(compositeStderr),
		ToolMean:        toolMean,
		MemoryMean:      memMean,
		MedianMs:        median(latencies),
		N:               len(perCase),
		PerCase:         perCase,
		PerCategory:     perCat,
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
