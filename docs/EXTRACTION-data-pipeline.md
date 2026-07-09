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

## RESOLVED (2026-07-09): generation is now non-LLM + deterministic

The "generation is LLM-based" blocker below is **dissolved**. The datagen pivot
made the whole generation path non-LLM and byte-reproducible from
(seed, bench_version): `GenerateTools` uses datagen template variants, memory
questions use seeded phrasing variants (`persona.askVariant`), and the LLM
paraphrase pass was removed entirely (paraphrase.go / question_gap.go deleted).
So `cmd/generate` now runs the REAL generator: it calls the exported
`gen.GenerateDataset(seed, prof)` and returns the canonical `DatasetArtifact`
(tool cases + staged memory waves + memory cases + fixture digests). Same seed →
same `dataset_sha256` (smoke-verified). No OpenRouter client needed. The run path
(`runSizeJob`) and the generate service share `gen.BuildArtifact`, so their bytes
can't drift.

**Staged waves are not a blocker either:** the artifact carries `MemoryWaves`
(the staged Tier-C `SeedRequest`s) and each memory case's `RunAfterWave`, so the
consuming run phase can drive the exact wave staging from the provided artifact.

**Remaining for #46 (exec side):** rewire dittobench-api's `/v1/submit` to accept
a PROVIDED `DatasetArtifact` (from the platform ticket) and drive its run/score
loop from it, instead of generating. Keep `scorer`/`runner`/`sandbox`. Design
fork to settle: keep the miner practice path (`/v1/dataset` + generate-and-score)
alongside the new score-provided path, or split them.

## CRITICAL FINDING (2026-07-09, now RESOLVED above): generation was LLM-based + staged

Reading `runSizeJob` (the real full-benchmark path) surfaced two things that make
the "generate a static dataset, ship it, score it" model non-trivial:

1. **There are two generators.** `datagen.Generate(seed,n)` (used by the practice
   `/v1/dataset` + the simple `evaluate()` path) is a pure-ish function of the
   seed. The **full-benchmark** generator is the `gen.*` profile pipeline in
   `runSizeJob` (`gen.GenerateTools` + `gen.GenerateMemorySuite` +
   `gen.GenerateIsolation`), and it is **LLM-based** (paraphrase + procedural
   persona synthesis via `genModel`), so it needs an OpenRouter client, not just
   a seed. The `cmd/generate` scaffold currently calls the SIMPLE `datagen.Generate`
   — a placeholder; the platform's real generate service must run the `gen.*`
   pipeline with an LLM client.
2. **The memory suite is STAGED, not a static blob.** The suite lays cases across
   seeding tiers (A prepared, B raw-pairs) and staged Tier-C waves; the run phase
   interleaves seeding the harness (across waves) with querying it. So "hand the
   validator one dataset and score once" does not cleanly hold for memory — the
   generation and the run are coupled through the staged waves.

**Design question this raises (needs a decision before the rewire):** does the
platform ship the full pre-rendered artifact (`gen.DatasetArtifact` already hashes
tool cases + memory waves + isolation + fixtures — a rendered, static form the
validator replays), and dittobench-api's run phase consume that instead of
generating? That is the natural seam (`DatasetArtifact` is designed for exactly
this "pin exact bytes for a dispute re-score"), but the run loop's wave staging
must be driven from the provided artifact rather than the live `gen.*` output.
This is the crux of the dittobench-api rewire and wants a focused design pass.

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

1. Shared packages stay in `dittobench-api`; the pipeline imports them via a
   local `replace github.com/ditto-assistant/dittobench-api => ../dittobench-api`
   until published. No third shared module. **Prerequisite (found on the first
   compile, 2026-07-09):** Go forbids importing another module's `internal/`
   packages, so the shared packages the generator needs must first be promoted
   out of `internal/`. The generator (`datagen`/`gen`/`persona`) needs
   `catalog` + `toolexec` (it uses `protocol`, already in `pkg/`, and does NOT
   import `llm`). So move `internal/catalog -> pkg/catalog` and
   `internal/toolexec -> pkg/toolexec` and update all importers before the move.
2. Generation is an HTTP service (`cmd/generate`) the platform calls, mirroring
   how the platform already talks to services over HTTP.
3. The extraction is done **locally first** (`../ditto-data-pipeline`, no
   remote/push) for review before publication.

## Execution order (dittobench-api will not compile mid-split; do on a branch)

0. **Prerequisite:** promote `internal/catalog -> pkg/catalog` and
   `internal/toolexec -> pkg/toolexec` in dittobench-api (Go blocks a foreign
   module from importing `internal/`); update every importer. This is a
   self-contained, non-breaking refactor dittobench-api can land on its own.
1. Scaffold `../ditto-data-pipeline` module (go.mod + `replace`). [done]
2. Move `persona`, `datagen`, `gen` (and the generator dev tools) into it;
   `gen`'s imports of `datagen`/`persona` repoint to the pipeline module, its
   `catalog`/`toolexec` imports repoint to the new `pkg/` paths. Compile the
   pipeline.
3. Strip generation from `cmd/dittobench-api`: `/v1/submit` takes a provided
   dataset; remove the `gen`/`datagen` imports. Compile the exec API.
4. Add `cmd/generate` to the pipeline (POST /generate). Compile + smoke test.
5. Platform: call the pipeline generate service in `POST /validator/job`
   (ticket issue), ship the dataset with the ticket (WP #44).
