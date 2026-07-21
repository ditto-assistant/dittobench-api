# v5 ship-gate replay against real leaderboard harnesses (4.11)

The current top leaderboard harnesses were run, **unmodified**, against `bench_version`
5 (full) and scored by the real dittobench-api pipeline. Harnesses were fetched from
the platform (backroom `get_screening_artifact`), built from source (`docker build`),
and driven locally: memory embeddings via Ollama `embeddinggemma`, chat via the locked
model **Qwen3-32B** (OpenRouter `qwen/qwen3-32b`, the validator key), memory **and**
tool cases observed through the validator's mock tool endpoint.

Raw numbers: `docs/v5-harness-replay.json`.

## Result — the ship gate holds

| harness | v4-view (ungated mean) | **v5 composite** | tool | memory | conv-sanity |
|---|---|---|---|---|---|
| **gggggggg v3** (scored champion, `81266c17`) | 0.86 | **0.42** | 0.91 | 0.81 | **0.00** |
| **pandas** (ATH-pending, F1rst-lineage clone, `2b260c75`) | 0.91 | **0.45** | 0.98 | 0.84 | **0.00** |
| mnemo-v6 (scored, pure-LLM, `d2a307a4`) | 0.18 | 0.09 | 0.21 | 0.16 | 0.00 |

"v4-view" is the ungated `0.5·tool + 0.5·memory` — what a v4 contract (no conversational
gate) would score, ≈ these harnesses' v4 composite. Every current top harness drops from
**~0.86–0.91 to ~0.42–0.45 on v5**, all **well below the 0.85 champion line** (4.11
necessary + target tiers both met for the top cohort).

## Why they drop — the phrase-list-router signature, confirmed

The drop is entirely the conversational-sanity gate: `conversational_sanity = 0` for
every champion triggers the factor's 0.5 floor (verified: gggggggg's post-gate composite
0.4218 / pre-gate 0.8605 = **0.490** gate). The drop is *specific*, not a blanket
difficulty hike — the counter-guard holds:

| category | gggggggg | pandas | reading |
|---|---|---|---|
| `single-session-recall` | 1.00 | 1.00 | strong ordinary recall |
| `multi-session` | 0.80 | — | strong retrieval |
| `memory-write-read` (EXPLICIT save verb) | **1.00** | **1.00** | captures explicit "remember/save" |
| `declarative-write-read` (NO save verb) | **0.00** | **0.00** | never captures a plain statement |
| `declarative-behavior` (apply the preference) | **0.00** | **0.00** | can't apply what it never stored |
| `conversational-chitchat` (greeting non-leak) | 0.50 | 1.00 | leaks stored values on a greeting |
| `injection-resistance` | 1.00 | 1.00 | resists injection (retrieval-as-data) |

The tell is unambiguous: both champions **ace the explicit-save-verb lifecycle (1.00)**
yet score **exactly 0.00 on the no-save-verb declarative write** — they recognize memory
writes only through the closed cue family and ignore ordinary declarative statements,
exactly the v4 exploit v5's 4.1/4.2 workstreams target.

Concrete "Aurora-9" evidence (gggggggg per-case notes):
- greeting → `"answer dump: surfaced 4 off-answer self values (scored 0)"` — a plain
  greeting force-routed into recall, dumping stored memory.
- declarative statement → `"abstained on an answerable question (scored 0)"` — the user
  states a fact; the router doesn't capture it and later abstains.

mnemo-v6 (a "pure-LLM" harness with no retrieval stack) is simply a weak harness across
the board (memory 0.16), low on v4 and v5 alike.

## How the run was optimized

Each case is a real Qwen3-32B call (~13s with reasoning), so throughput is the
constraint. Two env knobs (no code change) drove ~4× speedup and parallel harnesses:
- `DITTOBENCH_CASE_CONCURRENCY=8` — cases run concurrently within a run (wave barrier
  preserved).
- `DITTOBENCH_MAX_CONCURRENT_RUNS=3` — all three harnesses scored in parallel.

All three full runs (206 cases each) completed together in **~5 minutes** vs ~2+ hours
sequential, no rate limits.

## Local-run fixes (this session)

Two blockers were fixed to score a containerized harness via `harness_url` locally:
- The v5 screened-image contract wrongly gated `harness_url` direct runs, which never
  build source — exempted in local-dev mode (`allowPrivate`).
- The tool endpoint bound dual-stack `[::]`, unreachable from a container via Docker
  Desktop's IPv4 host-gateway — forced IPv4, plus a `DITTOBENCH_TOOL_HOST` override so a
  containerized local harness can reach it.
