# Seeds and scoring: how a submission is graded and how to verify it

This explains how DittoBench picks the dataset a submission is scored on, how the
champion comparison is seeded, and how a miner can reproduce and audit any score.
It also records the plan for surfacing this through the platform API and UI so
miners get the full picture without reading validator code.

The companion `v3-deferred-hardening.md` covers the two hardening steps this
design still wants (a fixed block offset and published transcripts).

## The seed is committed before it exists, not hidden then revealed

There is no window where a seed is secret and later released. The dataset seed is
derived from an on-chain block hash that is produced after the miner has already
committed the submission (upload, payment, screening pass). The harness is frozen
before that block exists, so the miner cannot precompute or overfit the dataset.
Because the ordering is what protects the benchmark, the seed can be fully public
the moment it is derived.

Per-submission derivation (`ditto/api_server/onchain_seed.py`, mirrored in the
validator at `ditto/validator/onchain_seed.py`):

    seed = int(SHA-256(normalized_block_hash || ":" || agent_id)[:8]) & (2**63-1)

Two properties follow:

- Unpredictable. The block postdates the commit, so the seed was unknowable at
  submission time.
- Per-agent. Binding `agent_id` into the hash means two submissions pinned at the
  same block still get different datasets, so a leaked dataset for one agent is
  useless to another. There is no shared answer key.

Validators do not trust the platform's seed. Each validator re-derives it with
its own copy of the formula and refuses any ticket whose seed does not match its
pinned block (`seed_matches`). A platform grinding seeds to favor an agent is
caught by every honest validator.

## The champion comparison uses common random numbers

Routine scoring uses the per-agent seed above. The decision that moves emissions,
whether a challenger dethrones the champion, uses common random numbers so the
two are compared on the same dataset (`ditto/validator/crn.py`):

    crn_seed = SHA-256(sorted(agent_ids) || bench_version || k) & (2**63-1)

Scoring the champion and challenger on an identical dataset collapses the
variance of their score difference, which is the only quantity KOTH cares about.
A dethrone runs `KOTH_CONFIRMATION_SEEDS = 3` common seeds (k = 0..2) and takes
each agent's median composite over them, so a crown flip has to replicate across
seeds instead of riding one lucky draw. The CRN seed is a pure function of the
compared set and the version, so every validator computes the same value and
Yuma consensus holds.

This is where per-run seed variance is removed. It is removed from the
comparison, not by forcing every miner onto one global dataset. A single global
seed would reopen the shared answer-key channel for no added fairness.

## Cadence: scoring is continuous, weights are hourly

Two cadences are deliberately separate (`ditto/validator/config.py`):

- `sweep_seconds = 120`. The scoring queue drains every two minutes, so a new
  admitted submission is picked up within about one sweep of its seed being
  pinned.
- `epoch_seconds = 3600`. On-chain weights are pushed at most hourly (the chain's
  `weights_rate_limit` can stretch this, never shorten it).

Because scoring is decoupled from the weight epoch, no one waits an epoch for a
first score. The champion keeps earning `KOTH_CHAMPION_SHARE = 0.9` and the tail
of four splits the rest while a challenger is evaluated, and only 20 percent of
miner emission is released at all (`MINER_EMISSION_SHARE = 0.2`, the rest
burned). The dominant first-score latency is screening plus build and run time,
not anything about seeds.

## Verify any score yourself

The generator is public and deterministic, and it is open precisely so miners can
reproduce scores rather than trust validators (`dittobench-datagen`). A given
`(seed, bench_version)` always produces the identical dataset, byte for byte.

1. Read the submission's scoring record (see the API section below). It carries
   the `dataset_seed`, the `dataset_sha256`, the `dataset_seed_block` and its
   hash, and each validator's signed composite.
2. Confirm the seed was not platform-chosen. Fetch the block by number, take its
   hash, and recompute `derive_seed(block_hash, agent_id)`. It must equal the
   published seed.
3. Regenerate the dataset and confirm the digest:

       generate -seed <dataset_seed> -run-size <run_size> -sha

   The printed `dataset_sha256` must equal the published one. That proves all
   validators scored the exact bytes the platform pinned.
4. Re-grade a transcript with the public `grade/` package to reproduce the
   composite. Full offline re-grading of an arbitrary run depends on transcript
   publishing, which is finding 3 in `v3-deferred-hardening.md`.

## What the platform exposes today

Per-submission transparency is already wired through the public API
(`ditto-platform/ditto/api_models/public.py`).

- `PublicSubmissionScores` at `/public/agent/{id}/scores` publishes, per agent:
  the finalized `median_composite`, the dataset pin (`dataset_seed`,
  `dataset_sha256`, `dataset_run_size`), the seed's on-chain origin
  (`dataset_seed_block`, `dataset_seed_block_hash`) with verification
  instructions in the field docs, and the full list of per-validator scores.
- `PublicValidatorScore` publishes each of the k=3 validators' `validator_hotkey`,
  exact numbers, the `seed` it scored on, its sr25519 `signature`, and a redacted
  per-case breakdown (category, kind, score, pass, latency, notes, never the
  answer key).
- `PublicLeaderboardEntry` at the leaderboard stays aggregate: composite, tool
  and memory means, models, `bench_version`, `dataset_sha256`, and a per-category
  breakdown. It omits the raw seed by design and links to the full record by
  `agent_id`. The seed is one drill-down away at `/public/agent/{id}/scores`,
  which is safe to publish for a finalized score because the generator is public,
  the harness is frozen, and each agent's seed is independent.

The gap is the KOTH layer. Nothing public yet shows which agent is the incumbent
champion or how a dethrone comparison resolved. That is the transparency a miner
most needs to understand why they did or did not take the crown.

## Plan: champion and dethrone transparency

### API

1. Mark the incumbent. Add a champion flag (and reign start) to the leaderboard
   response so the KOTH incumbent is identified, not just ranked first by
   composite.
2. Publish the dethrone comparison record. Add an endpoint, for example
   `/public/agent/{id}/dethrone` or a comparison block on the scores record, that
   exposes for each champion-versus-challenger evaluation:
   - the `bench_version` and the `KOTH_CONFIRMATION_SEEDS` CRN seeds used,
   - each agent's per-seed composites (`confirmation_composites`) and the median
     the fold used,
   - the dethrone band actually applied, both the flat relative margin
     (`KOTH_MARGIN`) and the statistical term (`KOTH_DETHRONE_Z` times the
     combined standard error), and which one bound,
   - the decision and, if it held, the exact shortfall.
   Every field here is derivable from public constants and regenerable datasets,
   so it adds no answer-key surface. Each CRN seed regenerates through the same
   `generate -seed` path as a normal run.
3. Expose the champion's own provenance prominently. The champion's
   `PublicSubmissionScores` already carries the seed, block, digest, and signed
   k=3 rows. Surface a direct link from the leaderboard champion to that record.
4. Keep signatures on every published number so a miner can check each validator
   actually reported what the platform folded.

### UI

1. A scoring panel on each submission: the composite and its standard error, the
   dataset seed with a one-click link to the block explorer and the derive-seed
   check, the dataset digest, and the k=3 validator rows with hotkeys and
   signature status.
2. A champion card: who holds the crown, since when, the champion's seed, block,
   and digest, and its median composite over the k=3 record.
3. A head-to-head view for the most recent dethrone attempt against the champion:
   the shared CRN seeds, each side's per-seed composites and median, the band,
   and the outcome, so a challenger can see exactly why the crown moved or held.
4. A short "verify this yourself" block that reproduces the four steps above with
   the submission's real seed and digest prefilled, pointing at the public
   generator and grader.

### Sequencing

The per-submission verification path already works, so the UI can surface the
existing `PublicSubmissionScores` fields immediately. The dethrone record is the
one new API artifact and should land first, since the head-to-head is the part
miners cannot currently see. Full offline re-grading of a run still depends on
transcript publishing (finding 3), so the "re-grade a transcript" step stays
partial until that ships.

## On hiding versus publishing the seed

The leaderboard's historical note about excluding the seed as anti-overfit is
superseded by the public-generator design. Hiding a specific seed buys nothing:
anyone can already generate unlimited `(seed, dataset)` pairs from the open
generator, and each agent's seed is an independent commit-reveal draw, so a
published seed cannot predict another agent's dataset or let a frozen harness
re-overfit. Publishing the seed for a finalized score is therefore safe and is
the intended path, which is why `PublicValidatorScore` and
`PublicSubmissionScores` already carry it. The aggregate leaderboard stays lean
for readability, not for secrecy.
