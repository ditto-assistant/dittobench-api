package protocol

import "time"

// BenchVersion is the scoring benchmark version stamped into every run's
// details. The §9 policy — bump it with EVERY scoring-affecting change so old and
// new ledger scores are never silently compared, and re-score the ledger on a
// bump — exists to protect a LIVE ledger's comparability. It only matters once
// miners are actually scoring against a version.
//
// v1 was version 1. The v2 redesign (Phases A/B/C below) is stamped **version 2**
// and held there: NO miner has ever been scored against benchmark v2, so there is
// no ledger to keep comparable and nothing to re-score. Collapsing A/B/C into a
// single version 2 avoids minting throwaway versions (and throwaway re-score
// sweeps) for a benchmark that has never gone live. The per-change bump policy
// resumes from version 3 for the FIRST scoring change made AFTER v2 is live and a
// miner has scored against it.
//
//   - Phase A — v1 hardening: seed-derived time, graded memory, trajectory/arg
//     scoring, judge hardening.
//   - Phase B — the data engine: the static LongMemEval fixture replaced by the
//     procedural persona/fact-graph generator (internal/persona +
//     gen.GenerateMemoryV2), difficulty tiers, near-miss distractors, seeding
//     tiers, dataset hashing, the 0.5/0.5 composite rebalance.
//   - Phase C — observed execution (§7): the validator serves a mock
//     tool-execution endpoint (RunRequest.ToolEndpoint) and scores a tool case on
//     the OBSERVED trajectory rather than self-report (retires W3); result-usage
//     scoring (C2) and multi-graph isolation (C3).
//
// All three ship together as version 2 while v2 is pre-launch.
const BenchVersion = 2

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
