package main

import "testing"

// TestV5ShipGateMultiSeed is the hermetic, cross-seed realization of the v5 plan's
// 4.11 ship gate (offline half) and its counter-guard. It runs the harness
// archetypes against REAL generated v5 datasets over 24 rotating seeds through the
// real scorer pipeline and asserts:
//
//   - The Unicorn-style leak-router and the no-model cleartext parser drop below
//     the 0.85 champion line on EVERY seed (the exploit is closed).
//   - A conversationally-grounded harness with the SAME strong retrieval stack as
//     the router stays far above them (the drop is specific to the conversational
//     margin, not a blanket difficulty hike -- the counter-guard).
//   - The conversational-sanity metric separates them cleanly (~high vs 0).
//
// The on-model winnability precheck against the frozen-Qwen reference still needs
// the model relay; this pins the half that is measurable hermetically.
func TestV5ShipGateMultiSeed(t *testing.T) {
	seeds := make([]int64, 24)
	for i := range seeds {
		seeds[i] = 20260720 + int64(i)
	}
	results, err := Calibrate(seeds, "full")
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Archetype] = r
		t.Logf("%-16s comp=%.3f±%.3f mem=%.3f conv=%.3f below0.85=%.0f%%",
			r.Archetype, r.CompositeMean, r.CompositeStdErr, r.MemoryMean, r.ConvSanityMean, r.FracBelow085*100)
	}

	router := byName["unicorn-router"]
	parser := byName["cleartext-parser"]
	honest := byName["honest-strong"]

	// The exploiters drop below the champion line on every seed.
	if router.FracBelow085 != 1.0 {
		t.Errorf("unicorn-router must be <0.85 on every seed, got %.0f%%", router.FracBelow085*100)
	}
	if parser.FracBelow085 != 1.0 {
		t.Errorf("cleartext-parser must be <0.85 on every seed, got %.0f%%", parser.FracBelow085*100)
	}

	// Conversational sanity separates honest from the exploiters.
	if honest.ConvSanityMean < 0.7 {
		t.Errorf("honest harness must pass conversational sanity, got %.3f", honest.ConvSanityMean)
	}
	if router.ConvSanityMean != 0 || parser.ConvSanityMean != 0 {
		t.Errorf("exploiters must fail conversational sanity, got router=%.3f parser=%.3f", router.ConvSanityMean, parser.ConvSanityMean)
	}

	// Counter-guard: same retrieval stack, but grounding conversation keeps the
	// honest harness far above the router -- the drop is specific to the
	// conversational margin, not a blanket difficulty hike.
	if honest.CompositeMean-router.CompositeMean < 0.25 {
		t.Errorf("v5 must separate honest (%.3f) from the same-retrieval router (%.3f) by >0.25", honest.CompositeMean, router.CompositeMean)
	}
	// And the honest harness is not dragged down by the conversational gate: its
	// composite retains most of its memory mean (conv factor ~1.0), while the
	// router loses roughly half of its (equal) memory mean to the gate.
	honestRetained := honest.CompositeMean / honest.MemoryMean
	routerPenalized := router.CompositeMean / router.MemoryMean
	if honestRetained < 0.8 {
		t.Errorf("honest harness must retain most of its memory mean (no conversational penalty), retained %.2f", honestRetained)
	}
	if routerPenalized > 0.65 {
		t.Errorf("router must be penalized by the conversational gate, retained %.2f", routerPenalized)
	}
	// A conversationally-grounded strong harness can still reach champion scores
	// under v5 (winnability: v5 did not make the ceiling degenerate).
	if honest.CompositeMax < 0.85 {
		t.Errorf("a strong grounded harness must be able to reach >=0.85 under v5, max %.3f", honest.CompositeMax)
	}
}
