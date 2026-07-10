// Package llm holds the model identity the validator enforces. There are no
// LLM calls in the validator itself anymore: generation is non-LLM, scoring is
// the deterministic judge-free grader (dittobench-datagen/grade), and the only
// model in a run is the locked one the harness talks to through the gateway.
package llm

import (
	"os"
	"strings"
)

const (
	// EnvAPIKey is the env var holding an OpenRouter key. Only the legacy
	// pre-lock Docker path uses it (forwarded to the crate's agent); under the
	// model lock no key exists anywhere in a run.
	EnvAPIKey = "OPENROUTER_API_KEY"

	// defaultHarnessModel is the locked open-weight model every miner harness
	// is scored against. Locking the harness to ONE open-weight model shrinks
	// the exploit surface (no model routing to game) and makes the k=3
	// validators' scores of a submission comparable. This is the ONE place the
	// locked model id lives; bump it here (or via HARNESS_MODEL). It must name
	// the model the host gateway actually serves.
	defaultHarnessModel = "qwen/qwen3-32b"
)

// HarnessModel returns the locked model id every miner harness is scored
// against (env HARNESS_MODEL or the default). It is the model-routing
// indirection: callers force this on the sandbox instead of letting the
// harness pick its own model. See defaultHarnessModel.
func HarnessModel() string {
	if m := strings.TrimSpace(os.Getenv("HARNESS_MODEL")); m != "" {
		return m
	}
	return defaultHarnessModel
}
