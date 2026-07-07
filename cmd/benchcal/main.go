// Command benchcal is the offline DittoBench calibration harness (A9).
//
// It measures between-seed variance of the DETERMINISTIC tool suite — the clean,
// judge-independent difficulty signal (§6.2, W1) — by generating N datasets
// (rotating seed) and scoring each against the deterministic reference routing
// policy (internal/refharness) in-process. No Docker, no OpenRouter key: the
// only thing that varies between runs is the dataset, so the spread of tool_mean
// IS the spread of dataset difficulty. It reports overall + per-category mean and
// stddev, plus a repeat-seed noise floor (which must be ~0 for a deterministic
// harness), as JSON.
//
// The memory-suite / composite σ needs the live judge + a harness image; drive
// that with cmd/calibrate --run-size full against a running API. benchcal covers
// the deterministic half that can be measured hermetically and committed.
//
//	benchcal --runs 30 --n 90 --out docs/benchcal-toolsuite.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"

	"github.com/ditto-assistant/dittobench-api/internal/catalog"
	"github.com/ditto-assistant/dittobench-api/internal/datagen"
	"github.com/ditto-assistant/dittobench-api/internal/refharness"
	"github.com/ditto-assistant/dittobench-api/internal/scorer"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// Stat summarizes a series of per-seed values.
type Stat struct {
	Mean   float64 `json:"mean"`
	Stddev float64 `json:"stddev"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	N      int     `json:"n"`
}

// Report is the committed calibration artifact.
type Report struct {
	Runs             int             `json:"runs"`
	NTools           int             `json:"n_tools"`
	Harness          string          `json:"harness"`
	ToolMean         Stat            `json:"tool_mean"`
	PerCategory      map[string]Stat `json:"per_category"`
	NoiseFloorStddev float64         `json:"noise_floor_stddev"` // repeat-seed σ; must be ~0
	Note             string          `json:"note"`
}

// runOnce generates the tool dataset for a seed, routes each case through the
// deterministic reference policy, and returns the overall tool_mean plus the
// per-category means for that seed.
func runOnce(seed int64, n int) (float64, map[string]float64) {
	ds := datagen.Generate(seed, n)
	tools := catalog.Catalog()
	var sum float64
	catSum := map[string]float64{}
	catCount := map[string]int{}
	for _, c := range ds.ToolCases {
		resp := protocol.RunResponse{ToolCalls: refharness.Route(c.Prompt, tools), LatencyMs: 1}
		cs := scorer.ScoreToolCase(c, resp, true)
		sum += cs.ToolScore
		catSum[c.Category] += cs.ToolScore
		catCount[c.Category]++
	}
	perCat := map[string]float64{}
	for cat, s := range catSum {
		perCat[cat] = s / float64(catCount[cat])
	}
	mean := 0.0
	if len(ds.ToolCases) > 0 {
		mean = sum / float64(len(ds.ToolCases))
	}
	return mean, perCat
}

// calibrate runs `runs` seeds (1..runs) plus a repeat-seed noise-floor pass.
func calibrate(runs, n int) Report {
	toolMeans := make([]float64, 0, runs)
	catSeries := map[string][]float64{}
	for i := 1; i <= runs; i++ {
		mean, perCat := runOnce(int64(i), n)
		toolMeans = append(toolMeans, mean)
		for cat, v := range perCat {
			catSeries[cat] = append(catSeries[cat], v)
		}
	}

	// Noise floor: the same seed repeated must give identical tool_mean (σ=0),
	// confirming the measurement isolates dataset difficulty, not harness noise.
	var floor []float64
	for r := 0; r < 5; r++ {
		m, _ := runOnce(7, n)
		floor = append(floor, m)
	}

	perCat := map[string]Stat{}
	for cat, series := range catSeries {
		perCat[cat] = stat(series)
	}
	return Report{
		Runs:             runs,
		NTools:           n,
		Harness:          "refharness (deterministic keyword router)",
		ToolMean:         stat(toolMeans),
		PerCategory:      perCat,
		NoiseFloorStddev: stat(floor).Stddev,
		Note:             "Deterministic tool-suite difficulty variance only; memory/composite σ needs the live judge (cmd/calibrate --run-size full).",
	}
}

func main() {
	runs := flag.Int("runs", 30, "number of seeds (1..runs)")
	n := flag.Int("n", 90, "tool cases per run")
	out := flag.String("out", "", "write the JSON report to this path (default stdout)")
	flag.Parse()

	rep := calibrate(*runs, *n)
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		log.Fatalf("marshal report: %v", err)
	}
	if *out == "" {
		fmt.Println(string(b))
		return
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	log.Printf("benchcal: %d seeds × %d tool cases → %s (tool_mean σ=%.4f, noise floor σ=%.4f)",
		rep.Runs, rep.NTools, *out, rep.ToolMean.Stddev, rep.NoiseFloorStddev)
}

// stat computes mean/stddev(population)/min/max of a series.
func stat(xs []float64) Stat {
	if len(xs) == 0 {
		return Stat{}
	}
	sort.Float64s(xs)
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var varSum float64
	for _, x := range xs {
		d := x - mean
		varSum += d * d
	}
	return Stat{
		Mean:   round4(mean),
		Stddev: round4(math.Sqrt(varSum / float64(len(xs)))),
		Min:    round4(xs[0]),
		Max:    round4(xs[len(xs)-1]),
		N:      len(xs),
	}
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }
