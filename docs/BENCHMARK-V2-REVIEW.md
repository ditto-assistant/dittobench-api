# DittoBench v2 — Phase B Dataset Review

*A comprehensive, research-grounded review of the v2 procedural memory dataset:
comprehensiveness, use-case variety, volume, sentence/fact structure, and
alignment with the agentic-memory-benchmark literature. Companion to
[`BENCHMARK-V2.md`](../../ditto-subnet/docs/BENCHMARK-V2.md) (the design) and the
Phase B work packages B1–B9.*

Scope: `internal/persona` (Layer-1 plan), `internal/gen` (Layer-2 realization +
suite assembly), `internal/scorer` (grading), `pkg/protocol` (wire contract).

**Confidence key** (carried from the literature scan): `[P]` primary source
verified, `[S]` secondary/abstract-level, `[U]` post-cutoff preprint or
single-source — do not quote figures without confirming.

---

## 0. TL;DR verdict

The v2 engine is **structurally strong and research-aligned** on the axes that
sank v1: it is contamination-resistant by construction (seeded procedural
generation, per-submission crypto-random seed), it covers the full canonical
memory-ability taxonomy, it now spans **professional registers** (software /
medical / legal) rather than only casual/personal life, and its filler is
coherent rather than disjoint padding. Difficulty is controlled by explicit,
mostly-orthogonal dials.

Three **honest gaps** remain, all documented in §8 with the research that names
them:

1. **The literal-match trap (highest priority).** Some recall questions still
   lexically overlap the stored fact ("What is my favorite color?" ↔ "My
   favorite color is teal"). NoLiMa shows this can overstate memory ability by
   30-point margins. We mitigate on the *needle* side (LLM paraphrase moves the
   stored wording) but not yet on the *query* side.
2. **No human-in-the-loop answer-key audit.** Our answer key is
   machine-derived-and-guaranteed (the canonical value is verbatim-preserved), so
   it avoids LoCoMo's 6.4% hallucinated-key problem *by construction* — but the
   judge's acceptance behavior on wrong-but-adjacent answers is not yet audited.
3. **Contradiction is a defined, not inherited, category.** No incumbent
   benchmark gives it to us; our reversal mechanic is reasonable but unvalidated
   against an external standard.

Everything below expands these with sources and concrete examples.

---

## 1. What Phase B built (recap)

v1 scored memory against a **static LongMemEval fixture** — a fixed ~500-question
set that a miner could memorize, with binary yes/no grading and a 0.6/0.4
tool/memory split. Phase B replaced it with a **two-layer procedural engine**:

- **Layer 1 (`persona/plan.go`)** — a pure function `BuildPlan(seed, opts)`
  produces a *persona universe*: a typed fact graph (scalars, lists,
  preferences, reversible opinions, near-miss distractors) plus the session
  scripts that introduce those facts over a seed-derived timeline. No wall clock,
  no crypto-rand, no map-iteration order → **same seed ⇒ byte-identical plan**
  (the reproducibility contract, §4.2 of the design).
- **Layer 2 (`gen/persona_render.go`)** — the LLM (generator model) rewrites each
  beat into natural dialogue, **verifying** that the fact's canonical value
  survives verbatim before accepting the rewrite; on LLM error or failed
  verification it keeps the deterministic template (never a silent drop — fixes
  v1's W5 collapse).
- **Question derivation (`persona/questions.go`)** — a deterministic, data-driven
  pass reads the plan and emits questions with **exact ground truth**, stratified
  into fixed per-type / per-tier quotas by `gen/memory_v2.go`.
- **Composite** rebalanced to `0.5*tool + 0.5*memory` (`scorer`), `bench_version`
  bumped to 3, dataset SHA-256 pinned into the run details, staged Tier-C
  seeding + Tier-B raw-pairs added.

This review covers that engine **plus the enrichment made during the review**:
the data-driven derivation refactor, the software/medical/legal domain families,
and the coherent-filler rework (commits on `nick/benchmark-v2`).

---

## 2. Comprehensiveness & volume

### 2.1 Combinatorics (anti-memorization)

The design targets ≥10⁹ distinct universes; the realized engine is far past that
**before** counting the LLM surface layer. Universal scalar skeleton (nine
always-present attributes):

```
firstNames(48) × lastNames(40) × cities-residence(40) × occupations(40) ×
companies(32) × carModels(28) × cities-hometown(40) × firstNames-partner(48) ×
universities(28)  ≈  3.0 × 10¹⁵  distinct skeletons.
```

The **domain layer** (new) multiplies this: every persona draws one of 3 domains,
each contributing two scalar attributes + one list family, e.g. software adds
`languages(18) × editors(12) × C(services 16,3)=560 ≈ 1.2×10⁵`; medical and legal
are comparable (~7–9×10⁴). Counting domains lifts the skeleton to **~3×10²⁰**.

On top of that, still multiplicative: list-attribute subsets
(`C(projectNames 20, k)`, trips, pets), preference/opinion draws, which
attributes receive an update chain or reversal, the near-miss distractor draws,
the seed-derived timestamp window, and — the unbounded term — **LLM paraphrase**
of each beat (natural-language rephrasings of one intent are uncountable). This
directly satisfies the contamination-resistance strategy of *Functional
Benchmarks* (arXiv:2402.19450) and **GSM-Symbolic** (arXiv:2410.05229) `[S]`:
write the benchmark as a seeded function, generate fresh instances, never reuse.

### 2.2 Question volume & stratification

The candidate question pool per persona comfortably exceeds the sampled quota at
every run size (`small` 6, `medium` 20, `full` 50 memory cases). Selection is
**stratified**: *which* questions of a type appear varies by seed; *how many* of
each type does not (a variance reducer — see §6.2 of the design). Difficulty is a
fixed per-run quota over easy/medium/hard tiers, so difficulty is identical
across seeds by construction.

### 2.3 Coverage of the memory-ability taxonomy

Mapping our question types onto the field's converged ability set (LongMemEval's
five canonical abilities `[P, arXiv:2410.10813]` plus cross-benchmark additions):

| Canonical ability (source) | v2 question type | Status |
|---|---|---|
| Information extraction / single-hop `[LongMemEval IE; LoCoMo single-hop]` | `single-session-recall` | ✅ |
| Multi-session reasoning / synthesis `[LongMemEval MR; LoCoMo multi-hop]` | `multi-session` (count + list-all) | ✅ |
| Temporal reasoning / event ordering `[LongMemEval TR; TempReason]` | `temporal-reasoning` | ✅ |
| Knowledge update / overwrite `[LongMemEval KU]` | `knowledge-update` (latest-value-wins) | ✅ |
| Abstention / false-premise `[LongMemEval ABS; LoCoMo adversarial]` | `abstention` (needle-absent) | ✅ |
| Preference application / personalization `[LongMemEval single-session-preference]` | `preference` + `preference-application` | ✅ (split into recall vs *apply*) |
| Cross-session entity/state tracking `[RULER var-tracking; LME-V2 dynamic state]` | knowledge-update on domain scalars (re-diagnosis, language migration) | ◑ partial |
| Aggregation / counting `[RULER CWE/FWE; bAbI]` | `multi-session` count questions (`Numeric`) | ✅ |
| Contradiction / conflict handling *(no benchmark owns this — you define it)* | `contradiction` (opinion reversal) | ✅ (defined, §8.3) |

We cover **all five canonical abilities plus preference-application, counting,
and contradiction** — broader than any single incumbent. The one partial is
first-class *state tracking* (a value that changes 3+ times with intermediate
queries); we test the 2-state update chain, not the N-state trajectory.

---

## 3. Use-case variety & domain register

### 3.1 The case for professional domains

Incumbent conversational-memory benchmarks are **personal/casual by
construction**: LoCoMo personas derive from MSC chit-chat; PerLTQA is explicitly
"Personal"; LongMemEval centers life-state and preferences. The purpose-built
successor **LongMemEval-V2** (arXiv:2605.12493 `[P-verified]`) reframes the target
from *personal assistant* to *"experienced colleague"* over professional
environments (e-commerce, IT workflows) it calls "absent from prior
conversational memory benchmarks."

The mechanism that makes register matter for a **memory** system specifically:
retrieval and entity-matching quality demonstrably **do not transfer** from
casual to specialist text — **BEIR** (NeurIPS 2021, arXiv:2104.08663 `[P]`) shows
dense retrievers that beat BM25 in-domain fail zero-shot cross-domain; domain
jargon breaks the paraphrase + embedding match that a memory harness runs on
*every* store→retrieve→match. A single-register benchmark overfits (arXiv:2502.07445).

### 3.2 What v2 adds (worked examples — real generator output)

Every persona is now assigned exactly one professional domain (seed-derived), its
fact families layered onto the universal personal facts. Verified: across 60
seeds all three domains appear, exactly one per persona, each domain scalar
surfaces as a recall/update question with correct ground truth
(`TestDomainCoverage`). Real Layer-1 renderings:

**Software engineering** (persona *Theo Moreau*, seed 2):

```
[scalar/primary_language] "I mostly write Ruby at work."
[scalar/code_editor]      "I do all my work in Emacs."
[scalar/code_editor→upd]  "I gave up my old editor and moved to Cursor."
[list/service]            "I own the billing-gateway service now."
[distractor/code_editor]  "By the way, my neighbor Aran's code editor is Sublime Text."

Q [single-session-recall] What is my primary programming language?      → Ruby
Q [knowledge-update]      What code editor do I use?                     → Cursor
Q [multi-session]         List all the services I maintain.             → rate-limiter, webhook-dispatcher, billing-gateway
Q [temporal-reasoning]    Which did I tell you about first: picking up Ruby, or switching to Cursor?
```

**Medical** (patient persona *Malik Kaur*, seed 0):

```
[scalar/diagnosis]        "I was diagnosed with gout."
[scalar/medication]       "My doctor put me on warfarin."
[scalar/medication→upd]   "My doctor switched me from that to montelukast."
[list/allergy]            "I have an allergy to codeine."
[distractor/medication]   "By the way, a friend Zoe's medication is warfarin."

Q [single-session-recall] What is my current medical diagnosis?         → gout
Q [knowledge-update]      What medication am I currently taking?         → montelukast
Q [multi-session]         What am I allergic to?                         → iodine contrast, codeine, peanuts
```

**Legal** (lawyer persona *Amara Reyes*, seed 1):

```
[scalar/practice_area]    "My specialty is immigration law."
[scalar/practice_area→upd]"I switched specialties — I'm doing real estate law now."
[scalar/bar_admission]    "I'm licensed to practice in California."
[list/legal_matter]       "I'm lead counsel on the Marsh v. Coleridge appeal."

Q [knowledge-update]      What area of law do I practice?                → real estate law
Q [single-session-recall] Which state's bar am I admitted to?           → California
Q [multi-session]         List all the legal matters I've told you about.→ the Ashford merger, the Marsh v. Coleridge appeal, the Pelham zoning appeal
```

The specialist terminology is genuine (drug names, condition names, case
captions, service names, language/editor names) and, crucially, flows through the
**same** knowledge-update / distractor / temporal machinery as the universal
facts — so a knowledge-update can land on a *re-diagnosis* or a *language
migration* (the professional dynamic-state case LME-V2 targets), and near-miss
decoys pressure retrieval **on the jargon**, where BEIR predicts it hurts most.

### 3.3 Design note: why one domain per persona (not all three)

Mixing three professional registers into one persona would be unnatural (few
people are simultaneously a practicing lawyer, a clinician, and a service owner)
and would dilute the jargon-retrieval pressure that makes register matter. One
coherent domain per persona keeps the conversation realistic while guaranteeing
domain coverage across the miner population (every submission draws a domain).

---

## 4. Sentence & fact structure

### 4.1 What's strong

- **Facts are embedded in dialogue, not stated as a bare fact list.** Each fact
  is a user turn + an assistant acknowledgement, with the canonical value carried
  verbatim (`TestFactValuePreservedInBeat`). Multiple statement templates per
  attribute (3–4) plus LLM paraphrase give surface variety.
- **Filler is now coherent, not disjoint padding.** The single canned noise line
  was reworked into 16 first-person topics × 6 user/assistant template pairs, with
  the assistant staying on-topic. This directly answers the well-attested
  memory-benchmark critique that LLM-generated histories are padded with
  "semantically disjoint conversational fillers" — an artificial haystack that
  inflates lexical-retrieval scores rather than a realistic memory load `[U —
  recurring community critique, not one primary source]`.
- **Fact density / SNR is a dial.** `noiseBeat` injection and session count are
  explicit knobs; the design keeps memory input lean, consistent with
  LongMemEval's finding that best QA uses ≤1,000 tokens of memory and high filler
  *actively hurts* retrieval `[P, arXiv:2410.10813]`.
- **Near-miss distractors, not random noise.** Distractors draw the **same
  attribute, different entity/value** ("my neighbor Aran's editor is Sublime
  Text") — a same-template sibling, exactly RULER's MK-NIAH hard-negative
  construction `[P, arXiv:2404.06654]`, not an unrelated sentence.

### 4.2 Where structure is thin (see §8)

- **Query↔needle lexical overlap** on some recall types (the NoLiMa trap).
- **Distractors are single-turn** and always third-party-attributed; a stronger
  variant would place a *self*-attributed near-miss in an earlier, superseded
  session (harder to disambiguate from the current value).
- **Evidence position is not yet explicitly controlled** for lost-in-the-middle
  (§8.4).

---

## 5. Research alignment — point by point

| Research finding | v2 status |
|---|---|
| **Seeded-function generation** beats fixed sets for contamination (Functional Benchmarks; GSM-Symbolic) `[S]` | ✅ core design |
| **Non-memorizable surface tokens** (RULER: random values) `[P]` | ✅ values drawn from large disjoint pools per seed |
| **Synthetic temporal facts**, graph-grounded, not real-entity dates (Test of Time, arXiv:2406.09170; TempReason) `[P/S]` | ✅ timeline is seed-derived, anchored to a pinned epoch, never wall clock |
| **Fact-ordering sensitivity** — models exploit presentation order not time (Test of Time) `[P]` | ◑ we vary session day-gaps + shuffle, but don't yet randomize evidence *position within* the ordering question |
| **Difficulty on orthogonal dials** — length, hops, distractor count/similarity, evidence distance (RULER; GSM-DC power-law) `[P/S]` | ◑ tiers + distractor presence + session gap are dials; distractor *count* and evidence↔query *distance* are not independent knobs yet |
| **Hard negatives = near-miss, not random** (RULER MK-NIAH; HotpotQA TF-IDF distractors) `[P/S]` | ✅ same-attribute-different-value decoys |
| **Check distractors aren't accidentally correct** (false-negative trap) `[S]` | ✅ `pickDistinct` guarantees decoy value ≠ persona value |
| **Literal-match trap** — verbatim/lexically-overlapping needles overstate ability by huge margins (NoLiMa, arXiv:2502.05167; Context Rot) `[P]` | ⚠️ **partial** — mitigated on needle (paraphrase) not query (§8.1) |
| **Lost in the Middle** — U-shaped accuracy by evidence position (arXiv:2307.03172) `[P]` | ⚠️ not explicitly controlled (§8.4) |
| **Coherent filler**, not disjoint padding `[U critique]` | ✅ reworked (§4.1) |
| **Human-in-the-loop answer-key + adversarial curation** (LoCoMo 6.4% key errors; LongMemEval ~70% human-edited) `[P]` | ◑ key is verbatim-guaranteed (no hallucinated key) but no human adversarial pass (§8.2) |
| **Validate the judge** against wrong-but-adjacent answers (LoCoMo judge accepted 62.81%) `[P]` | ⚠️ judge hardening exists (design §6.1) but no adjacency-audit harness yet (§8.2) |
| **Multi-domain / professional register** (BEIR non-transfer; LongMemEval-V2) `[P]` | ✅ software/medical/legal added (§3) |
| **Scoring target shapes rankings** ("Same Ranking, Different Winner", arXiv:2605.24060) `[U]` | ◑ we pin one target (0.5/0.5 composite, containment memory); documented, not ablated |

---

## 6. Comparison to the field

| | LoCoMo | LongMemEval | LongMemEval-V2 | **DittoBench v2** |
|---|---|---|---|---|
| Real vs synthetic | synthetic + human-verify | hybrid (LLM + human edit) | agent web trajectories | **synthetic, seeded-procedural** |
| Reusable fixed set? | yes (10 conv/1,540 QA) | yes (500 Q) | yes (451 Q) | **no — fresh per submission** |
| Contamination-resistant | no | no | post-cutoff only | **yes by construction** |
| Register | casual/personal | personal life-state | professional | **personal + software/medical/legal** |
| Answer-key integrity | 6.4% errors `[P]` | high (curated) | high | **verbatim-guaranteed** |
| Abstention / false-premise | adversarial subset | ✅ | ✅ premise-awareness | ✅ |
| Knowledge update | — | ✅ | ✅ dynamic state | ✅ (incl. domain) |
| Human curation | ✅ | ✅ (~70%) | ✅ | ❌ (machine-guaranteed key) |
| Reproducible from (seed) | n/a | n/a | n/a | **✅ byte-identical** |

v2's distinctive strength is the **contamination-resistance + reproducibility +
verbatim-key** triple, which is exactly what an on-chain, adversarial, KOTH
weight mechanism needs (a miner must not be able to precompute, and a dispute
must re-score to the same bytes). The cost is the absence of human adversarial
curation — acceptable *because* the key is guaranteed rather than authored, but it
leaves the judge-adjacency risk (§8.2).

---

## 7. Live validation evidence

**Unit (deterministic layer):** full `internal/persona` + `internal/gen` suites
green, including `TestBuildPlanDeterministic` (byte-identical across rebuilds, 4
seeds incl. int64 max), `TestBuildPlanDistinctSeeds`, `TestQuestionTypeCoverage`
(all 8 types + all 3 tiers), `TestAnswersMatchGroundTruth`,
`TestKnowledgeUpdateAnswerIsLatest`, and the new `TestDomainCoverage` (one domain
per persona, all three reachable across 60 seeds, domain scalars → correct-answer
questions).

**End-to-end (real generator + real judge), `small` against the deterministic
refharness:**

- `bench_version = 3`, `dataset_sha256 = 6090ebcd…6193` present and pinned.
- Paraphrase: **19/20 applied, 1 fallback** — the real generator rewrote nearly
  every beat with no template collapse (v1's W5 failure mode absent).
- All six memory question types graded by the real judge (preference,
  preference-application, knowledge-update, abstention, multi-session,
  contradiction all present in `per_category`).
- Composite = `0.5*0.317 + 0.5*0 = 0.158`; the 0.5/0.5 weighting is exact.
- `memory_mean = 0` is the **refharness floor** (the reference harness holds no
  memory), not a dataset fault — the seed's software-domain facts (editor,
  service) rendered correctly into the 46-pair haystack, confirmed by artifact
  inspection.

**End-to-end, `medium` against the refharness** (exercises the staged/Tier
paths the `small` run does not):

- `bench_version = 3`, `dataset_sha256 = 76d2cf8a…` pinned; **2 staged Tier-C
  seeding waves** and **2 Tier-B raw-pairs cases** (haystack seeded *without*
  prepared subjects — the harder memory-construction test).
- Paraphrase **48/51 applied** (94%, 3 fallback).
- **All eight** memory question types graded by the real judge (adds
  `single-session-recall` and `temporal-reasoning` over the small run).
- This seed drew a **medical** persona, and a **domain knowledge-update case
  surfaced and was graded end-to-end**: the generator paraphrased "What is my
  current medical diagnosis?" → *"Can you provide my present medical diagnosis?"*
  with answer **atrial fibrillation** — a re-diagnosis, i.e. exactly the
  professional dynamic-state case LME-V2 targets, proving domain content flows
  through generation → paraphrase → judging with the canonical value preserved.
- Composite = `0.5*0.405 + 0.5*0 = 0.2025` (weighting exact; `memory_mean = 0` is
  again the refharness floor).

Together the two runs prove the enriched dataset **generates, hashes, stages, and
grades** correctly under the real LLM pipeline, including specialist-domain cases.
A real-recall number (memory_mean > 0) requires a memory-capable harness (the
starter-kit + Ollama `embeddinggemma` retriever) — the recommended next
validation, and a harness property rather than a dataset-correctness property.

---

## 8. Gap assessment & prioritized recommendations

### 8.1 [HIGH] Close the query-side literal-match gap (NoLiMa)

**Finding:** NoLiMa (arXiv:2502.05167 `[P]`) removes lexical overlap between
question and needle and watches accuracy collapse — GPT-4o 99.3%→69.7% at 32K;
restoring the literal match restores the score, proving standard NIAH measured a
**lexical shortcut**. Several of our recall questions still share keywords with
the stored fact ("What is my *favorite color*?" ↔ "My *favorite color* is teal";
"What *instrument* do I *play*?" ↔ "I *play* the cello"). We paraphrase the
*needle* (Layer 2 moves the stored wording) but the *question* is fixed in
`scalarAsk`/`prefAsk`, so a retriever can still key on the shared phrase.

**Recommendation:** add a paraphrase-gap requirement to question realization —
LLM-reword the question to remove content-word overlap with its evidence pair
(preserving the canonical answer), and/or add a `latent`/associative question
variant that requires an inference step ("Which of my facts would a colorblind
friend need to know about my study repaint?" → color). Keep the deterministic
answer key. Track a `lexical_overlap` telemetry field to prove the gap is real.

### 8.2 [HIGH] Judge-adjacency audit harness

**Finding:** LoCoMo's gpt-4o-mini judge accepted **62.81%** of intentionally
wrong-but-topically-adjacent answers `[P, verified]`; "Same Ranking, Different
Winner" (arXiv:2605.24060 `[U]`) shows the scoring target flips rankings. Our
answer key is safe (verbatim-guaranteed, no LoCoMo-style 6.4% hallucination), but
the *judge's* behavior on near-miss answers is unmeasured.

**Recommendation:** a calibration harness that feeds the judge (a) the correct
answer, (b) a plausible-but-wrong sibling from the same pool
(`pickDistinct`), (c) an off-topic answer, and asserts accept / reject / reject.
Run it in `benchcal` alongside the difficulty calibration; gate a `bench_version`
bump on it. This is cheap (reuses the pools) and directly de-risks the weight
mechanism.

### 8.3 [MED] Validate the contradiction mechanic

**Finding:** contradiction/conflict handling is **not** a first-class category in
any incumbent — we defined it (opinion reversal: "I loved whittling" → "I've
given up whittling"). It's reasonable but unbenchmarked.

**Recommendation:** add a second contradiction shape — a *factual* conflict
across sessions (two mutually-exclusive current-state claims where the later
supersedes) distinct from an *opinion* reversal — and confirm the judge rewards
citing the resolution ("no longer X") over either raw claim. Consider a
short human spot-check of 20 contradiction items (the one place a human pass buys
the most, since the answer is a phrase not a token).

### 8.4 [MED] Make distractor-count and evidence-position independent dials

**Finding:** RULER MK-NIAH degrades monotonically with distractor count; GSM-DC
shows error follows a **power law** in distractor count `[S]`; Lost-in-the-Middle
`[P]` shows a U-shaped position effect. We have distractor *presence* (a tier
bump) and session gaps, but not distractor *count* or evidence *position* as
orthogonal, logged knobs.

**Recommendation:** parameterize `Opts.DecoyPeople` per-attribute (count as a
dial), and record each evidence pair's normalized position in its session +
across the haystack, so difficulty can be held constant *and* ablated. Vary
target-evidence position deliberately (start/middle/end) rather than letting the
shuffle decide.

### 8.5 [LOW] N-state trajectory tracking

Extend one update chain per run to 3+ states with an intermediate query, to test
true cross-session state tracking (RULER variable-tracking / LME-V2 dynamic
state) rather than the 2-state latest-wins case.

### 8.6 [LOW] Multi-modal memory sources

All memory is chat. The validity literature flags "limited data diversity
(chat-only, ignoring emails/journals/docs)" `[U]`. A future tier could seed a
fact via a pasted "email" or "doc" turn. Low priority for SN118's current
product surface.

---

## 9. Sources

**Primary / verified:** LongMemEval (arXiv:2410.10813, ICLR 2025) · LoCoMo
(arXiv:2402.17753, ACL 2024) + Penfield Labs audit (6.4% key errors; judge
accepts 62.81%) · NoLiMa (arXiv:2502.05167, ICML 2025) · RULER (arXiv:2404.06654,
COLM 2024) · Lost in the Middle (arXiv:2307.03172, TACL 2024) · GSM-Symbolic
(arXiv:2410.05229) · GSM-IC (arXiv:2302.00093) · HotpotQA (arXiv:1809.09600) ·
BEIR (arXiv:2104.08663, NeurIPS 2021) · Test of Time (arXiv:2406.09170, ICLR
2025) · TempReason (arXiv:2306.08952) · LongMemEval-V2 (arXiv:2605.12493).

**Secondary / flag before quoting:** Functional Benchmarks (arXiv:2402.19450) ·
GSM-DC (arXiv:2505.18761) · PerLTQA (arXiv:2402.16288) · MemBench
(arXiv:2506.21605) · "Same Ranking, Different Winner" (arXiv:2605.24060) ·
Chameleon single-register overfitting (arXiv:2502.07445). Post-cutoff 2026
preprints are `[U]` — leads, not citations.

---

*Review generated on the `nick/benchmark-v2` branch. Enrichment commits
(data-driven derivation, SE/medical/legal domains, coherent filler) accompany
this document; the gap items in §8 are the recommended Phase-C follow-ups.*
