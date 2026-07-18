# Complete v3: reproduce-under-transform audit + task-side hardening rollup

Status: PLAN ONLY. Nothing here is implemented. This is a handoff spec for the
agent who will fold the deferred v3.1 items into the existing v3 PR train so v3
ships "complete" rather than shipping now and following up later.

The existing v3 PRs (do not create new branches; add to these):
- dittobench-datagen `harden/anti-gaming` (PR #2)
- dittobench-api `harden/anti-gaming` (PR #28)
- ditto-subnet `harden/v3-deferred-hardening` (PR #155)
- ditto-platform `harden/v3-deferred-hardening` (PR #186)
- dittobench-starter-kit `feat/v3-preflight` (PR #14)

Read first, in this order: `dittobench-datagen/docs/anti-gaming.md` (the design
invariants and the on-chain spec this plan builds), this repo's
`docs/dittobench-v2-vs-v3.md` (what v3 does and does not claim), and the
attack-vector taxonomy the pre-merge review produced.

## The two invariants (do not break either)

1. **Trustless reproducible scoring.** A score and any audit verdict must be a
   pure, public, deterministic function of `(dataset, transcript)`. No
   validator-held secret, no LLM in the score/audit path, no nondeterminism.
   Any validator or third party must re-derive the same verdict byte-for-byte.
2. **Simple validator.** The validator generates from the seed with public
   code, runs the harness, grades deterministically, signs. Anything that
   cannot be a deterministic score stays in the screener or offline `gstudy`,
   never in the composite or the audit verdict.

Every mechanism below is checked against these. If a step needs a secret or a
model in the score path, it is wrong.

## Honest scope: what reproduce-under-transform does and does not defeat

State this plainly in every doc and comment you write, so we do not re-introduce
the over-claiming the pre-merge review just corrected.

- **Defeats** surface-brittle strategies: answers keyed to a question's exact
  surface form. That is the bulk of the rejected-harness corpus: template
  fingerprint dispatchers, trailing-question strippers, lookup keyed to the
  original phrasing, and precompute-and-inject where the precompute was keyed to
  the original question. Under an unpredictable post-commit rephrasing these
  produce a wrong or inconsistent answer while a genuine reasoner (or a robust
  solver) does not.
- **Does NOT defeat** a robust local solver that recomputes the answer from the
  cleartext haystack per question. It recomputes correctly under the transform
  too. Making a cleartext-haystack answer model-required is not achievable in a
  deterministic score. That residual is the screener behavioral oracle's job
  (force model invocation; shipped, v0.11.2) plus the per-submission fee. The
  transform audit and the oracle are complementary: the oracle forces a model on
  some traffic, the transform audit makes memorization and surface-brittle
  precompute negative-EV, and the audit cases are indistinguishable from normal
  cases so a harness cannot selectively reason only where it is watched.

Do not describe the transform audit as closing the general timeline-solver gap.
It closes the memorization/brittleness family and raises the cost of the rest.

## Part A: reproduce-under-transform audit

The mechanism is a **post-commit, unpredictable, per-case metamorphic audit**
that extends machinery already in the tree rather than building greenfield:
- datagen already emits metamorphic twin families and a `TwinGroup` tag, and
  point-in-time / multi-hop variants.
- the scorer already computes `MetamorphicConsistency` and folds a
  `MetamorphicConsistencyFactor` (`internal/scorer/scorer.go:388,436`).
- the platform already has signed-score ingest, an immutable audit chain
  (`append_audit_entry`), and a review-hold status (`ATH_PENDING_REVIEW`).

Today's `MetamorphicConsistency` uses **generator-chosen** twins fixed in the
dataset, so a harness can pre-handle the specific twin. The audit upgrades this
to **validator-derived, post-commit, block-hash-seeded** transforms the harness
cannot see coming, and adds an on-chain penalty for the brittleness signature.

### A1. datagen: transform functions and deterministic transformed answers

New package or file, e.g. `persona/transform.go` (or extend the twin machinery):

- `Transform(caseID string, seed int64, transformID int) (question string, expectedAnswer string, class TransformClass)`.
  Pure function of `(seed, caseID, transformID)`. Two classes:
  - `Invariance` (rephrase, reorder the surrounding context, synonym swap,
    passive/active voice): the expected answer is **unchanged** from the base
    case. Reuse the paraphrase/`varySurface` machinery already in `surface.go`.
    A surface-keyed harness answers the base but misses the transform.
  - `Covariance` (point-in-time date shift, unit change, entity/subject swap):
    the expected answer **changes deterministically** and is recomputed by the
    same generator code that produced the base answer (point-in-time and
    duration families already do this in `persona/questions.go`). A memorized
    answer returns the old value and is wrong.
- The transform space must be large and per-seed varied (do not ship a small
  fixed pool; that regresses to the enumerable-lexicon problem the review
  flagged). Compose transforms from parts keyed to `(seed, caseID, transformID)`.
- Every transform's expected answer must be produced by the existing
  deterministic grader path, so a third party regenerates it. No new answer
  source.

Add a red-team gate in `gen/redteam_test.go`:
`TestRedTeamSurfaceBrittleParserFailsTransform` — a parser that answers the base
case from a fixed surface template must score below full on the invariance
transform across a seed sweep, while the honest reference recovers both.

### A2. api scorer: grade audited cases, emit a transform-robustness metric

- The audited cases are ordinary graded cases whose question is the transformed
  question and whose expected answer is the transformed expected. They flow
  through the existing grade path and land in `per_case` with their own
  `CaseScore`. **Indistinguishable from a normal case on the wire** (no flag the
  harness can read).
- Add a per-run **transform robustness** metric to `RunDetails`: over cases that
  carry an audit twin, the agreement rate between the base-family score and the
  transform score for the same underlying fact. This generalizes
  `MetamorphicConsistency`; reuse `TwinGroup` to pair base and transform.
- Do NOT invent a new grade rule. The audit case scores by the same
  deterministic grader; robustness is a derived aggregate.
- Emit `transform_robustness` (0..1) and `audit_case_count` in the report
  details, and the per-case audit membership via the existing `twin_group`
  field so the platform and any third party can recompute the metric.

### A3. subnet validator: block-hash-seeded audit selection and verdict

- **Selection** is a pure function of the already-block-hash-derived dataset
  seed, so it is reproducible and unpredictable at commit time. For each case,
  `audited = H(seed || case_id || "audit-v3") % 10000 < AUDIT_BPS`, and
  `transformID = H(seed || case_id || "xform-v3") % TRANSFORM_SPACE`. `AUDIT_BPS`
  is a public constant (start conservative, e.g. 1500 = 15%). Put the derivation
  in datagen so every consumer shares one implementation (mirror the
  `onchain_seed` pattern: byte-identical in api and validator).
- The audit runs **inside the normal scored run** (the transformed cases are
  part of the generated suite for that seed). No extra round trip, no new
  harness protocol. This keeps the "simple validator" invariant.
- **Verdict**: the transform-robustness metric already rides in the signed score
  report. A **failed audit** is the brittleness signature: base-family
  competence high, transform robustness low, beyond a public threshold
  `AUDIT_MIN_ROBUSTNESS` (calibrate against the real-model baseline the same way
  the σ gates are calibrated; a genuine reasoner is robust, so the floor sits
  below honest-model robustness and above a surface-brittle parser). Because the
  metric is in the signed report and the transforms are public, every validator
  computes the same verdict.
- **Where to apply it**: fold the audit into the finalize path, not a new sweep.
  A champion/tail agent (the only agents that matter, same reasoning as the
  contested-dethrone work) whose finalized k=3 reports show the brittleness
  signature is flagged. Reuse `_confirm_and_submit`-style plumbing if you need a
  re-score on fresh audit seeds for the champion, but prefer reading the metric
  already present in the finalized reports first.

### A4. platform: audit record and emission penalty

- On finalize (`submit_score` finalize branch, `endpoints/validator.py`), when
  the k=3 median transform-robustness is below `AUDIT_MIN_ROBUSTNESS`, transition
  the agent to review rather than `scored`: reuse `ATH_PENDING_REVIEW` (or add a
  sibling `AUDIT_PENDING_REVIEW` in `ditto-screening-protocol` if you want the
  reason distinct) so it is excluded from emissions until an operator resolves
  it, exactly like the copy-review hold. Do not auto-ban; quarantine-then-review
  matches the existing policy.
- Append an `EVENT_AUDIT` entry to the immutable audit chain with the public
  inputs (seed, audited case ids, transform ids, per-case transform scores, the
  robustness value) so the verdict is independently verifiable off the public
  feed. Never put an answer key in the audit payload (same redaction rule as the
  score audit entry).
- Surface `transform_robustness` on the public score record
  (`PublicValidatorScore`) and the dashboard, alongside the existing metamorphic
  and calibration fields, with a one-line "what this measures" note.

### Determinism / trustless checklist for Part A

- Transform selection, transform text, and transformed expected answer are all
  pure functions of public inputs `(seed, case_id, transform_id)`. A third party
  regenerates them from the published seed. YES is required.
- The verdict is a deterministic threshold on a metric carried in the signed
  report. No validator secret. YES is required.
- Audit cases are wire-indistinguishable from normal cases. YES is required
  (otherwise a hybrid reasons only where watched).
- Calibrate `AUDIT_MIN_ROBUSTNESS` against a real locked-model harness, never the
  zero-variance reference router (`docs/calibration-trust.md`). A too-high floor
  false-fails honest models, which costs legitimate miners. This is the main
  risk; treat it like the σ-gate calibration.

## Part B: task-side hardening rollup (the smaller v3.1 items)

### B1. Dump-guard half-hedge (`dittobench-datagen`)

The whole-table dump is caught; a narrowed half-dump (answer + `DumpFloor-1`
other self-values) is not (`grade/grade.go` `DumpFloor`, `persona/questions.go`
`applyDumpGuard`). Do NOT just lower the floor; that false-positives on a
verbose correct model. Instead shift from count-based to **distractor-based**
detection: plant K plausible-but-wrong same-attribute distractor values on
value-recall cases (the distractor scan already zeroes on a hit for some
families; extend it to value-recall), so any dump that sweeps candidates hits a
distractor and zeroes regardless of the floor. Add a gate that a half-dump
(answer + floor-1 guard values + one distractor) scores 0.

### B2. Injection naturalization (`dittobench-datagen`)

13 finite templates remain memorizable (`persona/questions.go`
`injectionTemplates`, `injectionTwins`). Compose injection framings from
per-seed-varied parts rather than a fixed pool, add mid-sentence and multi-turn
framings, and keep the non-trailing property. Add a gate asserting no single
template shape covers more than a small fraction of injection cases across a
seed sweep. The trajectory-anchored bait already forces the observed channel;
this only reduces the surface-pattern memorizability of the text payloads.

### B3. Lifecycle global-map probe (`dittobench-datagen`)

Read-path cross-user leak is probed (`gen/isolation.go`), but lifecycle chains
carry no `UserID`, so a delete/update under user A followed by a read of the
same attribute under user B is never exercised (`gen/lifecycle.go`). Add a
cross-user lifecycle case: WRITE or DELETE the attribute under user A, then READ
it under user B where B's value must be unaffected; the forbidden answer is A's
post-mutation state. A harness with a global (not per-user) saved/deleted map
leaks and zeroes. Reuse `SecondaryUser` from the isolation machinery.

### B4. Memory over-call penalty (`dittobench-api`)

Emitting both a tool response and a memory answer ("only one is graded") is
observable but unscored (`internal/scorer/scorer.go`; `ToolEfficiencyFactor`
counts only `KindTool && Observed`). Extend the efficiency factor (or add a
small memory-case penalty) so a pure-memory case whose OBSERVED trajectory
carries an unexpected NON-memory tool call is penalized. Only non-memory
over-calls: a legitimate `search_memories` call is fine. Grade off the
validator-observed trajectory (already substituted before grading), so it cannot
be laundered.

## Explicitly out of scope for this rollup

State these in the PR so reviewers know they were considered, not missed:
- **Response commit-reveal timelock (drand)** and **template-space refresh
  cadence** (`anti-gaming.md` specs #1 and #3): larger standalone on-chain
  protocol changes with their own timelock/keyshare and category-retirement
  machinery. They defeat within-round copying and slow-lookup relays, which the
  per-agent post-commit seed and the oracle already blunt. Defer as their own
  track; note them in the roadmap.
- **Anything claiming to force a model on a general local solver.** That is the
  oracle's job and the irreducible limit; do not attempt a deterministic version.

## Sequencing and PR mapping

Land in this order so each layer has what it depends on:
1. datagen: transform functions + selection helper + B1/B2/B3 + red-team gates
   (PR #2). This re-pins the public-vector hash; bump it with a dated changelog
   note as prior commits did.
2. api: grade the audit cases into `per_case`, emit `transform_robustness` +
   `audit_case_count`, B4 (PR #28). Pin bump to the new datagen tag lands with
   the release retag already in `docs/v3-release.md`.
3. subnet: audit verdict read from the finalized reports, penalty submission
   (PR #155).
4. platform: audit finalize branch, `EVENT_AUDIT`, public surfacing,
   `AUDIT_PENDING_REVIEW` status if added (PR #186 + a screening-protocol bump if
   a new status).
5. starter-kit: no code needed (audit cases are normal cases). Document in
   MINER.md that a fraction of cases are unpredictable rephrasings and that
   surface-brittle dispatch fails them.

## Calibration and test strategy

- Reuse the calibration harness path already exercised in this session: run the
  starter-kit baseline on the real locked model (OpenRouter/Chutes) over a seed
  sweep WITH audits enabled, measure honest-model transform robustness, and set
  `AUDIT_MIN_ROBUSTNESS` below it with margin. Record the numbers in
  `docs/BASELINES.md` as a new run, like the v3 baseline entry.
- Red-team gates (datagen) for every mechanism: surface-brittle parser fails the
  transform, half-dump zeroes, injection pool has no dominant template, cross-user
  lifecycle leak zeroes.
- Cross-repo determinism test: the transform selection + text + expected answer
  regenerate byte-identically from the seed in both the api and validator copies
  (mirror the `onchain_seed` contract test).
- End-to-end: a full real-model run with audits on, transcript published, and a
  third-party re-grade (dataset + transcript + public grader) reproducing the
  composite AND the transform-robustness metric.

## Definition of done

- Transform audit lands in all four repos, reproducible end to end, with the
  honest-scope framing in anti-gaming.md, dittobench-v2-vs-v3.md, MINER.md, and
  the dashboard.
- `AUDIT_MIN_ROBUSTNESS` calibrated against the real model with a recorded
  baseline; no honest-model false-fail in the calibration sweep.
- B1-B4 land with red-team gates.
- Every new deterministic input has a cross-repo regeneration test.
- All suites green; public-vector hash re-pinned; the release checklist in
  `docs/v3-release.md` updated so the audit ships in the same v0.8.0 tag +
  platform window rather than as a v3.1 follow-up.
- Out-of-scope items recorded in the roadmap, not silently dropped.
