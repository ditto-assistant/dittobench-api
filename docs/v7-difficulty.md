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
GPT-OSS manifest stays the identity anchor; v7 applies an interim ×4 budget
scale with a 3x deeper penalty (`v7-relay-token-waste-p90-strict-v1`,
max 30%).

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

## Reviewer watch-items

- `V7TokenBudgetScale = 4` is an explicit interim constant. Before platform
  rollout of v7-hard, re-run `cmd/tokenbaseline` against the hardened datagen
  release and replace the scale with a genuinely recalibrated, reviewed
  manifest (then set the scale back to 1).
- The v7 per-case fixtures here measure the validator-side levers; the memory
  half of the ~10x target rides the datagen v7-hard suite and should be
  re-measured end-to-end once that module is tagged.
