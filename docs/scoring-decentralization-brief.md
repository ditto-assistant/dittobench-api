# DittoBench v2 Scoring: Decision and Plan

Status: DEPRECATED (2026-07-10), superseded by the one-validator-type model.
Kept for history only.

Superseding decision: there is no central scorer and no weights-only
validator. One validator type carries two duties: score when the platform
leases a ticket (distributed k=3, median), and set weights every interval
from the signed public ledger. The judge-free rework removed this brief's
premise: the generator, answer keys, and grader are public in
dittobench-datagen, so there is nothing secret to centralize around. Current
description: ditto-subnet docs/VALIDATOR-BRIEF.md and
docs/RUNNING-A-VALIDATOR.md.

Original text below, unedited.

---

Status: decided (2026-07-08)
Audience: subnet architecture, tokenomics/community, validator ops
Decision: independent weight-setting over centrally-computed scores

## Decision

Validators are independent in **weight-setting, not in scoring**. We run a
central scoring service that holds the oracle and computes a canonical score per
miner per epoch. Validators run an open, thin repo that reads those scores,
applies the eligibility gates, and sets weights on chain with their own hotkey.

This keeps the validator repo open and runnable (a hard requirement: without a
runnable repo, weights cannot be set) while the answer key never leaves the
closed scoring service. It also retires the entire per-validator enclave / TEE
workstream that a "validators score independently" model would have required.

## Architecture

- **Closed scoring service (dittobench-api, stays private).** Generates the
  seeded dataset, runs the miner's harness in a sandbox, and scores against the
  oracle. Produces a canonical score per miner per epoch and exposes it, signed,
  over an API. The oracle (answer keys, needles, judge rubric) never leaves this
  service.
- **Open thin validator (runnable by anyone).** Reads the canonical scores,
  verifies the signature, applies the eligibility gates, computes weights with a
  shared reference implementation, and submits weights on chain. It holds no
  secrets, so it can be fully open.
- **Consensus is trivial.** Every validator reads the same signed scores and runs
  the same weight computation, so honest validators converge. Yuma consensus and
  stake handle any operator who deviates.

The harness runs once centrally per epoch, not once per validator, so compute
cost is flat as validators are added.

### Where the work lands (current-state grounding)

- **dittobench-api (this repo) is already a stateless executor.** `POST
  /v1/submit` takes a miner tarball, generates the seeded dataset, runs the
  harness in a sandbox, and scores it; `GET /v1/runs/{id}` returns the report.
  Run state is an in-memory map, not persisted. This is already the right shape
  for the closed scoring service, so little changes here.
- **The scoring trigger currently lives in the validator's sweep.** That is the
  thing to move. A central orchestrator must decide which miner tarball to score
  and when, call dittobench-api, and persist the canonical result.
- **Persistence and the read API already live in platform-api (Postgres +
  leaderboard/ledger).** So the orchestrator, the canonical signed payload
  endpoint, the KMS signing, and the liveness handling land in platform-api, not
  in dittobench-api. The canonical payload is a signed, per-epoch superset of what
  the leaderboard already returns.
- **The open thin validator is a new repo** (extracted into the ditto-subnet
  side): it reads the signed payload, verifies, computes weights with the shared
  reference implementation, and sets weights on chain.

Net: dittobench-api stays roughly as-is; the new build is an orchestrator +
signed canonical endpoint in platform-api, plus a thin open validator.

### Public surface (already established)

The miner-side SDK is already public: `ditto-harness` (the Rust harness crate)
and `dittobench-starter-kit` (the baseline harness miners build from). The open
thin validator becomes the third public repo alongside them. The private boundary
stays exactly one thing: the scoring service (dittobench-api) with its oracle.
These public repos hold the tool catalog and interaction protocol, which miners
need, not the answer keys.

One hygiene item this surfaces: `GET /v1/dataset` returns the full `Dataset`
including `ToolCase.ExpectedTools` and `MemoryCase.ExpectedAnswer` (the answer
keys). The scoring path already strips these so only the prompt/question reaches
the harness, but the dataset endpoint does not. With the consumer now public and
the scorer going private, decide deliberately whether any publicly-reachable
practice path should expose answers, or strip the oracle fields from it. Tracked
as a task.

Public dataset sampler (`GET /v1/sample`): a full, real run-size
`DatasetArtifact` (the exact artifact validators score, via `gen.GenerateDataset`)
from a reserved public seed, so the community can inspect the benchmark over HTTP
without a Go toolchain. It is not redacted: with the generator public
(dittobench-datagen) the full dataset for any seed is derivable anyway, so hiding
answer keys here would add code without protecting anything. The one structural
guarantee it keeps is that it never accepts a caller seed. It derives the seed
from a reserved negative namespace (`?sample=0..9`), disjoint from every
non-negative per-submission seed, so a sample can never be a scored dataset nor be
aimed at a future submission's not-yet-drawn seed. No key required (generation is
deterministic and LLM-free).

## Why this over the alternatives

- It satisfies both hard constraints at once: open validator repo, and a private
  benchmark. The sensitive part stays behind an API boundary; the runnable part
  has nothing to hide.
- It drops a multi-month build. No sealed oracle distribution, no attestation in
  the weight path, no reproducible enclave builds, no per-validator confidential
  compute.
- It is forward-compatible. Scoring stays behind an API boundary either way, so
  if we ever need trustless scoring we can add sealed/attested local execution
  later without redoing the validator. See "Optional future" below.

## What this retires

The per-validator enclave plan (GCP Confidential Space, WIP/KMS sealed-secret
release, chain-seed-into-enclave, in-boundary harness execution) is shelved. It
was only required if validators had to compute scores without trusting our
service. They do not, so we are not building it.

## What we still need to get right

1. **Central scoring cadence.** Today the single validator's sweep triggers
   scoring. In the new model, scoring must be driven by the service itself (a
   scheduler or a designated orchestrator), so all validators read a canonical
   result rather than each triggering their own run. Without this we re-introduce
   the N-times-per-validator cost we just removed.
2. **Signed scores.** The service signs each epoch's canonical scores so any
   validator or observer can verify they came from us and were not altered in
   transit. This is the honest version of "trust the scoring API." Approach
   decided in "Score signing" below.
3. **Liveness.** Central scoring is now a hard dependency. If the API is down,
   validators have no fresh scores. Needs caching, serve-last-known, a clear
   staleness signal, and a defined validator policy for stale data.
4. **Where weights are computed.** The API returns canonical scores plus
   eligibility flags. The open validator computes weights from those with a shared
   reference implementation, so weight-setting stays independent while inputs and
   logic stay identical across validators. Decide against returning
   pre-computed weights, which would move policy server-side.

## Remaining considerations

- **Credibility posture.** This is "distributed weight-setting over
  centrally-computed scores," so validators trust our scoring. That is a normal
  subnet design. Signed scores and a public methodology are the mitigations. If
  delegators ever demand trustless scoring, the optional-future path is there.
- **Miner interaction model (resolved).** Miners submit a harness tarball we run
  in a sandbox. No live-endpoint path is planned, so the scoring service's
  execution path is settled.

## Optional future (not building now)

If trustless scoring is ever required, the closed scoring service can be packaged
to run locally on each validator as a sealed, attested binary (GCP Confidential
Space is the GCP-native route), with the oracle released only to the attested
image and scores signed by the enclave. Because scoring already sits behind an
API boundary, adopting this later does not require reworking the open validator.
It remains a documented option, not a committed plan.

## Score signing

Decided approach for signing the canonical score payload:

- **Dedicated Ed25519 key in GCP Cloud KMS** (fallback `EC_SIGN_P256_SHA256` if
  Ed25519 is unavailable in-region). The private key never leaves KMS; the service
  signs via the KMS API. HSM-backed, IAM-scoped, audit-logged. One signature per
  epoch, so latency and cost are irrelevant.
- **Do not reuse the owner Bittensor hotkey (sr25519).** Keep signing identity
  separate from the wallet that controls funds and weights.
- **Anchor trust to the on-chain identity once.** The owner hotkey signs a
  statement binding the KMS public key to the DittoBench scoring authority.
  Publish it.
- **Pin the public key out of band from the scores channel.** Bake it into the
  open validator repo (or commit the owner-hotkey attestation on chain).
  Validators never fetch-and-trust the pubkey over the same connection they fetch
  scores on, or a MITM defeats the signature.
- **Rotation:** KMS key versions, `key_id` carried in every payload, publish the
  new pubkey and attestation with an overlap window where validators accept both.
  Carry `key_id` now; build rotation machinery only when needed.
- **Why sign given TLS:** TLS secures the channel, the signature secures the
  score. It survives caching, mirrors, and the leaderboard, and it is
  non-repudiable, so a signed score cannot later be denied. For a benchmark where
  emissions ride on the number, that provenance is worth it.

## Canonical score payload

The wire contract between the closed scoring service and the open validator. The
service signs a SHA-256 over a canonical JSON serialization (fixed field order, no
insignificant whitespace; the exact canonicalization is published so validators
reproduce the bytes). Proposed shape, for team sign-off:

```json
{
  "schema_version": 1,
  "key_id": "<kms key version id>",
  "epoch_id": 12345,
  "seed": "<hex, chain-derived dataset seed for the epoch>",
  "dataset_sha256": "<hex, hash of the generated dataset>",
  "bench_version": 2,
  "generated_at": "<rfc3339 utc>",
  "scores": [
    {
      "miner_hotkey": "<ss58>",
      "tarball_hash": "<hex, which submission was scored>",
      "composite": 0.694,
      "composite_stderr": 0.037,
      "tool_mean": 0.608,
      "memory_mean": 0.783,
      "n": 114,
      "eligible": true,
      "per_category": [ { "category": "<slug>", "count": 3, "mean": 0.9 } ]
    }
  ],
  "sig": "<base64 signature over sha256(canonical json with sig omitted)>"
}
```

Notes:

- `sig` is computed over the payload with the `sig` field omitted, then attached.
- `eligible` echoes the reference gate (`n >= MIN_ELIGIBLE_CASES and composite >
  0`), but the open validator recomputes it from `n` and `composite` rather than
  trusting the flag.
- `tarball_hash` binds a score to the exact submission scored, so a resubmission
  gets a fresh score rather than inheriting an old one.
- `epoch_id` and `generated_at` let validators reject stale or replayed payloads
  (staleness policy in Phase 2/3).
- This is largely a signed, per-epoch superset of what the leaderboard API
  already returns, so the closed service can derive it from existing scoring
  output.

## Next steps

Phased so each step ships value and nothing is wasted.

### Phase 1: canonical central scoring (unblocks everything)
- Move the scoring trigger out of the validator and into the scoring service, so
  scores are computed once per epoch on a schedule and served to all validators.
- Define the canonical score payload (epoch id, seed, per-miner composite,
  per-category, eligibility fields, timestamp) and freeze the schema.
- Add signing per the "Score signing" section: a dedicated Cloud KMS Ed25519 key,
  pubkey pinned in the validator repo and attested by the owner hotkey.

### Phase 2: open thin validator
- Extract or build the open validator repo: fetch signed scores, verify
  signature, apply eligibility gates, compute weights with the reference
  implementation, submit weights on chain.
- Port the existing eligibility logic (n >= MIN_ELIGIBLE_CASES and composite > 0)
  into the shared reference weight computation so it lives with the validator, not
  only in the leaderboard query.
- Define and implement the staleness policy for a down or lagging scoring API.

### Phase 3: robustness and docs
- Caching and serve-last-known on the scoring API; staleness signaling.
- Validator operator guide: how to run it, what it trusts, how to verify score
  signatures.
- Public methodology note for delegators (what the score means, why the scoring
  is central, how to verify it).

## Glossary

- **Yuma consensus:** Bittensor's mechanism for combining validators' weight
  vectors into consensus, clipping stake-weighted outliers.
- **Oracle:** the benchmark's answer keys, needles, and grading rubric.
- **Canonical score:** the single per-miner-per-epoch score the central service
  computes and all validators read.
- **Eligibility gates:** the conditions a run must meet to earn weight
  (currently n >= MIN_ELIGIBLE_CASES and composite > 0).
