# Extraction: private `ditto-data-pipeline` (WP #46)

Split the generator (the secret oracle) out of `dittobench-api` into a private,
platform-only `ditto-data-pipeline` module, leaving `dittobench-api` as the
execution/scoring engine. **Both repos are private source** (corrected
2026-07-09). The distinction that drives the split: `dittobench-api`'s deployed
**API is public** (validators call it to score), so generation must NOT be
reachable via that public API, or a validator/miner could precompute answers. So
generation moves to `ditto-data-pipeline`, which is private *and* has no public
API (platform-only). Decision: the platform calls the pipeline to generate a
dataset per submission and ships it to the validator with the ticket; the
validator calls `dittobench-api`'s public scoring API to score it. See the
platform-side k=3 design. (`ditto-subnet`, the validator orchestration, goes
public only with explicit approval.)

## The seam (from the package dependency graph)

```
generation cluster (SECRET)          exec/scoring cluster (SHARED)
  internal/persona   (leaf)            internal/scorer   -> protocol
  internal/datagen   -> catalog,       internal/runner   -> netguard, protocol
                        toolexec,       internal/sandbox  -> astfp, netguard, protocol
                        protocol        internal/astfp    -> protocol
  internal/gen       -> datagen,        internal/toolexec -> protocol   (shared)
                        persona,        internal/netguard (leaf)
                        toolexec,        internal/store    -> protocol
                        protocol        internal/ratelimit
                                         internal/llm      (leaf, shared)
shared foundation (SHARED): pkg/protocol, internal/catalog, internal/toolexec,
internal/llm
```

Only `datagen` / `gen` / `persona` are sensitive: the distributions, templates,
and answer-key synthesis. The wire shapes (`protocol`), the category list
(`catalog`, already public in `COVERAGE.md`), the tool executor (`toolexec`),
and the LLM client (`llm`) are not the oracle and stay in `dittobench-api`.
So the private pipeline *imports* the `dittobench-api` module for those.

## Target layout

**`dittobench-api` (PRIVATE — exec/scoring + shared libs)**:
`pkg/protocol`, `internal/{catalog,toolexec,llm,scorer,runner,sandbox,astfp,netguard,store,ratelimit}`,
`cmd/dittobench-api` (now: provided-dataset + harness -> score, no generation),
`cmd/egress-proxy`.

**PRIVATE `ditto-data-pipeline`** (the generator):
`internal/{datagen,gen,persona}`, the generator dev tools
(`cmd/{calibrate,benchcal,refharness}`, `internal/refharness`), and a new
`cmd/generate` HTTP service (`POST /generate?seed=... -> dataset`) the platform
calls. Imports `github.com/ditto-assistant/dittobench-api` for the shared
packages.

## The cmd rewiring

`cmd/dittobench-api/main.go` today generates AND scores. It uses
`datagen.Generate`, `gen.{ProfileFor,Profile,NewRNG,GenerateTools,
GenerateMemorySuite,GenerateIsolation,ArtifactCase,FixtureDigest,
DatasetArtifact,StagedCase,FreshSeed}`, `datagen.IsResultUsage`.

- All of the above move to `ditto-data-pipeline`'s `cmd/generate` service.
- `POST /v1/submit` in the exec API drops generation; it accepts a
  **provided** dataset (from the platform ticket) + the harness tarball and
  returns a `ScoreReport` (keeps `scorer`, `runner`, `sandbox`, `store`).
- `GET /v1/dataset` (public practice) is served by the pipeline (it is
  generated data); the exec API either proxies it or drops it.

## Mechanics defaults (proceeding unless told otherwise)

1. Shared packages (`protocol`/`catalog`/`toolexec`/`llm`) stay in
   `dittobench-api`; the pipeline imports them via a local `replace
   github.com/ditto-assistant/dittobench-api => ../dittobench-api` until
   published. No third shared module.
2. Generation is an HTTP service (`cmd/generate`) the platform calls, mirroring
   how the platform already talks to services over HTTP.
3. The extraction is done **locally first** (`../ditto-data-pipeline`, no
   remote/push) for review before publication.

## Execution order (dittobench-api will not compile mid-split; do on a branch)

1. Scaffold `../ditto-data-pipeline` module (go.mod + `replace`).
2. Move `persona`, `datagen`, `gen` (and the generator dev tools) into it;
   fix imports to the `dittobench-api` module. Compile the pipeline.
3. Strip generation from `cmd/dittobench-api`: `/v1/submit` takes a provided
   dataset; remove the `gen`/`datagen` imports. Compile the exec API.
4. Add `cmd/generate` to the pipeline (POST /generate). Compile + smoke test.
5. Platform: call the pipeline generate service in `POST /validator/job`
   (ticket issue), ship the dataset with the ticket (WP #44).
