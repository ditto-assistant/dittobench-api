package scorer

import (
	"testing"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

func TestComposeTool(t *testing.T) {
	cs := protocol.CaseScore{ToolScore: 1.0, Kind: protocol.KindTool}
	cs = ComposeTool(cs, 0.6)
	// 0.5*1.0 + 0.5*0.6 = 0.8
	if cs.Score != 0.8 {
		t.Fatalf("expected composite 0.8, got %v", cs.Score)
	}
	if cs.Quality != 0.6 {
		t.Fatalf("expected quality 0.6, got %v", cs.Quality)
	}
}

func TestScoreMemoryCase(t *testing.T) {
	mc := protocol.MemoryCase{ID: "m1", QuestionType: "multi-session"}
	correct := ScoreMemoryCase(mc, protocol.RunResponse{LatencyMs: 50}, true)
	if correct.Score != 1.0 || !correct.Correct || correct.Kind != protocol.KindMemory {
		t.Fatalf("correct memory case: %+v", correct)
	}
	wrong := ScoreMemoryCase(mc, protocol.RunResponse{}, false)
	if wrong.Score != 0.0 || wrong.Correct {
		t.Fatalf("wrong memory case: %+v", wrong)
	}
	// empty question type defaults category to "memory"
	def := ScoreMemoryCase(protocol.MemoryCase{ID: "m2"}, protocol.RunResponse{}, false)
	if def.Category != "memory" {
		t.Fatalf("expected default category 'memory', got %q", def.Category)
	}
}

func TestAggregate(t *testing.T) {
	perCase := []protocol.CaseScore{
		{CaseID: "t1", Category: "web_search", Kind: protocol.KindTool, Score: 1.0, LatencyMs: 10},
		{CaseID: "t2", Category: "web_search", Kind: protocol.KindTool, Score: 0.0, LatencyMs: 30},
		{CaseID: "m1", Category: "multi-session", Kind: protocol.KindMemory, Score: 1.0, LatencyMs: 20},
		{CaseID: "m2", Category: "multi-session", Kind: protocol.KindMemory, Score: 0.0, LatencyMs: 40},
	}
	r := Aggregate("run", perCase)

	if r.ToolMean != 0.5 {
		t.Fatalf("tool_mean expected 0.5, got %v", r.ToolMean)
	}
	if r.MemoryMean != 0.5 {
		t.Fatalf("memory_mean expected 0.5, got %v", r.MemoryMean)
	}
	// All four latencies (10..40ms) are well under the 1s target → latency_mean 1.0.
	if r.LatencyMean != 1.0 {
		t.Fatalf("latency_mean expected 1.0, got %v", r.LatencyMean)
	}
	// correctness = 0.6*0.5 + 0.4*0.5 = 0.5; composite = 0.9*0.5 + 0.1*1.0 = 0.55.
	if r.Composite != 0.55 {
		t.Fatalf("composite expected 0.55, got %v", r.Composite)
	}
	if r.N != 4 {
		t.Fatalf("n expected 4, got %d", r.N)
	}
	if r.MedianMs != 25 { // median of 10,30,20,40 = (20+30)/2 = 25
		t.Fatalf("median expected 25, got %d", r.MedianMs)
	}
	if len(r.PerCategory) != 2 {
		t.Fatalf("expected 2 categories, got %d", len(r.PerCategory))
	}
	for _, c := range r.PerCategory {
		if c.Count != 2 || c.Mean != 0.5 {
			t.Fatalf("category %s: count=%d mean=%v", c.Category, c.Count, c.Mean)
		}
	}
}

func TestAggregateCompositeWeighting(t *testing.T) {
	// tool_mean=1.0, memory_mean=0.0 → correctness 0.6*1.0 + 0.4*0.0 = 0.6 (NOT
	// the old equal-per-case 0.5), pinning the canonical 0.6/0.4 weighting. Both
	// cases report 0ms latency → latency_mean 1.0, so the composite adds the 10%
	// latency slice: 0.9*0.6 + 0.1*1.0 = 0.64.
	r := Aggregate("run", []protocol.CaseScore{
		{CaseID: "t1", Category: "web_search", Kind: protocol.KindTool, Score: 1.0},
		{CaseID: "m1", Category: "multi-session", Kind: protocol.KindMemory, Score: 0.0},
	})
	if r.ToolMean != 1.0 || r.MemoryMean != 0.0 {
		t.Fatalf("means: tool=%v mem=%v", r.ToolMean, r.MemoryMean)
	}
	if r.Composite != 0.64 {
		t.Fatalf("composite expected 0.64 (0.9*(0.6*1+0.4*0) + 0.1*1.0), got %v", r.Composite)
	}
}

func TestLatencyScoreCurve(t *testing.T) {
	cases := []struct {
		ms   int64
		want float64
	}{
		{0, 1.0},
		{LatencyTargetMs, 1.0},       // at target: full credit
		{LatencyCeilingMs, 0.0},      // at ceiling: zero credit
		{LatencyCeilingMs + 5000, 0}, // past ceiling: clamped to zero
		{5500, 0.5},                  // midpoint of 1000..10000 → 0.5
	}
	for _, c := range cases {
		if got := latencyScore(c.ms); got != c.want {
			t.Fatalf("latencyScore(%d) = %v, want %v", c.ms, got, c.want)
		}
	}
}

func TestAggregateLatencyLowersComposite(t *testing.T) {
	// A perfectly correct but slow (at/over ceiling) run: correctness 1.0 but
	// latency_mean 0.0 → composite = 0.9*1.0 + 0.1*0.0 = 0.9. Speed cannot be
	// faked, and slowness costs the 10% latency slice.
	r := Aggregate("run", []protocol.CaseScore{
		{Kind: protocol.KindTool, Score: 1.0, Category: "x", LatencyMs: LatencyCeilingMs},
	})
	if r.LatencyMean != 0.0 {
		t.Fatalf("latency_mean expected 0.0, got %v", r.LatencyMean)
	}
	if r.Composite != 0.9 {
		t.Fatalf("composite expected 0.9, got %v", r.Composite)
	}
	if r.PerCase[0].LatencyScore != 0.0 {
		t.Fatalf("per-case latency_score expected 0.0, got %v", r.PerCase[0].LatencyScore)
	}
}

func TestAggregateMemoryOnly(t *testing.T) {
	r := Aggregate("run", []protocol.CaseScore{
		{Kind: protocol.KindMemory, Score: 1.0, Category: "x"},
	})
	if r.ToolMean != 0 || r.MemoryMean != 1.0 || r.Composite != 1.0 {
		t.Fatalf("memory-only: tool=%v mem=%v comp=%v", r.ToolMean, r.MemoryMean, r.Composite)
	}
}

func TestAggregateToolOnly(t *testing.T) {
	r := Aggregate("run", []protocol.CaseScore{
		{Kind: protocol.KindTool, Score: 1.0, Category: "x"},
	})
	if r.MemoryMean != 0 || r.ToolMean != 1.0 || r.Composite != 1.0 {
		t.Fatalf("tool-only: tool=%v mem=%v comp=%v", r.ToolMean, r.MemoryMean, r.Composite)
	}
}
