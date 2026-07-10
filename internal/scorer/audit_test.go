package scorer

import (
	"context"
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// Residual judge-disagreement measurement: the SCORER_MODEL_B audit slice must
// report whether the two judges agreed, so the run loop can log the live judge
// noise the k=3 median is exposed to (docs/judge-determinism.md).

func TestMemoryAuditAgreement(t *testing.T) {
	mc := protocol.MemoryCase{ID: "m1", QuestionType: "multi-session", ExpectedAnswer: "blue", Question: "q"}
	resp := protocol.RunResponse{FinalText: "no deterministic match here"}

	agree := &modelLLM{byModel: map[string]string{
		"a": `{"correct":"yes","grounded":"yes"}`,
		"b": `{"correct":"yes","grounded":"yes"}`,
	}}
	_, out := GradeMemory(context.Background(), agree, JudgeConfig{Model: "a", ModelB: "b", AuditEvery: 1}, mc, resp)
	if !out.Audited || out.Disagreed {
		t.Fatalf("matching verdicts: want audited, no disagreement; got %+v", out)
	}

	flip := &modelLLM{byModel: map[string]string{
		"a": `{"correct":"yes","grounded":"yes"}`,
		"b": `{"correct":"no","grounded":"yes"}`,
	}}
	_, out = GradeMemory(context.Background(), flip, JudgeConfig{Model: "a", ModelB: "b", AuditEvery: 1}, mc, resp)
	if !out.Audited || !out.Disagreed {
		t.Fatalf("correctness flip: want audited disagreement; got %+v", out)
	}

	// No second judge configured → never audited.
	solo := &modelLLM{def: `{"correct":"yes","grounded":"yes"}`}
	_, out = GradeMemory(context.Background(), solo, JudgeConfig{Model: "a"}, mc, resp)
	if out.Audited {
		t.Fatalf("no ModelB: must not report audited; got %+v", out)
	}
}

func TestMemoryAuditSkipsComparisonOnErroredJudge(t *testing.T) {
	mc := protocol.MemoryCase{ID: "m1", QuestionType: "multi-session", ExpectedAnswer: "blue", Question: "q"}
	resp := protocol.RunResponse{FinalText: "no deterministic match"}
	// Second judge returns garbage (no verdict fields) → Errored; an outage is
	// not a disagreement, so the case must not be counted as audited.
	l := &modelLLM{byModel: map[string]string{
		"a": `{"correct":"yes","grounded":"yes"}`,
		"b": `no json at all`,
	}}
	_, out := GradeMemory(context.Background(), l, JudgeConfig{Model: "a", ModelB: "b", AuditEvery: 1}, mc, resp)
	if out.Audited {
		t.Fatalf("errored second judge must not count as audited: %+v", out)
	}
}

func TestToolAuditAgreement(t *testing.T) {
	// Quality = mean(helpfulness, accuracy)/5. A 4/4 vs 4/4 pair agrees; a
	// 5/5 vs 3/3 pair is a 0.4 gap, well past toolAuditDisagreeDelta.
	agree := &modelLLM{byModel: map[string]string{
		"a": `{"helpfulness":4,"accuracy":4}`,
		"b": `{"helpfulness":4,"accuracy":4}`,
	}}
	_, _, out := GradeToolQuality(context.Background(), agree, JudgeConfig{Model: "a", ModelB: "b", AuditEvery: 1}, "c1", "p", nil, "b", "resp")
	if !out.Audited || out.Disagreed {
		t.Fatalf("matching quality: want audited, no disagreement; got %+v", out)
	}

	gap := &modelLLM{byModel: map[string]string{
		"a": `{"helpfulness":5,"accuracy":5}`,
		"b": `{"helpfulness":3,"accuracy":3}`,
	}}
	_, _, out = GradeToolQuality(context.Background(), gap, JudgeConfig{Model: "a", ModelB: "b", AuditEvery: 1}, "c1", "p", nil, "b", "resp")
	if !out.Audited || !out.Disagreed {
		t.Fatalf("0.4 quality gap: want audited disagreement; got %+v", out)
	}

	// One point on one dimension (0.1 gap) is a rounding wobble, not a
	// disagreement.
	wobble := &modelLLM{byModel: map[string]string{
		"a": `{"helpfulness":4,"accuracy":4}`,
		"b": `{"helpfulness":4,"accuracy":3}`,
	}}
	_, _, out = GradeToolQuality(context.Background(), wobble, JudgeConfig{Model: "a", ModelB: "b", AuditEvery: 1}, "c1", "p", nil, "b", "resp")
	if !out.Audited || out.Disagreed {
		t.Fatalf("0.1 quality gap must not count as disagreement; got %+v", out)
	}
}

// jsonModelLLM is modelLLM plus the CompleteJSON method — the judge must
// prefer JSON-mode completion when the client offers it.
type jsonModelLLM struct {
	modelLLM
	jsonCalls int
}

func (j *jsonModelLLM) CompleteJSON(ctx context.Context, model, system, user string) (string, error) {
	j.jsonCalls++
	return j.modelLLM.Complete(ctx, model, system, user)
}

func TestJudgePrefersJSONMode(t *testing.T) {
	l := &jsonModelLLM{modelLLM: modelLLM{def: `{"correct":"yes","grounded":"yes"}`}}
	v := JudgeMemoryGraded(context.Background(), l, "m", "q", "ans", "resp", "multi-session")
	if !v.Correct {
		t.Fatalf("sanity: verdict should parse, got %+v", v)
	}
	if l.jsonCalls == 0 {
		t.Fatal("judge must route through CompleteJSON when the client supports it")
	}
}
