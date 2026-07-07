package gen

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/persona"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// echoLLM returns valid paraphrase JSON that echoes the input, so every number
// and canonical value survives — models a well-behaved generator (Applied path).
type echoLLM struct{ calls int }

func (e *echoLLM) Complete(_ context.Context, _ string, _ string, user string) (string, error) {
	e.calls++
	b, _ := json.Marshal(map[string]string{
		"prompt":   "Rewritten — " + user,
		"response": "Understood. " + user,
	})
	return string(b), nil
}

// dropLLM returns generic reworded JSON with NO facts — the canonical value is
// gone, so fact beats must fall back to their template (Fallback path).
type dropLLM struct{ calls int }

func (d *dropLLM) Complete(_ context.Context, _ string, _ string, _ string) (string, error) {
	d.calls++
	return `{"prompt":"We chatted about something.","response":"Sounds good."}`, nil
}

// evidenceCarriesValue is the WP B2 reproducibility invariant (§8 gate 6, plan
// level): for every fact, the pair the evidence map points at contains the
// fact's canonical value verbatim — no matter whether the LLM rendering was
// accepted or the template was used as fallback. If this holds, the answer token
// a memory case is graded on is always seeded.
func evidenceCarriesValue(t *testing.T, plan *persona.Plan, pairs []protocol.MemoryPair, evidence map[string]string) {
	t.Helper()
	byID := map[string]protocol.MemoryPair{}
	for _, p := range pairs {
		byID[p.PairID] = p
	}
	for _, f := range plan.Facts {
		pid, ok := evidence[f.ID]
		if !ok {
			t.Fatalf("fact %s has no evidence pair", f.ID)
		}
		pair, ok := byID[pid]
		if !ok {
			t.Fatalf("evidence pair %s for fact %s missing from haystack", pid, f.ID)
		}
		joined := strings.ToLower(pair.Prompt + " " + pair.Response)
		if !strings.Contains(joined, strings.ToLower(f.Value)) {
			t.Fatalf("fact %s value %q not present in its evidence pair %q", f.ID, f.Value, joined)
		}
	}
}

func TestRenderHaystackTemplateDeterministic(t *testing.T) {
	plan := persona.BuildPlan(42, persona.DefaultOpts())
	a, evA, _ := RenderHaystack(context.Background(), NewRNG(1), plan, 0, nil, "")
	b, evB, _ := RenderHaystack(context.Background(), NewRNG(1), plan, 0, nil, "")
	if len(a) != len(b) {
		t.Fatalf("pair count differs: %d vs %d", len(a), len(b))
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("template-only render is not deterministic")
	}
	if len(evA) != len(evB) {
		t.Fatal("evidence maps differ")
	}
	evidenceCarriesValue(t, plan, a, evA)

	// Timestamps: strictly non-decreasing across the flat pair list and all ≤ the
	// pinned epoch (the haystack is the past).
	var prev time.Time
	for i, p := range a {
		ts, err := time.Parse(time.RFC3339, p.Timestamp)
		if err != nil {
			t.Fatalf("pair %s bad timestamp %q: %v", p.PairID, p.Timestamp, err)
		}
		if ts.After(protocol.DatasetEpoch) {
			t.Fatalf("pair %s timestamp %s after epoch", p.PairID, p.Timestamp)
		}
		if i > 0 && ts.Before(prev) {
			t.Fatalf("pair %s timestamp goes backward", p.PairID)
		}
		prev = ts
	}
}

func TestRenderHaystackLLMApplied(t *testing.T) {
	plan := persona.BuildPlan(7, persona.DefaultOpts())
	llm := &echoLLM{}
	pairs, ev, stats := RenderHaystack(context.Background(), NewRNG(2), plan, 1.0, llm, "gen-model")
	if stats.Attempted == 0 {
		t.Fatal("expected paraphrase attempts at frac=1.0")
	}
	if stats.Applied == 0 {
		t.Fatal("echo LLM preserves facts — expected some Applied")
	}
	if stats.Fallback != 0 {
		t.Fatalf("echo LLM should never fail verification, got %d fallbacks", stats.Fallback)
	}
	// Rewrites were accepted, yet every canonical value still survives.
	evidenceCarriesValue(t, plan, pairs, ev)
	if !strings.Contains(pairs[0].Prompt, "Rewritten") && !strings.Contains(pairs[0].Response, "Understood") {
		t.Fatal("expected LLM-rendered surface, got template")
	}
}

func TestRenderHaystackLLMFallbackPreservesValue(t *testing.T) {
	plan := persona.BuildPlan(99, persona.DefaultOpts())
	llm := &dropLLM{}
	pairs, ev, stats := RenderHaystack(context.Background(), NewRNG(3), plan, 1.0, llm, "gen-model")
	if stats.Fallback == 0 {
		t.Fatal("drop LLM strips fact values — expected fallbacks on fact beats")
	}
	// Despite the LLM dropping facts, every canonical value is preserved via the
	// template fallback — the reproducibility contract survives a hostile LLM.
	evidenceCarriesValue(t, plan, pairs, ev)
}
