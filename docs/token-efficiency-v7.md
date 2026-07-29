# DittoBench v7 GPT-OSS calibration

DittoBench v7 changes the harness inference contract to
`openai/gpt-oss-20b`. Historical v5/v6 Qwen baselines are never reused.

The final v7 contract also replaces validator-local embeddings with the
ticket-bound OpenRouter profile
`dittobench-v7-openrouter-pplx-embed-v1-0.6b-768-v1`: model
`perplexity/pplx-embed-v1-0.6b`, Perplexity provider order, fallback disabled,
data collection denied, float encoding, and 768 dimensions. The scorer exposes
only that frozen embedding operation through the ticket-bound local broker.
Provider credentials and routing fields never cross that boundary.

The initial release calibrates one immutable logical OpenRouter route:
throughput sorting, healthy fallback, denied data collection, and ZDR. A scored
run uses that logical profile from start to finish. The actual upstream provider
is platform telemetry and never changes the calibration identity reported by
the trusted broker. GPT-OSS requires reasoning; the campaign observed
OpenRouter's default medium effort, and the platform proxy explicitly pins that
same behavior under the reviewed aggregate profile
`openrouter-route-a471cd87ae7df5b9-v1`.

For the aggregate profile:

1. Pin the starter-kit revision and v7 datagen revision.
2. Generate the canonical 60-dataset manifest: 20 seeds each for `small`,
   `medium`, and `full`.
3. Run the unmodified starter kit through the trusted ticket broker. Accept
   only complete provider-response telemetry with no unavailable usage or
   infrastructure failures.
4. Preserve one nearest-rank p90 raw prompt/completion/total reference per run
   size. Derive the scoring allowance at exactly 7,500 basis points: total is
   `floor(raw_total * 7500 / 10000)`, prompt is derived the same way, and
   completion receives the remainder so the allowance arithmetic is exact.
   The profile becomes platform score-eligible only after the disabled
   candidate is reviewed, explicitly enabled, embedded, and admitted.

The current score-report schema does not carry a starter-kit revision, so a
report cannot self-attest which starter produced it. The
`-starter-kit-revision` value is therefore an external provenance assertion:
the tool requires a canonical lowercase 40-character Git SHA, and the reviewer
must verify that every input report came from the unmodified checkout at that
exact revision. The emitted manifest binds the reviewed assertion to every
baseline. A future report field can make this check mechanical.

The completed hosted-embedding campaign binds starter revision
`62223b028acceb38ad0db98790402f1e2361dd18`. All 60 accepted runs have complete
provider telemetry, zero failed ledger requests, and zero fixture residue.
Rejected attempts are excluded. The public-safe per-run accounting and artifact
digests are recorded in `docs/v7-hosted-embedding-calibration-evidence.json`;
raw transcripts are intentionally excluded.

Generate the dataset contract without enabling scoring:

```sh
go run ./cmd/tokenbaseline \
  -bench-version 7 \
  -starter-kit-revision <40-character-git-revision> \
  -refresh-datasets
```

After all 60 trusted reports exist, build the review candidate:

```sh
go run ./cmd/tokenbaseline \
  -bench-version 7 \
  -starter-kit-revision <40-character-git-revision> \
  reports/*.json
```

The reviewed-but-disabled candidate is versioned identically at
`docs/baselines-v7-candidate.json` and
`internal/efficiency/baselines_v7.json`, both with SHA-256
`b8c11eff829c4c85b4b1af4f95135e4b5c26da01c8b88f72f726dca26d03d9a7` and
`scoring_enabled: false`. The three raw references and their separately derived
75% allowances are content-bound into each baseline ID. Readiness rejects
missing raw evidence, a multiplier other than 7,500, arithmetic drift, or a
non-derived allowance.

`scoring_enabled: false` is the PERMANENT v7 quality-only state, not a
disabled-awaiting-activation state (see the admission contract below). The
calibration tooling still validates a scoring-enabled copy of this manifest as
a completeness check (`efficiency.ReadyForV7Production`), but production
readiness flows through the quality-only predicate
(`efficiency.ReadyForV7QualityOnly`), which requires `scoring_enabled: false`.

OpenRouter endpoint discovery is not calibration. Provider-specific baselines
and adaptive policy selection remain follow-up work; under the quality-only
contract they are not needed to admit or score a route, because no absolute
token baseline enters v7 scoring at all.

## v7 admission contract: model + profile + accounting (no reviewed baseline)

The scoring layer's quality-only contract is reconciled with the ADMISSION
layer so a v7 run can actually score in a safe production config (SSRF guard on,
`DITTOBENCH_ALLOW_PRIVATE_HARNESS` unset):

- The relay identity gate (`requireTokenAccounting`) admits a v7 run on
  trusted metered accounting (accounting v2), the immutable provider identity,
  the locked harness model (`openai/gpt-oss-20b`), and a well-formed versioned
  route profile (`openrouter-route-<16 hex>-v1`). It does NOT require a reviewed
  token baseline for the route — under quality-only, usage never moves the
  composite, so no baseline is needed to score. Complete metered usage is still
  separately enforced (`requireCompleteV7Usage`).
- `efficiency.ProductionReadyForVersion(v7)` and the advertised v7 calibration
  readiness are true with the embedded `scoring_enabled: false` manifest,
  gated fail-closed on the locked model identity, the aggregate route +
  hosted-embedding profile, and dataset known-vector / calibration-digest
  verification.

## v7 activation: the normal platform benchmark rollout (no validator flag)

Activation is separated cleanly from readiness, and it uses the SAME mechanism
v2→v6 used — there is no bespoke validator env var:

- **Capability (validator-side):** the validator ADVERTISES v7 in
  `/v1/capabilities` (`SupportedBenchVersions`) iff it is technically ready
  (`efficiency.ReadyForV7QualityOnly`: correct locked model, aggregate
  route/embedding profile, dataset verification, quality-only contract). This
  is exactly how v5/v6 advertise on their reviewed manifests. Merging and
  deploying the api makes validators v7-CAPABLE, not v7-active. A stale or
  misconfigured validator simply fails the readiness gate and does not
  advertise v7 — that is the safety, no flag required.
- **Activation (platform-side):** whether v7 is actually dispatched and scored
  is decided entirely by the platform's benchmark rollout (the active bench
  version, controlled via backroom). The validator learns the bench version
  from the run request the platform sends (`bench_version` in the submit/score
  body) and scores whatever active bench the platform dispatches, provided it
  advertises support.
- **Rollback:** because the validator scores whatever bench the platform
  dispatches, setting the active bench back to 6 rolls v7 back with no validator
  code or config change — the same rollback path v2→v6 already have.

## v7 token contract: quality-only (usage recorded, never scored)

While the v7 difficulty suite is moving, repeatedly recalibrating an ABSOLUTE
token budget is the wrong contract. Bench_version 7 therefore removes the
token penalty from the composite entirely (`efficiency.ApplyForVersion`,
bench_version >= 7):

- The v7 composite is a pure function of answer/trajectory quality; token
  usage never moves it. Every v7 scored report carries a neutral
  `token_efficiency` record (`formula_version: "v7-quality-only-v1"`,
  `decision_reason: "v7_quality_only_contract"`, multiplier 1) as the in-band
  proof, alongside the full audited `details.token_usage` block and the
  broker accounting record (chat + embedding usage, request counts, route
  identity). Usage reporting stays first-class; scoring does not consume it.
- v5/v6 continue through the unmodified `Apply` path byte-for-byte (10% max
  penalty, one-sided rational curve past the reviewed p90 budget;
  `TestApplyForVersionPreV7Identity`).
- Efficiency incentives move to the platform layer as a bounded, epoch-frozen
  RELATIVE bonus among quality-qualified submissions:
  `docs/relative-efficiency-bonus-spec.md`.

Under this contract the embedded campaign manifest stays
`scoring_enabled: false` for v7 permanently: its raw p90 references
(71,309 / 450,074 / 995,198 small/medium/full) and 75% allowances
(53,481 / 337,555 / 746,398) are the canonical reviewed-measured REFERENCE
IDENTITY and audited evidence, not budgets awaiting activation. The successor
mechanism is the platform-side relative bonus
(`docs/relative-efficiency-bonus-spec.md`), and the campaign additionally
serves as the fit corpus for the calibration-transfer research tooling
(`docs/token-calibration-transfer.md`).
