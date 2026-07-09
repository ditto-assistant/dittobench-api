package main

import (
	"net/http"

	"github.com/ditto-assistant/dittobench-api/internal/gen"
)

// Public dataset sampler (task #52).
//
// Lets the community inspect the benchmark's *shape and difficulty* — the real
// run-size DatasetArtifact a validator scores, not the simpler /v1/dataset
// practice generator — without exposing any answer key or any scored
// submission's seed.
//
// Two structural guarantees keep it safe:
//
//  1. Public-seed namespace. The sampler NEVER accepts a caller-supplied raw
//     seed; it derives the seed from a fixed public base in the NEGATIVE int64
//     range. Every per-submission seed is non-negative (gen.FreshSeed masks with
//     >>1; the platform draws secrets.randbits(63)), so a sample seed can never
//     collide with — or be used to pre-compute — a real scored dataset. A miner
//     cannot point the sampler at a future submission's (secret, not-yet-drawn)
//     seed.
//  2. Redaction. The response carries only harness-visible prompts/questions plus
//     aggregate shape counts. Expected tools/answers, forbidden answers, the
//     seeded memory facts (haystack), and the tool needles are dropped by
//     construction, so the sampler can never be scraped into a prompt→answer key.
const publicSampleSeedBase int64 = -1_000_000_007

// publicSampleSeed maps a sample index to its reserved-namespace public seed.
func publicSampleSeed(index int) int64 {
	return publicSampleSeedBase - int64(index)
}

// How many distinct public samples an index may select (0..maxSampleIndex).
const maxSampleIndex = 9

// datasetSample is the answer-free public view of a run-size DatasetArtifact:
// the harness-visible prompts/questions plus the aggregate shape, and nothing
// that could serve as an answer key.
type datasetSample struct {
	Seed         int64              `json:"seed"`
	BenchVersion int                `json:"bench_version"`
	RunSize      string             `json:"run_size"`
	Sample       int                `json:"sample"`
	GeneratedAt  string             `json:"generated_at"`
	Shape        sampleShape        `json:"shape"`
	ToolCases    []sampleToolCase   `json:"tool_cases"`
	MemoryCases  []sampleMemoryCase `json:"memory_cases,omitempty"`
	Note         string             `json:"note"`
}

// sampleShape is the aggregate difficulty picture: how many cases of each kind,
// how the memory haystack is staged, and the category/type mix.
type sampleShape struct {
	ToolCaseCount   int            `json:"tool_case_count"`
	MemoryCaseCount int            `json:"memory_case_count"`
	MemoryWaves     int            `json:"memory_waves"`
	IsolationCases  int            `json:"isolation_cases"`
	ToolCategories  map[string]int `json:"tool_categories"`
	MemoryTypes     map[string]int `json:"memory_types"`
}

// sampleToolCase exposes a tool case's prompt plus its difficulty knobs
// (category, call budget, ordering/extra-call tolerance) — never ExpectedTools
// or ExpectedBehavior.
type sampleToolCase struct {
	ID              string `json:"id"`
	Category        string `json:"category"`
	Prompt          string `json:"prompt"`
	MaxToolCalls    int    `json:"max_tool_calls"`
	AllowExtraTools bool   `json:"allow_extra_tools"`
	Unordered       bool   `json:"unordered,omitempty"`
}

// sampleMemoryCase exposes a memory case's question + its type — never
// ExpectedAnswer or ForbiddenAnswer.
type sampleMemoryCase struct {
	ID           string `json:"id"`
	QuestionType string `json:"question_type"`
	Question     string `json:"question"`
}

// handleSample serves a redacted, shape-only view of a real run-size dataset
// from a reserved public seed. Query: ?run_size=small|medium|full (default
// small), ?sample=<0..maxSampleIndex> (default 0). No key required — generation
// is deterministic and LLM-free.
func (s *server) handleSample(w http.ResponseWriter, r *http.Request) {
	runSize := r.URL.Query().Get("run_size")
	if runSize == "" {
		runSize = "small"
	}
	prof, ok := gen.ProfileFor(runSize)
	if !ok {
		writeError(w, http.StatusBadRequest, "run_size must be one of small|medium|full")
		return
	}
	index := parseIntDefault(r.URL.Query().Get("sample"), 0)
	if index < 0 || index > maxSampleIndex {
		writeError(w, http.StatusBadRequest, "sample must be between 0 and 9")
		return
	}
	seed := publicSampleSeed(index)
	art := gen.GenerateDataset(seed, prof)
	writeJSON(w, http.StatusOK, redactArtifact(art, runSize, index))
}

// redactArtifact reduces a DatasetArtifact to the public shape-only sample:
// prompts/questions + aggregate counts, never any grading data.
func redactArtifact(a gen.DatasetArtifact, runSize string, index int) datasetSample {
	shape := sampleShape{
		MemoryWaves:    len(a.MemoryWaves),
		ToolCategories: map[string]int{},
		MemoryTypes:    map[string]int{},
	}
	tools := make([]sampleToolCase, 0, len(a.ToolCases))
	for _, c := range a.ToolCases {
		tools = append(tools, sampleToolCase{
			ID:              c.ID,
			Category:        c.Category,
			Prompt:          c.Prompt,
			MaxToolCalls:    c.MaxToolCalls,
			AllowExtraTools: c.AllowExtraTools,
			Unordered:       c.Unordered,
		})
		shape.ToolCategories[c.Category]++
	}
	mems := make([]sampleMemoryCase, 0, len(a.MemoryCases))
	for _, c := range a.MemoryCases {
		mems = append(mems, sampleMemoryCase{
			ID:           c.ID,
			QuestionType: c.QuestionType,
			Question:     c.Question,
		})
		shape.MemoryTypes[c.QuestionType]++
		// Isolation cases are the multi-graph cases scoped to a non-default user.
		if c.UserID != "" {
			shape.IsolationCases++
		}
	}
	shape.ToolCaseCount = len(tools)
	shape.MemoryCaseCount = len(mems)
	return datasetSample{
		Seed:         a.Seed,
		BenchVersion: a.BenchVersion,
		RunSize:      runSize,
		Sample:       index,
		GeneratedAt:  a.GeneratedAt,
		Shape:        shape,
		ToolCases:    tools,
		MemoryCases:  mems,
		Note: "public sample from a reserved negative-seed namespace; " +
			"not a scored submission's seed, and answer keys are redacted",
	}
}
