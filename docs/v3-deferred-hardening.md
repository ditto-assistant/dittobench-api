# DittoBench v3 deferred hardening

The independent v3 review raised three items that are correct and relevant but
sit outside the three anti-gaming PRs (datagen, api, screener). They are
recorded here with the reasoning for deferring them, so the decision is explicit
and the follow-up work is scoped.

## Offline score reproducibility (review finding 3)

The signed score report retains per-case scores, observed tool names, and
annotations. It does not retain the full graded `RunResponse`: `final_text`,
`answer`, `abstain`, and the complete observed `(name, args, hop)` trajectory.
A third party can re-run the public grader, but cannot replay memory grading or
tool-argument grading from the published artifact alone, because the graded
inputs are not published.

This is a real gap in end-to-end trustlessness. It is deferred because closing
it is a platform change, not a grader change. It requires:

- a canonical, content-addressed dataset and transcript artifact,
- that artifact's digest embedded in the signed score payload,
- publication of the artifact, or an availability commitment sufficient for
  third-party deterministic re-grading.

The grader itself is already deterministic and public. What is missing is the
transport and storage of the graded inputs, which lives in `ditto-platform`
(`validator.py` publish path) and the object store, not in this repository. It
lands as its own platform PR with its own retention and availability review.

Until then, the reproducibility guarantee is scoped: the scoring function is
public and deterministic, and anyone holding the dataset and transcript can
re-grade to the same numbers. The remaining work is making the dataset and
transcript themselves publicly retrievable and digest-bound.

## Endpoint reachability preflight (review finding 9)

On the scored path the validator serves a mock tool endpoint over
`host.docker.internal` and requires observed execution. If the endpoint is
advertised but unreachable from the harness network namespace at runtime (Docker
routing, network policy, or a runtime fault), the harness sees ordinary tool
errors, the validator observes nothing, and every observable tool case scores
zero. The run completes with a zeroed report instead of failing.

The empty-endpoint case is already fail-closed: a scored run with no advertised
`tool_endpoint` aborts rather than emit zeros (see the `toolEndpoint == ""`
guard in `cmd/dittobench-api/main.go`). The advertised-but-unreachable case is
not yet distinguishable from a legitimately tool-less harness using validator
state alone. Both produce zero observed calls. Failing the run on zero
observations would falsely fail, and endlessly reschedule, a harness that simply
never calls tools.

Distinguishing the two requires an active reachability probe that the harness
participates in, for example a validator-owned echo tool the harness is required
to hit during a preflight turn. That is a harness protocol addition, so it is
deferred to a protocol revision with a matching integration test (unreachable
endpoint routes to a failed run, not a completed zero report). This is
infrastructure correctness and fairness, not an exploitable gaming vector: it
can only cost an honest miner points, never inflate a score.

## Cross-repo wire-model telemetry sync (review finding 16)

The v3 protocol adds audit fields to the score report: `result_usage`,
`twin_group`, `confidence`, `observed`, and `injection`. The Pydantic wire
models in `ditto-platform` and `ditto-subnet` do not declare these fields, so
Pydantic silently discards them on ingest. The aggregate composite is
unaffected, because it does not depend on these fields, but downstream audit
context is lost.

This is additive and low-risk, and it touches two repositories outside the three
under review. It is deferred to keep the v3 anti-gaming release to one PR per
in-scope repository. The follow-up synchronizes both wire-model copies with the
`dittobench-datagen/protocol` source of truth and adds a cross-repo
serialization round-trip test so the three copies cannot drift again. Because
the fields are audit-only, sequencing this after the core release does not affect
scoring correctness.
