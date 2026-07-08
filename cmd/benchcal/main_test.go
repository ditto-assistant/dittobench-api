package main

import "testing"

func TestCalibrateNoiseFloorZero(t *testing.T) {
	// A deterministic harness on a fixed seed must have zero repeat-seed spread —
	// this is what proves benchcal measures dataset difficulty, not harness noise.
	rep := calibrate(6, 60, 20)
	if rep.NoiseFloorStddev != 0 {
		t.Fatalf("noise floor stddev must be 0 for a deterministic harness, got %v", rep.NoiseFloorStddev)
	}
}

func TestCalibrateReportShape(t *testing.T) {
	rep := calibrate(8, 90, 30)
	if rep.Runs != 8 || rep.ToolMean.N != 8 {
		t.Fatalf("unexpected run count: %+v", rep.ToolMean)
	}
	if rep.ToolMean.Mean <= 0 || rep.ToolMean.Mean > 1 {
		t.Fatalf("tool_mean out of range: %v", rep.ToolMean.Mean)
	}
	if rep.MemoryMean.N != 8 || rep.Composite.N != 8 {
		t.Fatalf("memory/composite series missing: mem=%d comp=%d", rep.MemoryMean.N, rep.Composite.N)
	}
	if rep.Composite.Mean <= 0 || rep.Composite.Mean > 1 {
		t.Fatalf("composite out of range: %v", rep.Composite.Mean)
	}
	// Structural memory σ must be small — stratification fixes the type mix.
	if rep.MemoryMean.Stddev > 0.02 {
		t.Fatalf("memory structural σ unexpectedly high: %v", rep.MemoryMean.Stddev)
	}
	if len(rep.PerCategory) < 10 {
		t.Fatalf("expected per-category breakdown, got %d categories", len(rep.PerCategory))
	}
	// Discrimination sanity: the keyword router should ace at least one literal
	// category and be defeated by at least one routing trap. Assert on the SET of
	// traps (not one brittle category): overlap-sensitive traps like
	// route_memory_not_web drift with the RNG draw, but the run-vs-read / edit /
	// missing-arg traps structurally defeat a token-overlap router, so the minimum
	// trap mean stays low regardless of the draw.
	traps := []string{
		"route_memory_not_web", "route_web_not_memory", "agent_run_not_read",
		"agent_read_not_run", "image_edit_not_create", "workflow_not_job", "arg_hallucination",
	}
	minTrap := 1.0
	for _, name := range traps {
		if s, ok := rep.PerCategory[name]; ok && s.Mean < minTrap {
			minTrap = s.Mean
		}
	}
	if minTrap > 0.2 {
		t.Fatalf("expected at least one routing/restraint trap to defeat the keyword router, min trap mean = %.3f", minTrap)
	}
	aced := false
	for _, s := range rep.PerCategory {
		if s.Mean >= 0.8 {
			aced = true
			break
		}
	}
	if !aced {
		t.Fatal("expected the keyword router to ace at least one literal category (mean >= 0.8)")
	}
}

func TestCalibrateDeterministic(t *testing.T) {
	a := calibrate(5, 60, 20)
	b := calibrate(5, 60, 20)
	if a.ToolMean != b.ToolMean {
		t.Fatalf("calibration must be reproducible: %+v vs %+v", a.ToolMean, b.ToolMean)
	}
}
