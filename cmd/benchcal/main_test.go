package main

import "testing"

func TestCalibrateNoiseFloorZero(t *testing.T) {
	// A deterministic harness on a fixed seed must have zero repeat-seed spread —
	// this is what proves benchcal measures dataset difficulty, not harness noise.
	rep := calibrate(6, 60)
	if rep.NoiseFloorStddev != 0 {
		t.Fatalf("noise floor stddev must be 0 for a deterministic harness, got %v", rep.NoiseFloorStddev)
	}
}

func TestCalibrateReportShape(t *testing.T) {
	rep := calibrate(8, 90)
	if rep.Runs != 8 || rep.ToolMean.N != 8 {
		t.Fatalf("unexpected run count: %+v", rep.ToolMean)
	}
	if rep.ToolMean.Mean <= 0 || rep.ToolMean.Mean > 1 {
		t.Fatalf("tool_mean out of range: %v", rep.ToolMean.Mean)
	}
	if len(rep.PerCategory) < 10 {
		t.Fatalf("expected per-category breakdown, got %d categories", len(rep.PerCategory))
	}
	// Discrimination sanity: the refharness should ace at least one literal
	// category and fail at least one routing trap (stddev/means vary by category).
	if s, ok := rep.PerCategory["route_memory_not_web"]; !ok || s.Mean > 0.2 {
		t.Fatalf("routing trap should defeat the keyword router: %+v", s)
	}
}

func TestCalibrateDeterministic(t *testing.T) {
	a := calibrate(5, 60)
	b := calibrate(5, 60)
	if a.ToolMean != b.ToolMean {
		t.Fatalf("calibration must be reproducible: %+v vs %+v", a.ToolMean, b.ToolMean)
	}
}
