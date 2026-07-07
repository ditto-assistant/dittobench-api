package protocol

import "time"

// BenchVersion is the scoring benchmark version stamped into every run's
// details. Bump it with EVERY scoring-affecting change so old and new ledger
// scores are never silently compared (BENCHMARK-V2.md §9). Phase A (the v1
// hardening: seed-derived time, graded memory, trajectory/arg scoring, judge
// hardening) shipped as version 2; v1 was version 1.
//
// Phase B (version 3) — the data engine: the static LongMemEval fixture is
// replaced by the procedural persona/fact-graph generator (internal/persona +
// gen.GenerateMemoryV2), with difficulty tiers, near-miss distractors, seeding
// tiers, dataset hashing, and the 0.5/0.5 composite rebalance. Every one of
// those changes scoring, so they all ride this single bump.
const BenchVersion = 3

// DatasetEpoch is the pinned reference "as-of" instant for all generated
// datasets. Benchmark generation must be a pure function of the run seed and
// bench_version (the reproducibility contract in ditto-subnet
// docs/BENCHMARK-V2.md: same (seed, bench_version) => byte-identical plan-layer
// dataset). Wall-clock time (time.Now) is therefore banned from the generation
// path; haystack base dates are drawn *backward* from this fixed epoch instead,
// and the GeneratedAt envelope fields carry it verbatim so two runs of the same
// seed diff clean.
//
// It is NOT a wire timestamp of when a run physically executed: the subnet
// validator stamps its own wall-clock generated_at on the ScoreReport it
// forwards to the platform (ditto/validator/dittobench.py), and generated_at is
// not part of the platform's DB/signature contract. Pinned per bench_version;
// bump it only alongside a bench_version bump.
var DatasetEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// DatasetEpochRFC3339 is DatasetEpoch pre-rendered for the string GeneratedAt
// envelope fields (protocol.Dataset, protocol.ScoreReport). Derived from
// DatasetEpoch so the two can never drift.
var DatasetEpochRFC3339 = DatasetEpoch.UTC().Format(time.RFC3339)
