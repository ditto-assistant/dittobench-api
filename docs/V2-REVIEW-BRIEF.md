# DittoBench v2 — Full-Review Brief (for a reviewing agent)

**You are performing a full review pass over all DittoBench v2 work.** This is a
multi-repo benchmark redesign for Bittensor SN118: a validator that scores a
miner's agentic-memory harness. The work spans four phases (A/B/C implemented; a
v3 KOTH/reliability layer landed in the validator). Your job is to read the code
and docs below and produce a prioritized findings report.

Review **primarily for, in priority order**:

1. **Correctness** — does it do what it claims, exactly?
2. **Concision** — is it as small as it can be without losing clarity?
3. **Scalability** — does it hold up at `run_size=full` and beyond, within budget?
4. **Direct, readable code** — obvious control flow, honest names, right comment density.
5. **Robustness** — does it degrade safely under errors, edge cases, and adversaries?

Depth over breadth on correctness: a single confirmed correctness or determinism
bug is worth more than a page of style notes. Verify claims against the code —
don't take a comment's word for it.

---

## 1. What DittoBench v2 is (orientation)

v1 scored a harness against a **static, memorizable LongMemEval fixture** with a
binary judge. v2 replaces that with a **seeded procedural engine** so every run's
data is fresh (nothing to precompute), plus deterministic-first graded scoring,
judge hardening, and — in Phase C — **observed tool execution** (the validator
serves tool results and scores what it observes, not the harness's self-report).

The three implemented phases:

- **Phase A** — harden v1 in place: seed-derived time, graded memory credit,
  trajectory/argument scoring, full tool-catalog coverage, judge hardening
  (fencing + injection tripwire + optional 2nd judge), abstention cases.
- **Phase B** — the data engine: `internal/persona` plan layer (typed fact graph,
  update chains, reversals, near-miss distractors, 4 professional domains) →
  LLM surface realization (verify-or-fallback) → data-driven question derivation
  with difficulty tiers; seeding tiers (A prepared / B raw-pairs / C staged
  waves); dataset hashing; NoLiMa query-side low-overlap rewrite; judge-adjacency
  audit; 0.5/0.5 composite.
- **Phase C** — observed execution: `internal/toolexec` mock tool endpoint,
  observed-call trajectory scoring (retires self-report / W3), result-usage
  scoring, `user_id` multi-graph isolation.

**The two contracts that make or break everything — check these hardest:**

- **Determinism / reproducibility.** The whole plan layer must be a pure function
  of `(seed, bench_version)`: same seed ⇒ byte-identical plan and identical
  `dataset_sha256`. Entropy is `math/rand` seeded from one int64 **only**. There
  must be **no** `time.Now()`, no `crypto/rand`, and no reliance on Go
  map-iteration order anywhere in generation (Go randomizes map ranging — keys
  must be sorted before iterating). A single violation silently breaks the
  ledger's "re-run and challenge" promise.
- **Additive-optional wire.** Every wire change is a new optional field; an old
  harness that ignores it must still score (possibly capped), never error. The
  `ScoreReport` aggregate shape (`run_id`, `seed`, `composite`, `tool_mean`,
  `memory_mean`, `median_ms`, `n`) is a fixed DB/signature contract — all new
  data lives inside the opaque `details` / `per_case` JSON.

Both contracts are spelled out in the design doc §2 and §11.3 (below); treat
those as the invariants and hunt for anything that violates them.

---

## 2. Read these documents first (in order)

| Doc | Repo | What it is |
|---|---|---|
| `docs/BENCHMARK-V2.md` | **`ditto-subnet`** | The design/spec: verdict on v1, invariants (§2), alignment matrix (§3), data engine (§4), suites & scoring (§5), judge + variance (§6), protocol impact (§7), calibration gates (§8), rollout/versioning (§9), and the implementation handoff with per-WP anchors and done-criteria (§11). **Start here.** |
| `docs/BENCHMARK-V2-REVIEW.md` | `dittobench-api` | Research-grounded companion review of the Phase B dataset + the Phase C summary (§8.7). Explains *why* each mechanic exists (with sources) — useful for judging whether the code actually achieves the stated intent. |
| `PROTOCOL.md` | `dittobench-api` | The miner-facing wire contract, incl. the Phase C `tool_endpoint` / `user_id` / `ToolExec*` shapes. |

The design doc's §11.4 has the per-work-package (A1–A10, B1–B9, C1–C3) task list
with code anchors and "done when" criteria — use it as your correctness checklist:
for each WP, confirm the code meets its done-criterion.

---

## 3. Repos, branches, scope

All work is on the `nick/benchmark-v2` branch of each repo (nothing pushed).
Review the branch diff against `main` unless noted.

| Repo | Path | Branch | Role in the review |
|---|---|---|---|
| **`dittobench-api`** | `~/projects/dittobench-api` | `nick/benchmark-v2` | **Primary work site.** The Go scorer: generation, judging, scoring, sandbox, mock tool server. ~60 files changed vs `main`. |
| `dittobench-starter-kit` | `~/projects/dittobench-starter-kit` | `nick/benchmark-v2` | Rust reference harness + local practice scorer. Wire types must stay byte-compatible with the Go side. 8 files. |
| `ditto-subnet` | `~/projects/ditto-subnet` | `nick/benchmark-v2` | Validator worker: `details` passthrough, W&B telemetry, KOTH margin/CRN/dethrone-band, version-bump re-score. |
| `ditto-harness` | `~/projects/ditto-harness` | `main` | Memory crate. **Unchanged** by v2 — read-only reference for what Tier B asks harnesses to replicate. |
| `ditto-platform` | `~/projects/ditto-platform` | — | **No changes.** The `ScoreReport` DB/signature contract lives here; verify v2 respects it (read-only). |

Get the exact commit list per repo with `git log --oneline main..HEAD`.

---

## 4. File map — where to look, and what to check

### `dittobench-api` (Go — the core)

**Wire contract & determinism anchor**
- `pkg/protocol/protocol.go` — all wire types (`RunRequest` incl. Phase C
  `ToolEndpoint`/`UserID`, `ToolExecRequest/Response`, `MemoryCase`, `SeedRequest`,
  `CaseScore` incl. `ResultUsage`, `RunDetails` telemetry). Check: additive-only,
  `omitempty` correctness, `ScoreReport` aggregate shape unchanged.
- `pkg/protocol/epoch.go` — `BenchVersion` (held at **2** pre-launch — see the
  comment and confirm the rationale is coherent) and `DatasetEpoch` (the pinned
  "as-of" instant that replaces wall-clock time). Check: no `time.Now()` leaks.

**Generation (the determinism-critical layer)**
- `internal/persona/{plan.go,pools.go,helpers.go,questions.go}` — the pure plan
  layer: typed fact graph, update chains, reversals, distractors, domains,
  N-state trajectories, and data-driven question derivation with exact ground
  truth. **Highest-value determinism review.** Check: seed-only entropy, sorted
  map iteration, ground-truth answers are exact, pool arithmetic really is ≥10⁹.
- `internal/gen/{gen.go,memory.go,memory_v2.go,persona_render.go,tools.go}` —
  suite assembly: stratified sampling, seeding tiers (A/B/C), staged waves,
  surface realization (verify-or-fallback), profiles. Check: the Tier-B "no
  Tier-A fact loses its subject" two-pass logic (`memory_v2.go`); wave
  partitioning; fallback never silently drops a fact.
- `internal/gen/{lexical.go,question_gap.go}` — NoLiMa query-side low-overlap
  rewrite + overlap telemetry. Check: the rewrite can't leak the answer and
  strictly drops a shared content word.
- `internal/gen/isolation.go` — **Phase C C3.** Second persona under a different
  `user_id`; cross-user isolation cases. Check: the "other graph always holds a
  *distinct* value" invariant; expected answer is always the *queried* user's
  own value; determinism (`seed ^ salt`); the secondary graph is template-rendered
  (zero LLM cost).
- `internal/gen/artifact.go` — `DatasetArtifact` (the hashable snapshot). Check:
  field order fixed, no Go maps in the payload (byte-stable JSON), and it covers
  everything scoring-relevant (tool cases, memory waves incl. the secondary
  isolation wave, cases, tool-fixture needles).
- `internal/datagen/datagen.go` — tool-case grammar incl. the Phase C
  `*_result_usage` categories whose prompt subject is coupled to the fixture
  needle via `toolexec.NeedleFor`. Check: coupling is deterministic and coherent;
  `IsResultUsage` classification.
- `internal/toolexec/toolexec.go` — **Phase C C1/C2.** The mock tool endpoint:
  per-case seeded fixtures, the coined "needle" (fabricated so it's unguessable
  and un-memorizable), the HTTP server that serves results **and records the
  authoritative observed trajectory**. Check: memory tools are never served
  (would leak answers); the observed recording is concurrency-safe; results are
  pure in `(seed, caseID, tool, args)`.

**Scoring**
- `internal/scorer/scorer.go` — graded memory credit (`0.7·correctness +
  0.3·grounding`, deterministic-first), the composite (`0.5·tool + 0.5·memory`),
  `ScoreToolCaseObserved` (observed trajectory is authoritative over self-report),
  `CapUnobserved`/`UnobservedCeiling` (0.5), `ComposeResultUsage`
  (`0.4·trajectory + 0.6·answer-carries-needle`), and the isolation "force the
  judge, no containment short-circuit" rule. **Verify every scoring formula and
  every deterministic-match helper** (`deterministicMemoryHit`,
  `containsBoundedPhrase`, `containsNumberToken`, `answerCarriesValue`,
  `stripSeparators`) — these decide credit; a false positive credits a wrong
  answer.
- `internal/scorer/trajectory.go` — name-F1 / arg-F1 / order credit / extra-call
  penalty. Check: F1 math, order credit, penalty scaling, `AllowExtraTools`.
- `internal/scorer/judge.go` — judge hardening (fenced untrusted blocks,
  `injection_attempt` tripwire, graded judge, optional 2nd model on an audit
  slice). Check: injection can't be scored as correct; fail-closed on judge error;
  the run-level outage gate logic.

**Pipeline & harness plumbing**
- `cmd/dittobench-api/main.go` — `runSizeJob`: the full build→generate→hash→seed→
  run→judge→score→finish flow, incl. standing up the toolexec server
  (`startToolServer`, host.docker.internal vs 127.0.0.1 vs unadvertised), the
  observed/capped/result-usage branching, secondary-graph seeding, and per-case
  `user_id`. **This is where the phases integrate — check the wiring end-to-end.**
- `internal/runner/runner.go` — per-case HTTP driver (`RunCase`, `CaseOptions`,
  `Seed`), timeouts, fail-to-zero.
- `cmd/refharness/main.go` + `internal/refharness/{route.go,execute.go}` — the
  deterministic no-LLM reference harness; `execute.go` runs routed tools through
  the endpoint (used in the observed-execution E2E test). Check: still a pure
  function of `(prompt, tools, seeded endpoint)`.
- `cmd/benchcal/` — offline variance/difficulty calibrator + judge-adjacency
  audit. Check: the σ measurement and the audit thresholds.

### `dittobench-starter-kit` (Rust — the miner surface)

- `src/protocol.rs` — wire types; **must stay byte-compatible with the Go
  `pkg/protocol`.** Check field names/`serde` attrs against the Go side
  (`tool_endpoint`, `user_id`, `ToolExecRequest/Response`, `result_usage`).
- `src/baseline.rs` — the reference agent. **Phase C:** `WireTool`/`ToolExecCtx`
  execute catalog tools through `tool_endpoint` (shared HTTP client + monotonic
  hop counter) and `run()` honors `user_id` (scoped retrieval). Check: degrades
  cleanly when the endpoint is absent/unreachable; hop ordering; memory tools
  still run locally (not routed to the endpoint).
- `src/{seed.rs,scorer.rs,datagen.rs}`, `PROTOCOL.md`, `README.md` — B7 parity.

### `ditto-subnet` (Python — the validator)

- `ditto/validator/telemetry.py` — passes the scorer's `details` (incl. Phase C
  `observed_tool_cases`/`capped_tool_cases`/`isolation_cases`) to W&B. Additive.
- `ditto/validator/{worker.py,weights.py,crn.py,config.py,dittobench.py}` —
  version-aware fold, re-score sweep, KOTH margin, common-random-number re-scoring,
  indifference-band dethroning. Check: the fold only compares the max
  `bench_version` present; the re-score sweep triggers correctly; passthrough of
  `details` is verbatim (opaque).
- `ditto/tests/validator/` — the test suite (`test_worker.py`,
  `test_telemetry.py`, `test_crn.py`, `test_dethrone_band.py`, `test_bench_version.py`).

---

## 5. Cross-cutting invariants to verify (hard rules — design §11.3)

Treat a violation of any of these as a **correctness** finding, not a nit:

- [ ] Plan/generation entropy is `math/rand` from the run's int64 **only** — no
      `time.Now()`, no `crypto/rand`, no unsorted map iteration.
- [ ] Same `(seed, bench_version)` ⇒ byte-identical plan-layer dataset and
      identical `dataset_sha256` (there are golden/determinism tests — confirm they
      actually pin bytes, and that the artifact covers all scoring-relevant data).
- [ ] `ScoreReport` aggregate shape unchanged; all new data in `details`/`per_case`.
- [ ] Every wire change is additive-optional; an old harness scores (capped),
      never errors. The Phase C `tool_endpoint` unreachable / unadvertised path
      must degrade, not fail.
- [ ] Deterministic answer-matching never credits a wrong answer (false positives
      are worse than false negatives — a miss defers to the judge).
- [ ] Fail-closed: per-case LLM error → 0 for the miner; a *persistent* judge
      outage fails the **run** (no garbage score recorded).
- [ ] LLM usage stays within budget with headroom at `run_size=full`;
      `run_size=small` stays cheap.
- [ ] Anti-gaming actually holds: self-report can't beat observed; the result-usage
      needle is unguessable without executing; a "dump both users' values" answer
      doesn't pass isolation via incidental containment; nothing in the corpus is
      base-model-memorizable.

---

## 6. Scalability & performance angles

- Per-run **LLM call count** at `run_size=full` (60 tool + 50 memory + isolation +
  staged waves): generator paraphrase + judge. Where does it approach the token
  budget? Is the deterministic-first path actually cutting judge calls?
- The `toolexec` server holds per-case state for the whole run; the trajectory
  scorer, F1 maps, and haystack partitioning are the hot loops — any accidental
  O(n²) over cases/pairs/facts?
- Between-seed **variance** (σ ≤ 0.01 target, design §6.2/§8) — the stratification
  and graded credit are the levers; sanity-check the calibrator's math.
- Generation memory: the full haystack (hundreds of pairs) is built and hashed in
  memory; fine at these sizes, but note anything that wouldn't scale to a larger
  profile.

---

## 7. Already-spotted items (confirm / address; not exhaustive)

These were noticed in passing — verify and fold into your report as appropriate;
do not treat this list as the ceiling.

- **Stray tracked binary.** `dittobench-api/refharness` (an 8.6 MB ELF) is
  committed to the branch (introduced in `0b06681`, WP A9) and is **not**
  gitignored. Almost certainly an accidental build artifact — recommend removing
  it from history/tracking and adding a `.gitignore` entry.
- **Pre-existing formatting drift, not CI-enforced.** `dittobench-api` has a few
  non-`gofmt` files (`internal/scorer/graded_test.go`, `internal/netguard/netguard_test.go`,
  `cmd/benchcal/judgeaudit.go`); `dittobench-starter-kit` has repo-wide `rustfmt`
  drift. Worth flagging whether formatting should be enforced, but the drift
  predates this work.
- **Validation gaps (known).** No keyed `run_size` E2E has been run (needs
  `OPENROUTER_API_KEY`); the starter-kit's local `evaluate` does not yet mirror
  observed execution (no local mock endpoint). Confirm the unit/integration
  coverage is adequate in the meantime.
- **Phase C reachability.** In hosted practice with a *remote* `harness_url`, the
  validator's ephemeral tool endpoint is unreachable, so observable tool cases are
  scored capped. Confirm this is the intended, documented behavior and not a
  silent scoring surprise.

---

## 8. How to build, test, and verify

| Repo | Commands |
|---|---|
| `dittobench-api` | `go build ./... && go vet ./... && go test ./...`; offline calibration: `go run ./cmd/benchcal -runs 3 -n 46 -nmem 12`; the observed-execution E2E lives in `internal/refharness/execute_test.go` (`TestObservedExecutionRoundTrip`, `TestResultUsageEndToEnd`). |
| `dittobench-starter-kit` | `cargo build && cargo test --lib` (18 lib tests). Repo isn't `rustfmt`-clean; don't let a repo-wide `cargo fmt` mask real diffs. |
| `ditto-subnet` | `uv run pytest ditto/tests/validator -q`; `uv run ruff check ditto/` ; `uv run mypy ditto/`. |

A keyed end-to-end (`run_size=small` against a real retrieval harness, with an
`OPENROUTER_API_KEY`) is the highest-value validation still missing — if you can
run it, do; otherwise note it.

---

## 9. Deliverable — what to produce

A single findings report, **correctness-first**, structured as:

1. **Verdict** (2–4 sentences): is the v2 work sound to ship behind the current
   `run_size=full` profile? Biggest risks?
2. **Findings**, each with: **severity** (blocker / major / minor / nit),
   **dimension** (correctness / concision / scalability / readability / robustness),
   `repo path:line`, a one-line statement of the defect, and a concrete failure
   scenario or repro where it's a correctness/robustness claim. Rank most-severe
   first. Prefer confirmed defects over speculation; when uncertain, say so and
   say what would confirm it.
3. **Determinism/reproducibility attestation**: explicitly state whether you found
   any seed-nondeterminism (`time.Now`, `crypto/rand`, map-order) in the generation
   path, and whether `dataset_sha256` genuinely pins all scoring-relevant bytes.
4. **Concision/readability opportunities**: dead code, redundant paths,
   over-abstraction, misleading names/comments — grouped, not one-per-line.
5. **Test-coverage gaps** for the highest-risk logic (scoring formulas,
   determinism, isolation, observed-vs-self-report, injection).

Keep it tight and evidence-backed. A short report of confirmed, well-located
findings beats an exhaustive but shallow sweep.
