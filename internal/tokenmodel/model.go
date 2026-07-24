package tokenmodel

// The fitted prediction model. RESEARCH / LATER-USE STATUS: nothing in v7
// scoring consumes this model — the v7 composite is quality-only and audited
// usage is recorded, not scored (see docs/relative-efficiency-bonus-spec.md
// for where efficiency incentives now live). The model and the derive/smoke
// tooling exist so that, once difficulty stabilizes and an absolute budget or
// abuse ceiling is wanted again, its manifest can be DERIVED from a reviewed
// measured campaign instead of paying another 60-run live calibration. See
// docs/token-calibration-transfer.md for the fit, the holdout evidence, and
// the validity preconditions.

// PromptCoefficients predict relay-metered chat PROMPT tokens for one run of
// the unmodified reference harness:
//
//	prompt ≈ ToolCases·cTool + MemoryCases·cMem + ExpectedHops·cHop
//
// Per-case constants absorb the harness template + tool-catalog rendering and
// the average retrieved-context injection; the hop term absorbs the extra
// round trips (with re-sent conversation) of multi-hop tool execution.
type PromptCoefficients struct {
	ToolCases    float64 `json:"tool_cases"`
	MemoryCases  float64 `json:"memory_cases"`
	ExpectedHops float64 `json:"expected_hops"`
}

// CompletionCoefficients predict chat COMPLETION tokens (per-case-type
// constants; completions are small and per-type stable).
type CompletionCoefficients struct {
	ToolCases   float64 `json:"tool_cases"`
	MemoryCases float64 `json:"memory_cases"`
}

// EmbeddingCoefficients predict embedding PROMPT tokens (advisory: embedding
// usage never enters a chat-token manifest; the component exists to detect an
// embedding-profile drift in analysis).
type EmbeddingCoefficients struct {
	HaystackBytes float64 `json:"haystack_bytes"`
	HaystackPairs float64 `json:"haystack_pairs"`
	Subjects      float64 `json:"subjects"`
	MemoryCases   float64 `json:"memory_cases"`
}

// Anchor is the measured-vs-predicted correction at the p90 rank of the fit
// corpus for one run size. Derived budgets are prediction × anchor, so the
// model's systematic bias at the budget point cancels.
type Anchor struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
}

// RatioBounds is the closed interval a dataset-composition ratio must fall in
// for the model to be interpolating rather than extrapolating. Bounds are the
// fit corpus's observed range widened by ~50%; a target dataset outside any
// bound makes the derivation refuse (fail-closed → run a full calibration).
type RatioBounds struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Model is a frozen, versioned fit of the token model: coefficients, per-size
// anchors, fit-quality evidence, the interpolation envelope, and the smoke
// acceptance bands derived from the residual distribution.
type Model struct {
	// Version identifies this exact fit; it lands in derivation provenance.
	Version string
	// HarnessModel / RouteProfile / EmbeddingProfile pin the reference-harness
	// identity the fit is valid for. Transfer preconditions require an exact
	// match — a different chat model, tokenizer, route, or embedding profile
	// invalidates every coefficient below.
	HarnessModel     string
	RouteProfile     string
	EmbeddingProfile string
	// FitCorpusSHA256 is the digest over the fit corpus's per-run
	// (size, seed, dataset_sha256, prompt, completion, total) lines; FitRuns is
	// the number of clean (complete-usage, non-failed, non-rejected) runs.
	FitCorpusSHA256 string
	FitRuns         int

	Prompt     PromptCoefficients
	Completion CompletionCoefficients
	Embedding  EmbeddingCoefficients
	Anchors    map[string]Anchor // per run_size

	// Honest fit evidence (fractions, e.g. 0.05 = 5%): per-run |error| of the
	// chat TOTAL on the full fit corpus, and the end-metric cross-validation
	// error of the anchored p90 derivation procedure (fit half A / derive for
	// half B, both parities).
	ResidualMeanFrac     map[string]float64
	ResidualMaxFrac      map[string]float64
	TransferP90CVMaxFrac map[string]float64

	// Smoke acceptance: a smoke run passes iff its per-run |error| is at most
	// SmokeToleranceFrac[size] × (1 − SmokeBorderZoneFrac). Results inside the
	// border zone are treated as inconclusive and REJECTED (fail-closed).
	SmokeToleranceFrac  map[string]float64
	SmokeBorderZoneFrac float64

	// Envelope is the interpolation guard on dataset-composition ratios.
	Envelope map[string]RatioBounds
}

// Production is the frozen fit of the 2026-07 v7 aggregate calibration
// campaign corpus (59 clean runs of the unmodified starter kit; 20 small, 20
// medium, 19 full; every run's dataset_sha256 reproduced offline before
// fitting; failed/rejected attempts excluded). Fit method: ordinary least
// squares through the origin, pooled across run sizes.
var Production = Model{
	Version:          "tokenmodel-gptoss-v7corpus-2026-07-24-v1",
	HarnessModel:     "openai/gpt-oss-20b",
	RouteProfile:     "openrouter-route-a471cd87ae7df5b9-v1",
	EmbeddingProfile: "dittobench-v7-openrouter-pplx-embed-v1-0.6b-768-v1",
	FitCorpusSHA256:  "97cdfbef4d35ad662f4fd5370b317c1139e4bb2816b5e4bc75102ea332df9f02",
	FitRuns:          59,

	Prompt: PromptCoefficients{
		ToolCases:    3921.201580795692,
		MemoryCases:  3473.627636440864,
		ExpectedHops: 991.8413512457437,
	},
	Completion: CompletionCoefficients{
		ToolCases:   397.12368134012087,
		MemoryCases: 286.5711925069782,
	},
	Embedding: EmbeddingCoefficients{
		HaystackBytes: 0.08655198029424352,
		HaystackPairs: 16.393821037269248,
		Subjects:      14.70819597104064,
		MemoryCases:   42.28166714736028,
	},
	Anchors: map[string]Anchor{
		"small":  {Prompt: 1.1024516818806795, Completion: 1.3164357636332678},
		"medium": {Prompt: 1.1026553807521433, Completion: 1.020893262249707},
		"full":   {Prompt: 1.0612010040680881, Completion: 1.1210285612350495},
	},

	ResidualMeanFrac: map[string]float64{
		"small": 0.0779, "medium": 0.0516, "full": 0.0419,
	},
	ResidualMaxFrac: map[string]float64{
		"small": 0.2715, "medium": 0.1094, "full": 0.0849,
	},
	TransferP90CVMaxFrac: map[string]float64{
		"small": 0.0089, "medium": 0.0657, "full": 0.0518,
	},

	// Tolerances sit above the observed per-run residual maxima with margin
	// (small runs have the largest behavioral share of variance); the 20%
	// border zone rejects near-boundary results as inconclusive.
	SmokeToleranceFrac: map[string]float64{
		"small": 0.40, "medium": 0.16, "full": 0.13,
	},
	SmokeBorderZoneFrac: 0.20,

	// Fit-corpus observed ranges widened ~50% on each side.
	Envelope: map[string]RatioBounds{
		"expected_hops_per_tool_case":    {Min: 0.40, Max: 1.50},
		"prompt_bytes_per_case":          {Min: 20, Max: 120},
		"haystack_bytes_per_memory_case": {Min: 150, Max: 2000},
		"haystack_pairs_per_memory_case": {Min: 1.5, Max: 20},
	},
}

// PredictPromptTokens predicts relay-metered chat prompt tokens (unanchored).
func (m Model) PredictPromptTokens(f Features) float64 {
	return m.Prompt.ToolCases*float64(f.ToolCases) +
		m.Prompt.MemoryCases*float64(f.MemoryCases) +
		m.Prompt.ExpectedHops*float64(f.ExpectedHops)
}

// PredictCompletionTokens predicts chat completion tokens (unanchored).
func (m Model) PredictCompletionTokens(f Features) float64 {
	return m.Completion.ToolCases*float64(f.ToolCases) +
		m.Completion.MemoryCases*float64(f.MemoryCases)
}

// PredictChatTotalTokens is the unanchored chat total.
func (m Model) PredictChatTotalTokens(f Features) float64 {
	return m.PredictPromptTokens(f) + m.PredictCompletionTokens(f)
}

// PredictEmbeddingTokens predicts embedding prompt tokens (advisory).
func (m Model) PredictEmbeddingTokens(f Features) float64 {
	return m.Embedding.HaystackBytes*float64(f.HaystackBytes) +
		m.Embedding.HaystackPairs*float64(f.HaystackPairs) +
		m.Embedding.Subjects*float64(f.Subjects) +
		m.Embedding.MemoryCases*float64(f.MemoryCases)
}

// AnchoredChatTotal is the bias-corrected per-run chat-total prediction for a
// run size — the quantity smoke runs are compared against.
func (m Model) AnchoredChatTotal(runSize string, f Features) (float64, bool) {
	a, ok := m.Anchors[runSize]
	if !ok {
		return 0, false
	}
	return m.PredictPromptTokens(f)*a.Prompt + m.PredictCompletionTokens(f)*a.Completion, true
}

// envelopeRatios computes the dataset-composition ratios the interpolation
// guard checks.
func envelopeRatios(f Features) map[string]float64 {
	ratios := map[string]float64{}
	if f.ToolCases > 0 {
		ratios["expected_hops_per_tool_case"] = float64(f.ExpectedHops) / float64(f.ToolCases)
	}
	if f.Cases() > 0 {
		ratios["prompt_bytes_per_case"] = float64(f.PromptBytes) / float64(f.Cases())
	}
	if f.MemoryCases > 0 {
		ratios["haystack_bytes_per_memory_case"] = float64(f.HaystackBytes) / float64(f.MemoryCases)
		ratios["haystack_pairs_per_memory_case"] = float64(f.HaystackPairs) / float64(f.MemoryCases)
	}
	return ratios
}

// EnvelopeViolations returns the names of composition ratios that fall
// outside the model's interpolation envelope for the given dataset features,
// sorted for determinism. A non-empty result means the model would be
// extrapolating and the derivation must refuse (fail-closed).
func (m Model) EnvelopeViolations(f Features) []string {
	var out []string
	ratios := envelopeRatios(f)
	for name, bounds := range m.Envelope {
		r, ok := ratios[name]
		if !ok {
			continue // degenerate dataset slice (e.g. no memory cases): nothing to check
		}
		if r < bounds.Min || r > bounds.Max {
			out = append(out, name)
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
