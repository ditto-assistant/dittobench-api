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
	if r.Composite != 0.5 {
		t.Fatalf("composite expected 0.5, got %v", r.Composite)
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
	// tool_mean=1.0, memory_mean=0.0 → 0.5*1.0 + 0.5*0.0 = 0.5, pinning the v2
	// (bench_version 3) 0.5/0.5 rebalance: memory is the core product value and
	// the raw-pairs tier makes memory_mean the harder axis (BENCHMARK-V2 §5.3).
	r := Aggregate("run", []protocol.CaseScore{
		{CaseID: "t1", Category: "web_search", Kind: protocol.KindTool, Score: 1.0},
		{CaseID: "m1", Category: "multi-session", Kind: protocol.KindMemory, Score: 0.0},
	})
	if r.ToolMean != 1.0 || r.MemoryMean != 0.0 {
		t.Fatalf("means: tool=%v mem=%v", r.ToolMean, r.MemoryMean)
	}
	if r.Composite != 0.5 {
		t.Fatalf("composite expected 0.5 (0.5*1 + 0.5*0), got %v", r.Composite)
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
