// Package gen produces FRESH, ANTI-CHEAT benchmark datasets for a single
// submission. It is the centerpiece of the validator: the permutation space is
// made astronomically large so a miner cannot precompute a lookup table or
// overfit to a fixed dataset.
//
// # Anti-cheat combinatorics
//
// Each submission draws a fresh crypto-random seed (see FreshSeed, logged with
// the run). Every layer below multiplies the entropy:
//
// Tool cases:
//   - category × template × entity are sampled per case from large pools
//     (datagen), and then each prompt is LLM-PARAPHRASED by the generator model
//     so the surface wording is novel every run (effectively unbounded entropy
//     per case — natural-language rephrasings of one intent are uncountable).
//   - The deterministic ground truth (expected_tools + expected_behavior) is
//     preserved, so scoring stays exact while the prompt the miner sees never
//     repeats.
//
// Memory cases (LongMemEval):
//   - n questions are chosen uniformly at random from the ~500-question oracle:
//     C(500, n) selections (e.g. C(500,20) ≈ 2.7e35; C(500,50) ≈ 1e69).
//   - D distractor pairs are injected from the ~5000 non-selected pairs:
//     C(~5000, D) further selections (C(5000,100) ≈ 1e241).
//   - every pair gets a FRESH timestamp: a random base date over a multi-year
//     window, per-session day offsets, and per-pair jitter, then the whole
//     haystack is shuffled — so temporal structure (and ordering) differs each
//     run.
//   - a random fraction of pairs AND the selected questions are LLM-paraphrased
//     (facts/answers preserved) for textual freshness.
//
// Product: C(500,n) × C(5000,D) × timestamp-space × paraphrase-entropy. Even the
// small profile is far beyond any feasible lookup table; medium/full are
// astronomically larger. Combined with a per-submission crypto-random seed, the
// dataset is effectively non-repeating.
package gen

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math/rand"
)

// LLM is the minimal chat interface gen needs (the generator model). A nil LLM
// disables paraphrasing (cases keep their templated wording) — used in tests.
type LLM interface {
	Complete(ctx context.Context, model, system, user string) (string, error)
}

// Profile is the size knobs for a run_size.
type Profile struct {
	Tools          int     // number of tool cases
	Mem            int     // number of memory cases
	Distractors    int     // (legacy LongMemEval knob; unused by the v2 persona engine)
	ParaphraseFrac float64 // fraction of prompts/pairs to LLM-paraphrase [0,1]
	// Waves is the number of staged Tier-C seeding waves (1 = single seed).
	// RawPairsFrac is the fraction of memory questions seeded pairs-only (Tier B).
	// small stays a single Tier-A wave so the smoke path is cheap + simple.
	Waves        int
	RawPairsFrac float64
	// IsoCases is the number of multi-graph isolation cases: a second
	// persona is seeded under a different user_id and these cases query one user
	// while the other holds a conflicting value. 0 keeps the small/smoke path
	// single-user and cheap.
	IsoCases int
}

// Profiles maps run_size → counts. small is kept CHEAP (few LLM calls) so local
// testing is fast.
var Profiles = map[string]Profile{
	"small":  {Tools: 6, Mem: 6, Distractors: 20, ParaphraseFrac: 0.3, Waves: 1, RawPairsFrac: 0, IsoCases: 0},
	"medium": {Tools: 20, Mem: 20, Distractors: 100, ParaphraseFrac: 0.5, Waves: 2, RawPairsFrac: 0.3, IsoCases: 2},
	"full":   {Tools: 60, Mem: 50, Distractors: 300, ParaphraseFrac: 0.7, Waves: 2, RawPairsFrac: 0.35, IsoCases: 4},
}

// ProfileFor returns the Profile for a run_size, defaulting to small.
func ProfileFor(runSize string) (Profile, bool) {
	p, ok := Profiles[runSize]
	if !ok {
		return Profiles["small"], false
	}
	return p, true
}

// FreshSeed returns a cryptographically-random int64 seed for a submission. The
// caller logs it with the run so a dispute can reproduce the exact dataset.
func FreshSeed() int64 {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to a non-zero constant-ish value.
		return int64(1)
	}
	// Mask to a non-negative int64 for clean logging/JSON.
	return int64(binary.LittleEndian.Uint64(b[:]) >> 1)
}

// NewRNG returns a math/rand source seeded from seed. Determinism per seed is
// what lets a logged run be reproduced exactly.
func NewRNG(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}

// fmtErr wraps a stage error with package context.
func fmtErr(stage string, err error) error {
	return fmt.Errorf("gen %s: %w", stage, err)
}
