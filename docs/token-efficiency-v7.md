# DittoBench v7 GPT-OSS calibration

DittoBench v7 changes the harness inference contract to
`openai/gpt-oss-20b`. Historical v5/v6 Qwen baselines are never reused.

The initial release calibrates one immutable logical OpenRouter route:
throughput sorting, healthy fallback, denied data collection, and ZDR. A scored
run uses that logical profile from start to finish. The actual upstream provider
is platform telemetry and never changes the calibration identity reported by
the trusted broker. GPT-OSS requires reasoning; the campaign observed
OpenRouter's default medium effort, and the platform proxy explicitly pins that
same behavior under the reviewed v1 profile.

For the aggregate profile:

1. Pin the starter-kit revision and v7 datagen revision.
2. Generate the canonical 60-dataset manifest: 20 seeds each for `small`,
   `medium`, and `full`.
3. Run the unmodified starter kit through the trusted ticket broker. Accept
   only complete provider-response telemetry with no unavailable usage or
   infrastructure failures.
4. Produce one nearest-rank p90 prompt/completion/total token baseline per run
   size. The profile becomes platform score-eligible only after the manifest is
   reviewed and embedded.

The current score-report schema does not carry a starter-kit revision, so a
report cannot self-attest which starter produced it. The
`-starter-kit-revision` value is therefore an external provenance assertion:
the tool requires a canonical lowercase 40-character Git SHA, and the reviewer
must verify that every input report came from the unmodified checkout at that
exact revision. The emitted manifest binds the reviewed assertion to every
baseline. A future report field can make this check mechanical.

The calibration remains bound to starter revision
`2ec9029568f20015562193a378eb8bce51191470`. The later starter PR head adds only
v7-scoped deterministic classification of the already-returned final text; it
does not change prompts, provider requests, model calls, or relay-observed token
usage. Keeping the measured revision here is intentional provenance, not an
assertion that the later source was used for these reports.

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
  -enable-scoring \
  reports/*.json
```

The audited candidate is versioned at `docs/baselines-v7-candidate.json` with
`scoring_enabled: false`. Its nearest-rank p90 totals are 66,475 tokens for
`small`, 414,008 for `medium`, and 936,353 for `full`. Candidate SHA-256:
`b6b8b9397164b14910ee707a91609f54a671af702bffb79735e7e27258750a58`.
The separately reviewable production copy is embedded at
`internal/efficiency/baselines_v7.json` with `scoring_enabled: true` and SHA-256
`8a5af55f04c4483224b4749dd0cf2151eb5b97b2446931f7b49cb55087e12cc5`.
It allows this binary to advertise the exact aggregate route and apply the v7
token transform. Platform rollout authority remains independent and inactive;
merging this calibration does not select v7 for any ticket.

OpenRouter endpoint discovery is not calibration. Provider-specific baselines
and adaptive policy selection remain dark follow-up work; they cannot make a
route score-eligible during the initial aggregate rollout.
