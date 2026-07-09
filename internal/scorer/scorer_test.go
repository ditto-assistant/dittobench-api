package scorer

import (
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func tc(id, cat string, expected ...string) protocol.ToolCase {
	specs := make([]protocol.ToolSpec, 0, len(expected))
	for _, e := range expected {
		specs = append(specs, protocol.ToolSpec{Name: e})
	}
	return protocol.ToolCase{ID: id, Category: cat, ExpectedTools: specs}
}

func resp(latency int64, tools ...string) protocol.RunResponse {
	calls := make([]protocol.ObservedToolCall, 0, len(tools))
	for _, t := range tools {
		calls = append(calls, protocol.ObservedToolCall{Name: t})
	}
	return protocol.RunResponse{LatencyMs: latency, ToolCalls: calls}
}

func TestPerfectMatch(t *testing.T) {
	cases := []protocol.ToolCase{tc("c1", "web_search", "search_web")}
	resps := map[string]protocol.RunResponse{"c1": resp(100, "search_web")}
	r := Score("run", cases, resps)
	if r.PerCase[0].ToolScore != 1.0 {
		t.Fatalf("expected 1.0, got %v", r.PerCase[0].ToolScore)
	}
	if r.Composite != 1.0 || r.ToolMean != 1.0 {
		t.Fatalf("composite/mean expected 1.0, got %v/%v", r.Composite, r.ToolMean)
	}
}

func TestMissedTool(t *testing.T) {
	cases := []protocol.ToolCase{tc("c1", "web_search", "search_web")}
	resps := map[string]protocol.RunResponse{"c1": resp(100)} // called nothing
	r := Score("run", cases, resps)
	if r.PerCase[0].ToolScore != 0.0 {
		t.Fatalf("expected 0.0 for missed tool, got %v", r.PerCase[0].ToolScore)
	}
}

func TestExtraCallPenalty(t *testing.T) {
	cases := []protocol.ToolCase{tc("c1", "web_search", "search_web")}
	// Correct tool + one extra unexpected call under the trajectory formula:
	//   name-F1 = f1(prec=1/2, rec=1/1) = 0.6667
	//   arg-F1  = 1.0 (no required args)
	//   penalty = extras/expectedTotal = 1/1 = 1 → trajectory term = 0
	//   score   = 0.4*0.6667 + 0.4*1 + 0.2*0 = 0.6667
	// (harsher and call-count-scaling, replacing v1's flat -0.1.)
	resps := map[string]protocol.RunResponse{"c1": resp(100, "search_web", "create_image")}
	r := Score("run", cases, resps)
	if got := r.PerCase[0].ToolScore; got < 0.66 || got > 0.667 {
		t.Fatalf("expected ~0.6667 after one extra call, got %v", got)
	}
}

func TestAllowExtraTools(t *testing.T) {
	c := tc("c1", "web_search", "search_web")
	c.AllowExtraTools = true
	cases := []protocol.ToolCase{c}
	resps := map[string]protocol.RunResponse{"c1": resp(100, "search_web", "create_image")}
	r := Score("run", cases, resps)
	if got := r.PerCase[0].ToolScore; got != 1.0 {
		t.Fatalf("AllowExtraTools should not penalize, got %v", got)
	}
}

func TestNoToolCasePerfect(t *testing.T) {
	cases := []protocol.ToolCase{tc("c1", "no_tool")}
	resps := map[string]protocol.RunResponse{"c1": resp(50)}
	r := Score("run", cases, resps)
	if r.PerCase[0].ToolScore != 1.0 {
		t.Fatalf("no-tool case with no calls should be 1.0, got %v", r.PerCase[0].ToolScore)
	}
}

func TestNoToolCaseViolated(t *testing.T) {
	cases := []protocol.ToolCase{tc("c1", "abstention")}
	resps := map[string]protocol.RunResponse{"c1": resp(50, "search_web")}
	r := Score("run", cases, resps)
	if r.PerCase[0].ToolScore != 0.0 {
		t.Fatalf("no-tool case with a call should be 0.0, got %v", r.PerCase[0].ToolScore)
	}
}

func TestMissingResponseScoresZero(t *testing.T) {
	cases := []protocol.ToolCase{tc("c1", "web_search", "search_web")}
	r := Score("run", cases, map[string]protocol.RunResponse{}) // no response
	if r.PerCase[0].ToolScore != 0.0 {
		t.Fatalf("missing response should score 0.0, got %v", r.PerCase[0].ToolScore)
	}
	if len(r.PerCase[0].Notes) == 0 {
		t.Fatalf("missing response should carry a note")
	}
}

func TestMedianLatency(t *testing.T) {
	cases := []protocol.ToolCase{
		tc("c1", "x", "a"), tc("c2", "x", "a"), tc("c3", "x", "a"),
	}
	resps := map[string]protocol.RunResponse{
		"c1": resp(10, "a"), "c2": resp(30, "a"), "c3": resp(20, "a"),
	}
	r := Score("run", cases, resps)
	if r.MedianMs != 20 {
		t.Fatalf("expected median 20, got %d", r.MedianMs)
	}
}

func TestMedianEvenCount(t *testing.T) {
	if got := median([]int64{10, 20, 30, 40}); got != 25 {
		t.Fatalf("expected 25, got %d", got)
	}
	if got := median(nil); got != 0 {
		t.Fatalf("expected 0 for empty, got %d", got)
	}
}

func TestCanaryIntegrityFactor(t *testing.T) {
	pass := protocol.CaseScore{Kind: protocol.KindMemory, Category: "canary", Score: 0.8}
	fail := protocol.CaseScore{Kind: protocol.KindMemory, Category: "canary", Score: 0.1}
	other := protocol.CaseScore{Kind: protocol.KindMemory, Category: "single-session-recall", Score: 0.1}

	if f := CanaryIntegrityFactor([]protocol.CaseScore{pass, other}); f != 1.0 {
		t.Fatalf("passed canary should not penalize, got %v", f)
	}
	if f := CanaryIntegrityFactor([]protocol.CaseScore{fail, other}); f != canaryFailPenalty {
		t.Fatalf("failed canary should apply %v, got %v", canaryFailPenalty, f)
	}
	if f := CanaryIntegrityFactor([]protocol.CaseScore{fail, fail}); f != canaryFailPenalty*canaryFailPenalty {
		t.Fatalf("two failed canaries should compound, got %v", f)
	}
	if f := CanaryIntegrityFactor([]protocol.CaseScore{other}); f != 1.0 {
		t.Fatalf("no canary should not penalize, got %v", f)
	}
}
