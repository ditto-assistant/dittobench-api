# Token-calibration transfer (research tooling)

Status: RESEARCH / LATER-USE. Nothing in v7 scoring consumes this — the v7
composite is quality-only (`docs/relative-efficiency-bonus-spec.md`), and the
v7 readiness gate accepts only reviewed-measured manifests (derived manifests
are rejected fail-closed, `efficiency.ReadyForV7Production`). This tooling
exists so that when an ABSOLUTE token budget or hard abuse ceiling is wanted
again (once difficulty stabilizes), its manifest can be DERIVED from an
existing reviewed campaign plus a short smoke pass instead of a fresh 60-run
live calibration (~hours; the full-size runs alone are ~11 minutes each).

Context on the measured curve this would feed: the shipped absolute
transform's maximum penalty is 10% (one-sided rational curve past the p90
budget; `internal/efficiency.Apply`); with the curve's allowance,
starter-kit-level usage sees on the order of a 2.5% effect. A derived budget
therefore needs single-digit-percent accuracy at the p90 point to be useful —
which is what the numbers below measure.

## The model

Fit corpus: the COMPLETE 2026-07 v7 aggregate calibration campaign — all
60/60 clean runs of the unmodified starter kit (20 per run size; failed and
rejected attempts excluded; every run's `dataset_sha256` reproduced offline
from (seed, run_size, bench_version) before its features entered the fit).
This is the same corpus whose reviewed-measured manifest merged in PR #91.
Identity: `openai/gpt-oss-20b` via `openrouter-route-a471cd87ae7df5b9-v1`,
embeddings `dittobench-v7-openrouter-pplx-embed-v1-0.6b-768-v1`. Corpus
digest (sorted `size:seed:sha:prompt:compl:total` lines):
`bcd7873b07b3…c55503a8`. Model version:
`tokenmodel-gptoss-v7corpus60-2026-07-24-v2`.

Structure (ordinary least squares through the origin, pooled across sizes;
`internal/tokenmodel`):

    chat_prompt      ≈ 3875.1·tool_cases + 3507.5·memory_cases + 991.9·expected_hops
    chat_completion  ≈  387.3·tool_cases +  293.8·memory_cases
    embedding_prompt ≈ 0.0958·haystack_bytes + 15.36·pairs + 15.17·subjects + 42.40·memory_cases   (advisory)

Every feature is computable offline from the deterministic dataset artifact
(`tokenmodel.Extract`): case counts, expected observed-execution hops
(+1 forced retry on AllowExtraTools cases), rendered prompt bytes, haystack
size. No tokenizer dependency: per-case/per-hop coefficients absorb the
template + tool-catalog rendering at the fitted bytes→tokens behavior.

## Fit quality — measured, not assumed

The reviewer point stands and the numbers CONFIRM it: prompt usage is not
purely dataset-deterministic (retrieved context, actual trajectories, and
conversation state add behavioral variance). The fit captures the
dataset-driven component; the residuals below are the honest behavioral
remainder.

Per-run |error| of the chat TOTAL (full 60-run corpus):

| run_size | mean | p90 | max |
| --- | --- | --- | --- |
| small | 7.8% | 17.1% | 27.2% |
| medium | 5.2% | 10.2% | 10.9% |
| full | 4.2% | 7.1% | 8.6% |

Held-out END-METRIC accuracy (the quantity that matters: derive the p90
budget from predictions anchored on one half of the seeds, compare to the
actual measured p90 of the OTHER half; both parities):

| run_size | mean error | max error |
| --- | --- | --- |
| small | 0.9% | 0.9% |
| medium | 6.4% | 6.6% |
| full | 2.9% | 2.9% |

Validation against the PUBLISHED campaign raw p90s (the merged PR #91
manifest: 71,309 / 450,074 / 995,198 small/medium/full): the corpus
reproduces them exactly (it is the campaign), and the UNANCHORED model
prediction at the predicted-p90 rank lands at −10.6% / −8.8% / −5.8%
respectively — the systematic under-prediction the per-size anchors absorb
by construction. The anchored derivation therefore reproduces the published
p90s on the fit corpus identically; its out-of-sample error is the held-out
row above.

Embedding-token model residuals: ≤ 4.4% max (advisory only; embedding usage
never enters a chat-token manifest).

Achieved tolerance: the anchored transfer derives p90 budgets within ~7% of
a fresh 20-runs-per-size measurement. Against a 10%-max-penalty curve, a ±7%
budget error moves a starter-level run's multiplier by well under one point —
defensible for a practice/abuse-ceiling budget, and the smoke gate below
bounds the failure mode where the world drifted.

## Validity preconditions (checked in code, not a footnote)

`DeriveManifest` REFUSES (error says "run a full calibration") unless all of:

1. the source manifest is schema-2, scoring-enabled, reviewed-measured (no
   derivation provenance — no chaining), and every source baseline is on the
   exact fitted identity (`openai/gpt-oss-20b` @
   `openrouter-route-a471cd87ae7df5b9-v1`);
2. the target bench version locks the SAME harness model
   (`llm.HarnessModelForVersion`);
3. every regenerated target dataset lies inside the model's interpolation
   ENVELOPE — per-run-size dataset-composition strata (expected hops per tool
   case, prompt bytes per case, haystack bytes per memory case, pairs per
   memory case) within the fit corpus's observed range (±~50%).

A relay model change, tokenizer change, embedding-profile change, routing
contract change, tool-schema overhaul, or harness-template overhaul
invalidates the fit and REQUIRES a full calibration. The first two are caught
by (1)/(2); template/tool-schema drift is what the smoke gate exists to
catch; dataset-shape drift is caught by (3).

Live demonstration of (3): the hardened v7 suite's small-size hop density
(2.0 expected hops per tool case, dependent link chains) is ~2x the fit
corpus's maximum (0.95), so deriving a v7-hard manifest from the v7-old fit
is correctly REFUSED — that recalibration genuinely needs measurement
(pinned by `TestProductionEnvelopeRefusesHardenedSuite`).

## Smoke validation (fail-closed)

`SmokeValidate` takes the derived manifest plus K live runs — recommended
K=3: one small + one medium + one full (~15–20 min) — and accepts only if:

- every run size has an accepted run (per-size coverage; a p90 tail cannot
  be sanity-checked from another size's run);
- each report is a complete proxy-metered run of a dataset PINNED in the
  derived calibration grid (seed and `dataset_sha256` must match — proves the
  smoke ran the exact target datasets on the exact datagen revision) on the
  exact route/model identity;
- each measured chat total is within the acceptance band of its per-seed
  anchored prediction: tolerance 40% (small) / 16% (medium) / 13% (full),
  SHRUNK by a 20% border zone — a result between acceptance and tolerance is
  INCONCLUSIVE and rejected. Bands sit above the fit's observed residual
  maxima (27.1/10.9/8.5%) so an honest world passes, while the drift modes
  smoke exists to catch (template overhaul, silent model/tokenizer change)
  shift totals far beyond them.

Three runs reject gross drift; they cannot certify a p90 tail. That is why
the per-category strata live at DERIVE time (the envelope check runs on all
60 target datasets), why the border zone rejects near-boundary results, and
why a derived manifest — even smoke-passed — stays `scoring_enabled: false`
and provenance-tagged until an explicit, platform-level policy decision. Any
rejection routes to the full campaign.

## Provenance classes

- `reviewed-measured` — a live 60-run campaign, reviewed and embedded. The
  in-flight 2026-07 v7 baseline (PR #91) is and stays this class. The ONLY
  class any validator gate accepts today.
- `derived-smoke-validated` — produced by this tooling, smoke passed.
  Reserved for later revisions; accepting it anywhere is a platform policy
  decision, not a validator default.
- `derived-unvalidated` — derived, no smoke pass. Never acceptable.

`efficiency.ManifestProvenance` classifies; `ReadyForV7Production` rejects
any manifest with derivation provenance; derived baselines carry
`"aggregation": "nearest_rank_p90_derived"` so they can never satisfy a
measured-only validity check even if mislabeled.

## Commands for the next bench-version roll

Once an absolute budget is wanted again and the preconditions hold:

    # 1. Derive the target manifest offline (seconds):
    go run ./cmd/tokenbaseline \
      -transfer-from reviewed-source-manifest.json \
      -bench-version <target> > derived-manifest.json

    # 2. Run K=3 smoke runs (one per run size) against pinned seeds from
    #    derived-manifest.json's calibration grid, then:
    go run ./cmd/tokenbaseline \
      -smoke derived-manifest.json \
      small-report.json medium-report.json full-report.json \
      > derived-manifest.smoke.json

    # 3. Review; platform policy decides whether the derived+smoke-validated
    #    class is acceptable for the rollout, or a full campaign is run.

If either command refuses, run the full calibration exactly as today
(`-refresh-datasets`, 60 live runs, `-enable-scoring`).

## Refitting after the next measured campaign

The fit itself is reproducible: corpus features come from
`tokenmodel.Extract` over regenerated datasets (hash-verified against each
report's `dataset_sha256`), targets from `details.token_usage`; exclude any
run whose usage is not `status == "complete"` and any `*-failed-*` /
`*-rejected-*` attempt; OLS through the origin; anchors = measured/predicted
at the nearest-rank-p90 run per size; freeze everything as a new
`tokenmodel.Model` with a new Version and the new corpus digest. Raw
transcripts stay out of committed artifacts — only aggregates and digests
land in the model.
