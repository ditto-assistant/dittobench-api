# DittoBench v7 GPT-OSS calibration

DittoBench v7 changes the harness inference contract to
`openai/gpt-oss-20b`. Historical v5/v6 Qwen baselines are never reused.

The final v7 contract also replaces validator-local embeddings with the
ticket-bound OpenRouter profile
`dittobench-v7-openrouter-pplx-embed-v1-0.6b-768-v1`: model
`perplexity/pplx-embed-v1-0.6b`, Perplexity provider order, fallback disabled,
data collection denied, float encoding, and 768 dimensions. The scorer still
exposes only the frozen Ollama-compatible `embeddinggemma` operation to the
harness. Provider credentials and routing fields never cross that boundary.
Historical v2-v6 runs retain the local EmbeddingGemma path.

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
non-derived allowance. Infra must eventually pin the separately reviewed,
enabled production digest; the pin remains empty at this stage. Merging this
candidate still does not select benchmark v7 or admit the route.

OpenRouter endpoint discovery is not calibration. Provider-specific baselines
and adaptive policy selection remain dark follow-up work; they cannot make a
route score-eligible during the initial aggregate rollout.
