# DittoBench v7 difficulty contract (~10x)

Benchmark version 7 is the difficulty release: the datagen suite gets ~10x
harder (more cases, denser haystacks, more adversarial paraphrase — tracked in
`dittobench-datagen`) and this validator's scoring gets strictly harder. This
document is the validator-side half: what changed, why each change is safe for
v2–v6, and the measured difficulty identity.

Everything is gated on `bench_version >= 7` (`protocol.BenchVersionV7`). No
wire type changed; the only wire difference v7 harnesses see is the additive
`bench_version` field on `RunRequest` that has shipped since the v6
byte-compatibility work (#68).

## Difficulty target

Naive/pattern-matching strategies must score roughly an order of magnitude
lower under v7 scoring than under v6, while a correct oracle response set
still scores full marks.

Measured identity, pinned by `TestV7DifficultyNaiveVsOracle`
(`internal/scorer/v7_test.go`) through the same scoring pipeline
`runSizeJob` drives:

| strategy | v6 practice composite | v7 practice composite | ratio |
| --- | --- | --- | --- |
| naive parser (self-reported trajectories, decoy-grepped answers) | 0.475 | 0.0375 | **12.7x lower** |
| oracle (observed execution, correct trajectories, needles incorporated) | 1.000 | 1.000 | 1.0x |

The naive fixture is the deterministic-parser archetype the anti-cheat work
targets: six observable tool cases answered with a plausible self-report and
no `tool_endpoint` execution, plus two result-usage cases where a number was
grepped out of the wrong tool's payload (the served decoy).

End-to-end companion on REAL generated suites
(`TestV7DifficultyOnGeneratedToolSuites`, `cmd/dittobench-api`), medium tool
suites from the hardened datagen (seed 123456789):

| strategy | v6 suite + v6 scoring | v7-hard suite + v7 scoring | ratio |
| --- | --- | --- | --- |
| naive parser (best-case self-reports, fails the negation trap) | 0.600 | 0.178 | **3.4x lower** |
| oracle | 1.000 | 1.000 | 1.0x |

The suite-level ratio is smaller than the lever-level 12.7x because
abstention/no-tool and memory-routing cases legitimately reward inaction in
both contracts; on the observable slice — where the parser's free credit
lived — the collapse is the full 10x (0.5 → 0.05 per case, traps at 0). The
memory-axis hardening is measured in the datagen module's own v7 difficulty
tests.

The v2–v6 benchmark evidence in
`docs/third-party-benchmark-timeline/` (#76, native Hermes and OpenClaw
harnesses on OpenRouter/Nebius) is the calibration backdrop: competent honest
harnesses execute through `tool_endpoint`, self-report faithfully, and
incorporate served needle values, so none of the v7 levers below move an
honest run that was already scoring on observed execution.

## The v7 levers (validator side)

Per-case (`internal/scorer/v7.go`, wired in `cmd/dittobench-api/main.go`):

| lever | v2–v6 | v7 |
| --- | --- | --- |
| observable-but-unobserved ceiling (practice) | 0.5 | **0.05** (scored scope stays 0) |
| result-usage composition | `0.4·traj + 0.6·usage` | **`traj × gate`**: needle 1.0 / miss 0.1 / decoy 0.0 (whole case) |
| self-report vs observed trajectory mismatch | unscored (observed replaces self-report) | **×0.5** on the case (non-empty disagreeing self-report) |
| forbidden argument on an expected tool | dents arg precision | **case scores 0** |
| hop order on ordered multi-hop | scales the 0.2 trajectory term | **multiplies the whole score** |
| extra-call / over-budget penalty | `extras/expected` | **doubled** |

Composite gates (`compositeGateV7`):

| gate | v3–v6 | v7 |
| --- | --- | --- |
| tool-efficiency free overshoot / saturation / max | 1 / +5 / 15% | **0 / +3 / 40%** (gate score 0.5 → 0.6) |
| memory over-call max | 10% | **25%** |
| metamorphic split max | 15% | **40%** |
| bounded-product floor | 0.75 | **0.40** |
| conversational-sanity floor | 0.5 | **0.25** |
| canary leak multiplier | 0.5 | **0.25** |
| transform audit | observational (env-gated), 15% | **enforced by contract, 40%** |

The transform audit keeps its directional key (base-only minus transform-only
splits ≥ the 4-pair minimum), so symmetric honest noise still gates 1.0 — the
2026-07-18 calibration separation argument is unchanged; v7 simply stops
shipping the gate dark.

Token efficiency: see `docs/token-efficiency-v7.md` — the reviewed aggregate
GPT-OSS manifest stays the identity anchor; v7 applies an interim ×2 budget
scale (sized from the measured v6 → v7-hard dataset growth) with a 3x deeper
penalty (`v7-relay-token-waste-p90-strict-v1`, max 30%).

## Operational envelopes for the ~10x datasets

Client-side only; no wire bytes change:

- per-case `/run` deadline: 120s → 5m for v7 (`DITTOBENCH_V7_CASE_TIMEOUT`).
- `/seed` deadline: ≥ 15m for v7 (`DITTOBENCH_V7_SEED_TIMEOUT`; a larger
  `DITTOBENCH_SEED_TIMEOUT` still wins).
- sandbox caps stay 3g/512m by default (frozen v2–v6 envelope) and are
  overridable via `DITTOBENCH_SANDBOX_MEMORY_LIMIT` /
  `DITTOBENCH_SANDBOX_TMPFS_LIMIT` for v7 rollout (suggested 6g / 2g).

## Backwards-compatibility evidence

- Wire: `TestRunCaseSendsBenchVersionOnlyForV7Plus` (runner) pins that v2–v6
  `RunRequest` bytes are unchanged; `TestSeedForVersionWireIdentity` pins that
  the v7 seed path marshals byte-identical `/seed` requests.
- Scoring: `TestScoreToolCaseObservedForVersionPreV7Identity`,
  `TestComposeResultUsageForVersionPreV7Identity`,
  `TestCapUnobservedForVersion`, `TestApplyForVersionPreV7Identity`
  (efficiency) pin that every versioned entry point delegates byte-identically
  for versions ≤ 6; the pre-existing v2 (`TestAggregateForVersionV2IsUngated`),
  v3/v4 gate, v5 ship-gate, and replay suites pass unchanged.
- The v5/v6 gate stack in `CompositeGateForVersion` is untouched for
  `benchVersion < 7`; v7 routes to `compositeGateV7`.

## Datagen v7-hard integration status (2026-07-23, integrated)

The hardened datagen suite (branch `v7-difficulty` of `dittobench-datagen`,
sibling worktree `../datagen-v7-hard`) went green and this repo is now built
against it via the `go.mod` replace directive marked
`// TEMPORARY: swap for tagged release before merge`. Integration findings:

- Full suite passes with the replace active; `gofmt`/`go vet` clean.
- Only allowed drift observed: the v7 known vector moved to
  `1aa1ad26d6b5285258128f5e4a222a150e6d3781411ffdc982f0e336f8e1ee94`
  (re-pinned in `cmd/tokenbaseline/main_test.go`). The v2/v3 vectors pinned by
  `TestBenchVersionDatasetVectors` and every v5/v6 identity test are
  unchanged.
- New v7 tool categories (`negation_no_tool`, `stale_context_web`,
  `link_chain_result_usage`, `job_chain_recovery_result_usage`) route
  correctly with no API change: `datagen.IsResultUsage` is suffix-based and
  `toolexec.Observable`/the fixture serving gate handle the dependent
  link-chain (search_web serves a per-case page URL; read_links reveals the
  needle only when called with it) inside the datagen module the validator
  already drives. New memory modules (deepchain, deepjoin, near-miss
  abstention, tempcalc, composedinj) grade through the public
  `dittobench-datagen/grade` path the scorer already uses. The MemorySuite
  telemetry counters are additive Go fields only.
- `internal/efficiency` remains internally consistent: the embedded reviewed
  manifest and `v7DatasetKnownVector` still pin the pre-hardening vector
  `1cfc6e3b…f58fcf`, so a manifest refreshed under the hardened datagen is
  correctly rejected by `ReadyForV7Production` until the real recalibration
  campaign produces a reviewed replacement (v7 scored rollout stays gated on
  that review, as designed).

Remaining pre-merge steps:

1. Tag the datagen `v7-difficulty` release and swap the replace for the
   tagged `require` version (`go mod edit -dropreplace …` + version bump).
2. Run the `cmd/tokenbaseline` 60-dataset recalibration campaign against the
   tagged release; embed the reviewed manifest; retire `V7TokenBudgetScale`
   (set to 1) and update `v7DatasetKnownVector`.

## Reviewer watch-items

- `V7TokenBudgetScale = 2` is an explicit interim constant, sized from the
  measured v6 → v7-hard dataset growth (see the constant's comment and
  `docs/token-efficiency-v7.md`). Before platform rollout of v7-hard, re-run
  `cmd/tokenbaseline` against the hardened datagen release and replace the
  scale with a genuinely recalibrated, reviewed manifest (then set the scale
  back to 1).
- The v7 per-case fixtures here measure the validator-side levers; the memory
  half of the ~10x target rides the datagen v7-hard suite and should be
  re-measured end-to-end once that module is tagged.
