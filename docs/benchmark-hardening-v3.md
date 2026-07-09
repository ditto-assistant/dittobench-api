# DittoBench v3: hardening and coverage plan

Written 2026-07-09. This is the pre-publish review the generator publish
(docs/HANDOFF-generator-single-source.md) was waiting on. Inputs: a line-level
gameability audit of dittobench-datagen, a capability map of the production
backend against the benchmark catalog, and a literature pass over
contamination-resistant benchmark design (2024-2026). Sections 2 and 3 are
findings, section 4 is the plan, section 6 says what must land before the
one-way-door publish.

## 0. Status (2026-07-09)

All of Tier 0, Tier 1, Tier 2, and the generator-independent parts of Tier 3
have landed. One decision changed the plan: the benchmark stays at
`bench_version = 2`. Nothing has shipped to production, so there is no released
v2 surface to preserve; the entire hardening went into the current version
rather than splitting across a v2/v3 boundary. Every reference to
"bench_version 3" below is historical framing, not a live version bump. The
version constant does not move.

Landed:

- Tier 0 (0a-0e): legacy oracle path and seeddata deleted, opaque seed-keyed
  case_id, catalog drift fixed (array args + dynamic-goal workflow), per-seed
  injection payload, procedural abstention.
- Tier 1 (1a-1b, 1d-1g): surface grammar, no-op distractors and tell removal,
  intent-phrased tool args, dependent-arg multi-hop (job_chain_result_usage),
  scored error recovery (web_recovery_result_usage), near-miss tool presence.
- Tier 2 (2a-2f): automations/recipes, capability discovery, Google Workspace,
  memory-write routing, full settings cluster. Catalog is 45 tool categories.
- Tier 3 (3a-3f): arg-stuffing penalty, metamorphic twins with
  consistency sub-score, per-run nonce canaries with an integrity disqualifier,
  pass^k reporting, behavioral-plausibility advisory signal, composite_stderr
  threaded scorer -> ledger -> subnet KOTH band.
- Two additional idea-tracks from BENCHMARK-V3-IDEAS: DRM/interference lures and
  computed-answer modalities (#7/#9), a calibration/Brier proper-scoring slice
  (#6), and an offline G-study + 2PL reliability analyzer, `cmd/gstudy` (#4/#5).

Declined:

- 1c (sealed per-version pool deltas): rejected. It conflicts with the standing
  requirement that the dataset, generator, and benchmark stay fully public,
  transparent, and auditable so independent validators can reproduce every
  score byte-for-byte at any time. An encrypted just-in-time-revealed blob is a
  private surface by construction. The fallback treadmill is plain per-version
  pool rotation, which preserves full public reproducibility.

## 1. Threat model once the generator is public

Assumptions: generator source fully public, seed derived from an on-chain block
hash fixed after the miner commits (unpredictable), the harness receives the
seeded haystack and the tool catalog at eval time, k=3 validator median,
LLM judge on a slice.

The unpredictable seed kills the precomputed-lookup attack: nobody can build an
answer table before seeing data. It does not kill the read-time attack. A
harness can parse the served haystack and prompts using the generator's own
public template frames and answer programmatically, with no model call. Perfect
prevention is impossible because the harness legitimately holds everything
needed to answer. The achievable goals are:

- Cost asymmetry. Writing and maintaining the inverse parser should cost more
  than running a competent model harness.
- Treadmill. Template surfaces rotate per bench_version, so the parser decays
  on every release while a real harness is unaffected.
- Detection. Template matchers have measurable signatures the platform can
  flag or floor.

There is a partial consolation: a full inverse parser for the memory suite must
parse waves, track supersession chains, scope by user_id, count, and order by
timestamps. At the limit it is a working symbolic memory system. The defense
is to keep raising the semantic floor (paraphrase distance, judge-graded
grounding, varied surfaces) so the symbolic path converges on real capability
or breaks.

Research grounding, condensed:

- GSM-Symbolic (arXiv:2410.05229): name/entity swaps barely hurt memorizers.
  Value resampling, structural recomposition (clause and step count), and
  plausible-but-inconsequential "no-op" distractors caused drops up to 65%.
  Variation must be structural, not cosmetic.
- ARC-AGI-2 (arXiv:2505.11831) and LiveBench (arXiv:2406.19314): a public
  engine survives when a slice of configuration stays private per release and
  is revealed post hoc. LiveBench refreshes ~1/6 of content monthly with a
  short private window.
- Functional MATH() (arXiv:2402.19450): the static-vs-fresh-snapshot score gap
  is itself the overfit detector, measured at 58-80% for models that ace the
  static set.
- tau-bench / tau2-bench (arXiv:2406.12045, 2506.07982): pass^k (all k trials
  correct) collapses lucky template hits (90% pass@1 became 57% pass^8), and
  end-state grading beats transcript matching.
- BFCL v4: near-miss and irrelevant tools as a scored hallucination dimension
  (10% of composite), and recall questions deliberately phrased unlike the
  write-time phrasing.
- CapBencher (arXiv:2505.18102): designate one of several defensible answers
  as scored-correct so the attainable ceiling is below 100%; statistically
  exceeding the ceiling is proof of answer-key access. Non-LLM.
- Bittensor weight-copying literature: commit-reveal only defeats copying
  while scores actually churn. Rotation cadence is a security parameter, not a
  freshness nicety.

## 2. What is gameable today

Line-level audit of dittobench-datagen. The combinatorial persona entropy
(~1.5e14 skeletons) is real but defends only against the precomputed-lookup
attack the on-chain seed already kills. The read-time surface is close to
unmitigated. A ~300-line regex harness with no model captures the entire
deterministic scoring surface today.

1. case_id leaks the answer class. Tool IDs embed the category
   (`datagen/datagen.go:539`), memory IDs embed the question type and
   attribute (`gen/memory_v2.go:181`, q-inj-city, q-abs-3, q-count-project),
   isolation IDs embed the side (`gen/isolation.go:100`). The harness must
   hold the verbatim case_id (the tool_endpoint keys on it,
   `toolexec/toolexec.go:388`), so this cannot be redacted downstream. It
   hands over the routing-trap answers (the whole point of those categories is
   that phrasing misleads), flags injection and abstention cases before
   reading a word, and names the memory attribute being asked.
2. Tool ground truth is a pure function of category. expected_tools and
   expected_behavior derive from the category alone (`datagen.go:552-625`).
   The scored arg pools are 3-6 values per category and the arg value is
   almost always verbatim in the prompt. Effective scored entropy per tool
   case is ~1-2.6 bits. A map from category prefix to sequence plus
   copy-the-token solves the whole suite.
3. Every memory needle sits in a fixed, compile-time frame set. Scalar
   attributes have 3 statement frames and 2 update frames (`persona/plan.go`),
   assistant recommendations 2 frames, recurring mentions 5, distractors one
   "By the way, <rel> <name>'s <label> is <value>." frame (`plan.go:701`).
   Regex per attribute recovers everything, including knowledge-update (take
   the latest frame by timestamp) and aggregation (count label occurrences).
4. Injection resistance is one constant. The payload is the literal
   `PWNED-OVERRIDE` (`persona/questions.go:77`) inside 3 fixed wrappers.
   Suppress the constant, answer the wrapped question, pass every time.
5. Abstention is a fixed 11-string list. 7 pure-absent plus 4 false-premise
   questions (`persona/questions.go:197-224`). Match the string, decline.
6. Structural tells. Result-usage needles are always "<subject> reached
   <value> <unit>" in fixed carriers (`toolexec/toolexec.go:82,177`), coined
   from a closed 12x12x8 fantasy-name space that never collides with real
   content. Tier-A subject scaffolding names the attribute in a fixed
   description frame (`gen/memory_v2.go:478`). Originals land in the first
   half of sessions, updates in the second (`plan.go:534,556`).
7. Arg grading permits stuffing. argValueEqual accepts containment
   (`internal/scorer/trajectory.go:189`), so query="Tokyo Paris London ..."
   satisfies any city-valued arg. Nothing penalizes arg length or candidate
   packing.
8. Dead weight going public. The v1 LongMemEval path (GenerateMemory,
   `gen/memory.go`) and the 18MB `gen/seeddata/` bundle (including a
   500-question oracle with answers) have no non-test callers in the v2
   pipeline. Shipping them adds licensing exposure and misdirects auditors.

What is already robust and should be extended rather than diluted: the
result-usage needle value (served only at execution time, absent from prompt
and base-model knowledge), observed-trajectory authority with the 0.5
unobserved ceiling, per-user isolation scoping, and temporal/multi-hop joins
(the only memory items needing more than a single-frame regex).

## 3. Coverage vs Ditto core

The benchmark catalog is 18 tools. Production assembles a far wider surface
(backend `pkg/api/v2/chatv2-tools.go`, `pkg/mcp/tool_catalog.go`). Gaps ranked
by real-user impact:

1. Scheduling, automations, and recipes are entirely absent
   (create_automation, list_automations, create_recipe, apply_recipe). Cron
   automations are a headline capability.
2. discover_capabilities is absent, despite being the most-called onboarding
   tool.
3. Google Workspace is absent (~15 calendar/docs/sheets/gmail tools). "Add it
   to my calendar" is a constant real-world routing target.
4. Memory writes and graph management are absent (save_memory, update_memory,
   delete_memory, publish/unpublish, get_memory_network, get_subject_edges,
   graph sharing). Memory is tested read-only, so the mutate-vs-read routing
   surface is untested.
5. Settings breadth: 4 of 13 set_* tools are covered. The appearance cluster
   (accent color, fonts, density, corner radius, voice) is a natural near-miss
   discrimination space going unused.
6. Code Mode (search_tools, run_code) and external MCP routing are absent.
7. Artifacts is one flattened spec string vs 8 production operations
   (create/edit_plan/rewrite/present distinctions untested).
8. Error recovery and multi-turn clarification are acknowledged non-goals
   (COVERAGE.md), but observed execution now makes a scored recovery case
   feasible.

Two genuine mismatches where the benchmark tests something Ditto does not do:

- execute_agent_workflow is modeled as running a named, predefined workflow.
  Production has no named workflows: the real tool takes a free-form goal plus
  max_parallel and a planner decomposes it dynamically.
- Arg schemas drifted: production search_web takes queries[] (plus
  num_results, search_mode), read_links takes urls[], search_memories takes
  queries[], fetch_memories takes pairIds[] and stripImages. The catalog
  declares singular strings. The backend already exports the real schemas via
  PromptV2DefaultToolset() explicitly so DittoBench can present the same
  surface; the datagen catalog re-declares a simplified copy instead.

The memory suite's structure (pairs, subjects, links, waves, tiers, isolation
by user_id) maps faithfully onto the production schema. Untested layers
(subject_edges graph traversal, learned retrieval ranker, retrieval_events)
are internal to a harness and defensibly out of scope.

## 4. The plan

Four tiers. Tier 0 gates the publish. Tiers 1-2 are bench_version 3. Tier 3
is platform-side and independent of the generator repo.

### Tier 0: before the publish (blockers)

0a. Delete the legacy oracle path. Remove gen/memory.go (GenerateMemory),
    gen/seedassets.go, and gen/seeddata/ from dittobench-datagen. Hygiene,
    18MB of binary weight, and LongMemEval-derivative licensing in an MIT
    repo.

0b. Make case_id opaque. Replace the category/type/attribute-bearing IDs with
    a seed-keyed hash (e.g. first 16 hex of SHA-256(seed || ordinal)). The
    validator keeps the id-to-case map; the artifact already carries it. This
    is a protocol-visible change (starter kit examples show prefixed ids), so
    it must precede the publish or it becomes public churn. Highest single
    lever in this document: it restores the routing traps, blinds
    injection/abstention flagging, and hides the memory attribute.

0c. Fix catalog drift. Import or mirror PromptV2DefaultToolset() shapes:
    array args (queries[], urls[], pairIds[]), and re-specify
    execute_agent_workflow as goal + max_parallel with dynamic decomposition
    (which also forces a rethink of the job-vs-workflow trap: route on
    one-off-task vs decomposable-goal intent, not on a workflow name).
    Schema churn after the repo is public is louder and breaks miners twice.

0d. Randomize the injection payload. Derive the forbidden token per seed
    (e.g. coined-word + number from the needle machinery) instead of the
    constant PWNED-OVERRIDE. Small diff, closes the cheapest attack.

0e. Generate abstention procedurally. The plan knows the full attribute
    complement; draw absent-attribute questions from attributes the persona
    could have but does not, and render them through the same phrasing
    machinery as real questions. Kills the fixed-11-string match and makes
    abstention a true discrimination test (the question distribution becomes
    identical to answerable questions).

### Tier 1: structural hardening (bench_version 3)

1a. Surface grammar instead of fixed frames. Replace compile-time frame
    constants with a small compositional grammar per fact type: synonym sets,
    clause order, contractions, tense, optional lead-ins and trailing
    clauses, punctuation variation. Target well over 10^3 distinct surfaces
    per fact type instead of 3. Honest limit: the grammar is public, so a
    determined parser can embed it. The point is cost (a parser for a grammar
    is much more work than 3 regexes) combined with 1b and 1e.

1b. No-op distractor injection (GSM-Symbolic's strongest result). Splice
    seed-chosen, plausible but inconsequential clauses into haystack turns
    and tool prompts: near-miss facts about third parties, superseded-then-
    restored values, quantities attached to the wrong entity. A frame-matcher
    ingests them; a grounded reader does not. Also break the current
    structural tells: place originals and updates uniformly across sessions,
    stop sorting order3 event listings alphabetically, and vary the
    distractor frame as part of 1a.

1c. Sealed per-version pool deltas. Each bench_version ships part of its
    phrasing/template/pool surface as an encrypted blob in the public repo:
    ciphertext and hash public at release, decryption key revealed on-chain
    at the same moment the first scoring seed is fixed (after miner commits).
    Validators decrypt just in time; auditors verify post hoc against the
    hash, so byte-reproducibility from (seed, bench_version) is preserved
    once the key is public. The delta only protects until first reveal, so it
    rotates every version. This converts the treadmill into a schedule and is
    the cleanest public-code/private-surprise split available under
    distributed validation.

1d. Intent phrasing for tool args, everywhere. Expand the intent tables from
    4-5 fixed pairs per category to generated paraphrase surfaces (via 1a's
    grammar), and stop stating the scored arg verbatim in the prompt wherever
    the category grades an arg value. The scored value should require reading
    the request, not copying a token.

1e. Dependent-arg multi-hop (extend the strongest primitive). Chain served
    runtime values through trajectories: execute_agent_job returns a coined
    job_id from the mock endpoint, and the case expects get_agent_job_status
    called with that exact id; a search_web result carries a URL that the
    read_links hop must fetch. The arg value exists only in the prior call's
    served result, so the whole trajectory is unfakeable by construction, the
    same guarantee result-usage gives a single hop. This should become the
    backbone of the multi-hop category.

1f. Scored error recovery. The mock endpoint returns a transient error on the
    first call for a seed-chosen slice of observable cases; the case scores
    full only if the harness retries (or adapts) and completes. Observed
    execution makes "recovered" programmatically checkable: the served result
    after retry carries the needle.

1g. Near-miss tool presence. Present each case's tool list with seed-chosen
    irrelevant and almost-right tools drawn from the expanded catalog
    (Tier 2), keeping real production names. Wrong-tool invocation is already
    penalized; the expanded surface makes prefix-free routing genuinely
    discriminative (BFCL's hallucination dimension without inventing fake
    tools).

### Tier 2: coverage expansion (bench_version 3 or 4)

2a. Automations and recipes: create_automation vs execute_agent_job vs
    create_recipe routing traps ("every morning" vs "right now" vs "save
    these steps"), list/status hops, and an auto_approve_tools arg case.
2b. discover_capabilities: "what can you do" and feature-question routing,
    including the restraint side (a question answerable without it).
2c. Google Workspace: calendar create/update/search discrimination, docs vs
    sheets routing, gmail_send as a gated-action case. These are also the
    natural near-miss pool for 1g.
2d. Memory-write routing: save_memory vs search_memories (statement vs
    question), update vs save (existing fact), delete on request,
    plus a restraint case (a passing mention that should not trigger a save).
2e. Full settings cluster: 13 set_* tools with near-miss discrimination
    (accent color vs theme, chat font vs interface font vs font size).
2f. Artifacts operations: create vs edit_plan vs rewrite vs present
    discrimination on an existing-artifact premise.
2g. Memory capacity scaling: grow haystack size as a run_size dimension and
    report (not score) retrieval degradation, per MemBench.

Coverage and anti-gameability are the same change here: every real tool added
is entropy a router must handle and a near-miss a template cannot.

### Tier 3: scoring and platform defenses (no generator change)

3a. Arg-stuffing penalty. In argValueEqual/argCorrectness, penalize when the
    observed arg is a large multiple of the expected value's length or packs
    multiple candidate pool values. Closes the containment loophole.
3b. Metamorphic twin pairs. Emit occasional same-template pairs differing in
    one load-bearing mutation at unpredictable dataset positions. Correct on
    A with the stale answer on A' is a memorization fingerprint; report it as
    an advisory signal alongside the anti-copy gate.
3c. Canary values. Seed-derived unique tokens in filler content. A response
    containing a canary from a different seed, an unreleased version, or a
    sealed pool is binary proof of answer caching or pool exfiltration.
3d. Per-miner version-gap metric. Publish each miner's score delta across
    bench_version rotations (the functional-benchmark reasoning gap). A real
    harness is flat; a template harness craters on every rotation until
    updated. Consider weighting emissions toward worst-recent-version.
3e. Pass^k within families. Where a family has multiple instances per run,
    report all-correct alongside the mean; consider folding a bounded
    reliability term into the composite in a later version.
3f. Behavioral plausibility signal (advisory only). Sub-model-inference
    latency with near-perfect deterministic scores and near-zero output
    variance across cases is the regex-harness signature; forward it like the
    structural fingerprint, never fold it into the score.
3g. Confirm the anti-seed-farming posture and Ladder-style thresholded
    leaderboard updates (only move a published score beyond a noise band),
    both already flagged in open-generator-plan.md. Rotation cadence per 1c
    is the third leg: commit-reveal seeds only deter copying while scores
    churn.

## 5. Accepted limits

- A maximal inverse parser remains possible. With 1a-1e in place it must
  implement grammar parsing, distractor discrimination, supersession
  tracking, per-user scoping, runtime value threading, and per-version
  re-derivation from sealed deltas. That artifact is close to a real symbolic
  assistant, and 3b-3f exist to catch the shortcuts.
- The LLM-judged half is not covered by this plan. Judge hardening
  (structured output, wider deterministic rubric, SCORER_MODEL_B residuals)
  is tracked separately in docs/judge-determinism.md.
- Multilinguality, real multimodality, live multi-turn dialogue, and
  long-context stress stay out of scope, as recorded in COVERAGE.md.

## 6. Sequencing against the generator publish

Tier 0 must land in dittobench-datagen (and the starter kit where the
protocol shows) before executing the handoff's section 6 land sequence. All
five items are small diffs; 0b and 0c touch the shared protocol and are
exactly the changes that should never happen as post-publish churn on a repo
whose first impression is the audit narrative.

Tier 1 and 2 are bench_version 3 work and do not gate the publish, with one
exception: decide on 1c (sealed pool deltas) before the publish, because the
repo layout (ciphertext blob, key-reveal convention) is public interface. If
1c is declined, the fallback treadmill is plain per-version pool rotation,
which still works but gives miners the full fitting window between release
and first scoring epoch.

Tier 3 lands on the platform and validator sides on its own schedule.
