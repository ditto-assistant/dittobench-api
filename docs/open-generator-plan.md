# Plan: open-source the dataset generator

Status: **PLAN ONLY. Do not execute without Nick's explicit approval**, the same
gate as publishing ditto-subnet (task #38). Publishing source is effectively
irreversible (public git history), so this is a one-way door. This document is
the design and the go-live checklist, not an instruction to act.

## Goal

Make the benchmark's dataset generator public so the benchmark is maximally
auditable: anyone can regenerate any seed's dataset (including answer keys),
independently re-grade any scored submission, and inspect exactly how the
anti-overfit machinery works. This is the endpoint of the "transparency is the
trust mechanism" direction.

## Why this is safe now (and was not before)

The generator was kept private because generation is deterministic: with the
generator source plus a predictable seed, a miner could pre-compute their exact
dataset and overfit to it.

On-chain seed derivation (task #54, landed) removes that dependency. The seed is
now derived from an on-chain block hash fixed at job-ready, which is causally
after the miner has already committed their submission. The miner cannot know
their seed in advance, so even a fully public generator cannot be pre-computed
against. The anti-overfit guarantee moves from "keep the generator secret" (an
operational secret that can leak) to "the chain provides an unpredictable seed"
(a cryptographic property). That is a strictly stronger footing, and it is what
makes opening the generator defensible.

## Residual risks and mitigations

- **Distribution overfitting.** With the generator public, a miner can study the
  full distribution of cases (categories, difficulty knobs, phrasing variants)
  and build a harness that is good at that distribution. This is already largely
  public (COVERAGE.md, the `/v1/sample` sampler, the open harness/starter-kit),
  and "being good at the benchmark's task distribution" is the point of the
  benchmark. Specific-instance overfitting stays blocked by the fresh on-chain
  seed. Net: acceptable.
- **Seed farming.** A miner could resubmit repeatedly to draw different on-chain
  seeds and keep their best score. This is orthogonal to generator openness (it
  exists today) and is bounded by submission cost plus the KOTH first-seen
  tiebreak. Before go-live, confirm the anti-farming posture (per-hotkey
  submission cooldown / rate limit) is adequate; opening the generator does not
  worsen it but does make it more worth checking.
- **Platform block-choice grinding.** The platform reads "latest block" at
  job-ready and could in principle retry to influence the seed. Mitigated today
  by the pinned, publicly verifiable block reference (the platform commits to a
  specific block). A future-block binding (derive from a block N ahead of the
  submission's commit block) removes even this; recommended as a hardening step
  before or shortly after go-live.
- **Judge exposure.** Opening the generator does not require opening the judge.
  Decide explicitly what stays private (the judge rubric / grading prompts, if
  their secrecy carries any anti-gaming value) versus what ships. The generator
  is non-LLM and deterministic; the judge is a separate LLM-graded component.

## What becomes public

- `internal/gen` (the deterministic, non-LLM generation pipeline) and the
  `DatasetArtifact` schema (already the shape validators score).
- `cmd/generate` (the generate service entry point) and the private
  `ditto-data-pipeline` copy, consolidated so there is one public generator.
- `internal/persona` and the tool/memory case builders it drives.

What is NOT in scope for this task: the scoring engine internals, the judge
rubric, any keys/secrets, and the platform's private business logic. Confirm the
generator can be extracted without dragging those in.

## Prerequisites / go-live checklist

1. On-chain seed derivation landed (task #54). DONE.
2. Anti-seed-farming policy confirmed adequate (submission cost / cooldown).
3. Optional hardening: future-block seed binding (removes platform block-choice
   grinding).
4. Judge-privacy decision: what, if anything, stays closed.
5. Repo hygiene: the extracted history contains no secrets, no keys, no private
   judge data. A fresh-history extraction (not a full mirror) is safest.
6. License decision (MIT, matching the public starter-kit / harness).
7. **Nick's explicit approval.** This is the gate. Nothing is made public before
   it, exactly like ditto-subnet (#38).

## Rollout (only after approval)

1. Extract `internal/gen` (+ persona, + the generate service) into the public
   generator repo with a clean history.
2. Repoint imports; keep dittobench-api's in-tree copy (it needs the generator
   for `/v1/score` regeneration) or depend on the public module.
3. CI + determinism check (same seed → same artifact bytes) in the public repo.
4. Announce alongside the finalized-dataset reveal (task A) so the community gets
   generator + reproducible datasets + independent re-grade together.

## Reversibility

Publishing source cannot be undone (the history is public the moment it lands).
Treat approval as final and one-way. Until then, tasks A / B / C deliver most of
the auditability benefit without this irreversible step.
