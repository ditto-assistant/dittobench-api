// Command benchcal is the offline DittoBench calibration harness.
//
// It measures between-seed variance of the DETERMINISTIC tool suite — the clean
// difficulty signal — by generating N datasets (rotating seed) and scoring each
// against the deterministic reference routing policy (internal/refharness)
// in-process. No Docker, no key: the only thing that varies between runs is the
// dataset, so the spread of tool_mean IS the spread of dataset difficulty. It
// reports overall + per-category mean and stddev, plus a repeat-seed noise
// floor (which must be ~0 for a deterministic harness), as JSON.
//
// The memory-suite / composite σ under a real model needs a harness image;
// drive that with cmd/calibrate --run-size full against a running API. benchcal
// covers the half that can be measured hermetically and committed.
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

	"github.com/ditto-assistant/dittobench-api/internal/refharness"
	"github.com/ditto-assistant/dittobench-api/internal/scorer"
	"github.com/ditto-assistant/dittobench-datagen/catalog"
	"github.com/ditto-assistant/dittobench-datagen/datagen"
	"github.com/ditto-assistant/dittobench-datagen/gen"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// refMemoryCompetence is a FIXED (seed-independent) reference harness profile:
// how well a mid-tier harness answers each v2 memory question type. Because the
// suite stratifies the type mix by a fixed per-run quota, applying a fixed
// per-type score makes the structural memory_mean identical across seeds — i.e.
// v2 removed v1's question-type-draw variance. The residual real σ comes
// from the LLM judge + harness stochasticity, which this hermetic pass cannot
// see; it is measured by the hosted 30-seed run.
var refMemoryCompetence = map[string]float64{
	"single-session-recall":  0.90,
	"multi-session":          0.60,
	"temporal-reasoning":     0.45,
	"knowledge-update":       0.70,
	"preference":             0.88,
	"preference-application": 0.60,
	"contradiction":          0.45,
	"abstention":             0.55,
}

func refMemoryScore(qtype string) float64 {
	if v, ok := refMemoryCompetence[qtype]; ok {
		return v
	}
	return 0.5
}

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
	NMem             int             `json:"n_mem"`
	Harness          string          `json:"harness"`
	ToolMean         Stat            `json:"tool_mean"`
	MemoryMean       Stat            `json:"memory_mean"` // structural (fixed reference policy)
	Composite        Stat            `json:"composite"`   // 0.5 tool + 0.5 memory, per seed
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

// memRunOnce builds the v2 memory suite for a seed (deterministically) and
// scores it under the fixed reference-competence policy — a hermetic proxy for
// structural memory_mean.
func memRunOnce(seed int64, nMem int) float64 {
	suite := gen.GenerateMemorySuite(gen.NewRNG(seed), seed, nMem, 1, 0)
	if len(suite.Cases) == 0 {
		return 0
	}
	var sum float64
	for _, sc := range suite.Cases {
		sum += refMemoryScore(sc.Case.QuestionType)
	}
	return sum / float64(len(suite.Cases))
}

// calibrate runs `runs` seeds (1..runs) plus a repeat-seed noise-floor pass.
func calibrate(runs, n, nMem int) Report {
	toolMeans := make([]float64, 0, runs)
	memMeans := make([]float64, 0, runs)
	composites := make([]float64, 0, runs)
	catSeries := map[string][]float64{}
	for i := 1; i <= runs; i++ {
		mean, perCat := runOnce(int64(i), n)
		mm := memRunOnce(int64(i), nMem)
		toolMeans = append(toolMeans, mean)
		memMeans = append(memMeans, mm)
		composites = append(composites, 0.5*mean+0.5*mm)
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
		NMem:             nMem,
		Harness:          "refharness (tools) + fixed reference-competence policy (memory)",
		ToolMean:         stat(toolMeans),
		MemoryMean:       stat(memMeans),
		Composite:        stat(composites),
		PerCategory:      perCat,
		NoiseFloorStddev: stat(floor).Stddev,
		Note:             "Hermetic structural variance: tool σ = deterministic routing difficulty; memory σ ≈ 0 shows v2 stratification removed question-type-mix variance. Real champion-region composite σ (judge + harness noise) needs the hosted 30-seed run; the KOTH margin/score_tol retune assumes the design target σ ≤ 0.01 and must be reconfirmed there.",
	}
}

func main() {
	runs := flag.Int("runs", 30, "number of seeds (1..runs)")
	n := flag.Int("n", 90, "tool cases per run")
	nMem := flag.Int("nmem", 50, "memory cases per run")
	out := flag.String("out", "", "write the JSON report to this path (default stdout)")
	flag.Parse()

	rep := calibrate(*runs, *n, *nMem)
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
	log.Printf("benchcal: %d seeds × (%d tool, %d mem) → %s (tool σ=%.4f, mem σ=%.4f, composite σ=%.4f, noise floor σ=%.4f)",
		rep.Runs, rep.NTools, rep.NMem, *out, rep.ToolMean.Stddev, rep.MemoryMean.Stddev, rep.Composite.Stddev, rep.NoiseFloorStddev)
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
