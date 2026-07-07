package scorer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// Deterministic tool-trajectory scoring (A6, §5.2). Ported from the backend's
// pkg/dittobench/scorers/toolcall.go (name-F1 / arg-F1 / trajectory), extended
// with explicit order credit for multi-hop expected sequences. The composite
// weights name-F1, arg-F1, and the trajectory term (order × extra-call
// discipline) 0.4 / 0.4 / 0.2.
const (
	wName       = 0.4
	wArg        = 0.4
	wTrajectory = 0.2
)

// deterministicToolScore compares an observed trajectory against a case's
// expected tools, returning a [0,1] score and human-readable notes. It assumes
// the caller has already handled the no-expected-tool and no-response cases.
func deterministicToolScore(c protocol.ToolCase, calls []protocol.ObservedToolCall) (float64, []string) {
	var notes []string

	expectedNames := map[string]int{}
	for _, t := range c.ExpectedTools {
		expectedNames[t.Name]++
	}
	observedNames := map[string]int{}
	for _, o := range calls {
		observedNames[o.Name]++
	}

	truePositive, expectedTotal, observedTotal := 0, 0, 0
	for name, needed := range expectedNames {
		truePositive += min(observedNames[name], needed)
		expectedTotal += needed
	}
	for _, v := range observedNames {
		observedTotal += v
	}

	// Nothing the case asked for was called → no deterministic credit.
	if truePositive == 0 {
		return 0, append(notes, "no expected tool was called")
	}

	prec := ratio(truePositive, observedTotal)
	rec := ratio(truePositive, expectedTotal)
	nameScore := f1(prec, rec)
	// When extra calls are explicitly allowed, judge names on recall only so a
	// permitted extra call does not depress precision.
	if c.AllowExtraTools {
		nameScore = rec
	}

	argF1 := argCorrectness(c.ExpectedTools, calls, &notes)
	order := orderCredit(expectedNames, c.ExpectedTools, calls)

	// Extra-call / over-budget penalty (scales with call count, unlike v1's flat
	// 0.1). Skipped entirely when the case allows extra tools.
	penalty := 0.0
	if !c.AllowExtraTools {
		if c.MaxToolCalls > 0 && observedTotal > c.MaxToolCalls {
			penalty = ratioF(observedTotal-c.MaxToolCalls, c.MaxToolCalls)
		}
		extras := 0
		for name, got := range observedNames {
			if e := got - expectedNames[name]; e > 0 {
				extras += e
			}
		}
		if expectedTotal > 0 && extras > 0 {
			if p := ratioF(extras, expectedTotal); p > penalty {
				penalty = p
			}
			notes = append(notes, fmt.Sprintf("%d extra/unexpected tool call(s)", extras))
		}
	}
	if penalty > 1 {
		penalty = 1
	}

	trajectory := order * (1 - penalty)
	score := wName*nameScore + wArg*argF1 + wTrajectory*trajectory
	return clamp01(round6(score)), notes
}

// argCorrectness scores required/forbidden arguments over the matched expected
// tools (F1 of value-correct required args). Returns 1.0 when no required args
// are specified. Appends notes for forbidden-arg violations.
func argCorrectness(expected []protocol.ToolSpec, calls []protocol.ObservedToolCall, notes *[]string) float64 {
	totalExpected, totalObservedRequired, correct := 0, 0, 0
	used := make([]bool, len(calls))
	for _, et := range expected {
		idx := -1
		for i, o := range calls {
			if !used[i] && o.Name == et.Name {
				idx = i
				break
			}
		}
		if idx == -1 {
			totalExpected += len(et.RequiredArgs)
			continue
		}
		used[idx] = true
		observedArgs := parseArgs(calls[idx].Args)
		for _, key := range sortedArgKeys(et.RequiredArgs) {
			totalExpected++
			got, ok := observedArgs[key]
			if !ok {
				continue
			}
			totalObservedRequired++
			if argValueEqual(got, et.RequiredArgs[key]) {
				correct++
			} else {
				*notes = append(*notes, "wrong value for arg "+key)
			}
		}
		for _, forbidden := range et.ForbiddenArgs {
			if _, ok := observedArgs[forbidden]; ok {
				*notes = append(*notes, "forbidden arg present: "+forbidden)
				totalObservedRequired++ // counts against precision
			}
		}
	}
	if totalExpected == 0 && totalObservedRequired == 0 {
		return 1.0 // no argument expectations → full credit
	}
	return f1(ratio(correct, totalObservedRequired), ratio(correct, totalExpected))
}

// orderCredit rewards a multi-hop trajectory for calling the expected tools in
// the expected relative order: the fraction of consecutive expected pairs
// (e[i], e[i+1]) whose first observed occurrences appear in that order. Single-
// tool expectations have no ordering constraint and score 1.0.
func orderCredit(expectedNames map[string]int, expected []protocol.ToolSpec, calls []protocol.ObservedToolCall) float64 {
	if len(expected) < 2 {
		return 1.0
	}
	firstIdx := map[string]int{}
	for i, o := range calls {
		if _, seen := firstIdx[o.Name]; !seen {
			firstIdx[o.Name] = i
		}
	}
	pairs, satisfied := 0, 0
	for i := 0; i+1 < len(expected); i++ {
		a, b := expected[i].Name, expected[i+1].Name
		pairs++
		ia, oka := firstIdx[a]
		ib, okb := firstIdx[b]
		if oka && okb && ia < ib {
			satisfied++
		}
	}
	if pairs == 0 {
		return 1.0
	}
	return float64(satisfied) / float64(pairs)
}

func parseArgs(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// argValueEqual compares an observed arg value (of any JSON type) to the
// expected string. Strings compare case-insensitively after trimming; other
// types compare via their JSON encoding. Containment is allowed so a query arg
// that embeds the required entity ("news about Tokyo" ⊇ "Tokyo") still credits.
func argValueEqual(got any, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return false // an empty required value is undefined; never auto-credit
	}
	var gotStr string
	switch v := got.(type) {
	case string:
		gotStr = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		gotStr = string(b)
	}
	gotStr = strings.ToLower(strings.TrimSpace(gotStr))
	// Whole-token/phrase containment (not raw substring) so a short enum value
	// like "low" is not satisfied by "yellow"/"below".
	return gotStr == want || containsBoundedPhrase(gotStr, want)
}

func sortedArgKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic order (map iteration is randomized)
	return keys
}

func ratio(num, denom int) float64 {
	if denom <= 0 {
		return 0
	}
	return float64(num) / float64(denom)
}

func ratioF(num, denom int) float64 { return ratio(num, denom) }

func f1(p, r float64) float64 {
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}
