// Package tokenmodel is the deterministic token-calibration transfer model:
// it predicts a reference-harness run's relay-metered chat token usage from
// OFFLINE dataset features, so a bench-version or dataset change can derive
// its token-baseline manifest from an existing reviewed one instead of paying
// a 60-run live calibration campaign.
//
// The model was fit on the v7 aggregate calibration corpus (60 live runs of
// the unmodified starter kit over 20 seeds × {small, medium, full}, OpenRouter
// route openrouter-route-a471cd87ae7df5b9-v1, openai/gpt-oss-20b, see
// docs/token-calibration-transfer.md for the fit and holdout numbers). It is
// only valid while the reference-harness identity holds: same locked chat
// model, same embedding profile, same starter-kit prompt template. Any of
// those changing requires a full live recalibration, and SmokeValidate is the
// fail-closed check that catches a drifted world.
package tokenmodel

import (
	"github.com/ditto-assistant/dittobench-datagen/gen"
	"github.com/ditto-assistant/dittobench-datagen/toolexec"
)

// Features are the offline-computable dataset quantities the token model
// consumes. They are pure functions of the generated dataset artifact — no
// live run, network, or tokenizer is involved — so any third party can
// recompute them from (seed, run_size, bench_version) alone.
type Features struct {
	// ToolCases and MemoryCases count the dataset's chat round-trip floor:
	// every case is at least one /run call by the harness against the locked
	// model.
	ToolCases   int `json:"tool_cases"`
	MemoryCases int `json:"memory_cases"`
	// ExpectedHops is the expected count of observed tool executions: the sum
	// of expected tools over observable tool cases, plus one forced retry for
	// AllowExtraTools cases (the serving layer fails the first content call).
	// Each hop is an extra model round trip that re-sends the grown
	// conversation.
	ExpectedHops int `json:"expected_hops"`
	// PromptBytes is the rendered question surface: tool-case prompts plus
	// memory-case questions, in bytes.
	PromptBytes int `json:"prompt_bytes"`
	// HaystackBytes and HaystackPairs describe the seeded memory corpus (pair
	// prompt+response bytes; count). They drive the embedding load and the
	// retrieved-context size memory answers re-inject into chat prompts.
	HaystackBytes int `json:"haystack_bytes"`
	HaystackPairs int `json:"haystack_pairs"`
	// Subjects counts haystack subject records (embedded alongside pairs).
	Subjects int `json:"subjects"`
}

// Extract computes Features from a generated dataset artifact. Deterministic:
// the artifact is a pure function of (seed, profile, bench_version) and every
// quantity below is a pure function of the artifact.
func Extract(a gen.DatasetArtifact) Features {
	var f Features
	f.ToolCases = len(a.ToolCases)
	f.MemoryCases = len(a.MemoryCases)
	for _, c := range a.ToolCases {
		f.PromptBytes += len(c.Prompt)
		if toolexec.Observable(c) {
			f.ExpectedHops += len(c.ExpectedTools)
			if c.AllowExtraTools {
				f.ExpectedHops++
			}
		}
	}
	for _, mc := range a.MemoryCases {
		f.PromptBytes += len(mc.Question)
	}
	for _, w := range a.MemoryWaves {
		f.HaystackPairs += len(w.Pairs)
		f.Subjects += len(w.Subjects)
		for _, p := range w.Pairs {
			f.HaystackBytes += len(p.Prompt) + len(p.Response)
		}
	}
	return f
}

// Cases is the chat round-trip floor (every case is one /run).
func (f Features) Cases() int { return f.ToolCases + f.MemoryCases }
