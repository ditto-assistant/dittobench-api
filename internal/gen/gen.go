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
//     (datagen); the prompt is one of the category's hand-written phrasing
//     variants, chosen by the seeded rng, so the surface wording varies per seed
//     while the deterministic ground truth (expected_tools + expected_behavior)
//     is preserved — scoring stays exact and the dataset is reproducible.
//
// Memory cases (persona engine):
//   - a procedural persona universe is built from the seed; its beats render to a
//     haystack from deterministic template surfaces (seeded phrasing variants),
//     and questions are derived from ground truth with per-(seed, question)
//     phrasing variants.
//   - every pair gets a FRESH timestamp anchored to the dataset epoch (per-session
//     day offsets + per-pair spacing), so temporal structure differs each run.
//
// Generation is fully non-LLM and byte-reproducible from (seed, bench_version):
// the anti-cheat entropy is the seed-driven combinatorics (persona facts,
// template-variant selection, timestamps, sampling) plus a fresh per-submission
// crypto-random seed, not a post-hoc paraphrase. Even the small profile is far
// beyond any feasible lookup table; medium/full are astronomically larger.
package gen

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math/rand"
)

// Profile is the size knobs for a run_size.
type Profile struct {
	Tools       int // number of tool cases
	Mem         int // number of memory cases
	Distractors int // (legacy LongMemEval knob; unused by the v2 persona engine)
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

// Profiles maps run_size → counts.
var Profiles = map[string]Profile{
	"small":  {Tools: 6, Mem: 6, Distractors: 20, Waves: 1, RawPairsFrac: 0, IsoCases: 0},
	"medium": {Tools: 20, Mem: 20, Distractors: 100, Waves: 2, RawPairsFrac: 0.3, IsoCases: 2},
	"full":   {Tools: 60, Mem: 50, Distractors: 300, Waves: 2, RawPairsFrac: 0.35, IsoCases: 4},
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
