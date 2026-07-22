# Hermes Agent on DittoBench v6

This directory records two full DittoBench v6 measurements of Nous Research
Hermes Agent on the same seed and generated dataset:

- the original controlled baseline in
  [`results/2026-07-22-hermes-openrouter-v6.json`](results/2026-07-22-hermes-openrouter-v6.json);
- a separately labeled Hermes-favorable replay in
  [`results/2026-07-22-hermes-favorable-openrouter-v6.json`](results/2026-07-22-hermes-favorable-openrouter-v6.json).

The adapter is under [`integrations/hermes`](../../integrations/hermes/README.md).
The first result remains unchanged.

## Paired result

| Metric | Baseline | Hermes-favorable | Delta |
| --- | ---: | ---: | ---: |
| Composite | 0.241189 | **0.246898** | +0.005709 |
| Tool mean | 0.862424 | **0.884242** | +0.021818 |
| Memory mean | 0.239583 | **0.260417** | +0.020833 |
| Composite standard error | 0.011398 | 0.011159 | -0.000239 |
| Median case latency | 8,410 ms | 7,052 ms | -1,358 ms |
| Metamorphic consistency | 0.818182 | 0.727273 | -0.090909 |
| Tool efficiency | 1.0 | 0.999537 | -0.000463 |
| Conversational sanity | 0.0 | 0.0 | 0.0 |

Both runs used all 206 cases generated from seed `3058240546919425205`.
Their dataset SHA-256 is identical. The favorable profile produced five memory
case improvements and three regressions, for a net gain of two correct cases
out of 96. Single-session recall improved from 0.20 to 0.333333 and the canary
from 0 to 1; multi-query recall regressed from 0.50 to 0. The other memory
categories had no net change.

This is useful negative evidence against the claim that the original result was
mostly an artificially tight adapter configuration. The boost helped, but it
did not repair Hermes' missing graph, temporal, lifecycle, or defensive recall
semantics: temporal depth, multi-hop relations, multi-session recall, isolation
recall, injection resistance, and declarative write/read remained at 0. The
favorable memory mean was still below the 0.50 minimum in the recorded
20-miner public snapshot; the 0.9375 leader remained 3.6 times higher.

It is still one stochastic replay, not a confidence interval over repeated
model samples. The baseline relay recorded four upstream errors and four
timeout/no-response cases, while the favorable replay recorded no upstream
errors and one timeout/no-response case. The tool delta in particular should
not be attributed solely to the profile. Additional repeated paired runs would
be required to isolate sampling and transport variance.

## What the favorable profile changes

The `favorable` profile uses three general Hermes-facing changes:

- Hermes' documented default 90 agent-loop iterations instead of eight;
- up to 20 native `session_search` results instead of five;
- generic recall guidance adapted to the benchmark's memory-tool aliases.

It does **not** contain expected answers, case IDs, benchmark categories,
answer extraction, query rewriting, embeddings, reranking, a graph index, or a
benchmark-specific skill. Qwen still chose every tool name and argument through
the ordinary Hermes agent loop. Both profiles used the same model, disabled
thinking, non-streaming relay shape, public tool catalog, observed tool
endpoint, container sandbox, seed, and deterministic grader.

This is deliberately a generous **native SessionDB** profile, not a claim to
cover every third-party memory provider Hermes supports. Provider-assisted
variants should be reported as separate products because they introduce a new
retrieval backend, credentials, and data-processing behavior.

## Exact contract

Shared contract:

- DittoBench datagen: `55343ff961e36ebaa1c7db378dd7e5e0663ff5f3`
- Hermes Agent: `e57918ac800121cf9c2956fe55e27df3ea80b562`
  (`0.19.0`)
- benchmark: v6, `full`, 206 cases (110 tool and 96 memory)
- seed: `3058240546919425205`
- dataset SHA-256: `f9b8e6eea10f20316a6fc82c2f6b0af0f0cf0bc3f263cf96a760e68cdaa3ea2d`
- model: `qwen/qwen3-32b`, thinking disabled, non-streaming
- route: OpenRouter automatic routing

Baseline:

- DittoBench API: `a0a0bca0460620cb5dcd381b1802a04567a6c762`
- adapter source: `d952e0a9e38df9898bfa8d6ce4b9df5f59c7a61a`
- run: `712a8c3c-860c-4962-ac07-0d2e83a57b59`
- transcript SHA-256: `0e263ad754b7fdec35d196d44d3cfcf571c7b2a2f1465a25413d6a9d1c30b00b`
- providers: 404 SiliconFlow responses and one DeepInfra response
- relay: 409 requests, 2,001,492 tokens, four upstream errors, $0.286825
- elapsed: 11m33s

Hermes-favorable:

- DittoBench API and adapter profile: `609a01e435f53dea0922d5cc99c45f1cc1eec6da`
- run: `745a769b-0183-40f4-912b-155ac79dc1c5`
- transcript SHA-256: `bc74b821ad41e72e500d5aa4d8fd8d4d2ea79743599b03cb10202dd5eed9b821`
- provider: 439 SiliconFlow responses
- relay: 439 requests, 2,224,942 tokens, no upstream errors, $0.318771
- elapsed: 7m43s

The host-side relay kept the OpenRouter key out of both containers. Practice
scope cannot bind trusted relay accounting into the score, so the token
efficiency multiplier remained neutral. Memory accuracy is deterministically
graded and unaffected by that neutral multiplier.

## Adapter boundary

Hermes documents two native memory layers: small curated `MEMORY.md`/`USER.md`
files and full session history searched through SQLite FTS5. Importing hundreds
of benchmark pairs into the curated files would evaluate an adapter-written
summarizer rather than Hermes. Both profiles therefore:

- import each benchmark conversation as a native Hermes session;
- map benchmark memory-read aliases to native `session_search`;
- use one physical Hermes database per benchmark user;
- forward every non-memory tool to the validator-observed endpoint;
- keep evaluation questions out of seeded history;
- accept subjects and links without inventing graph support;
- do not reinterpret lifecycle tool calls as edits to imported history.

Claims should say “Hermes Agent 0.19.0 native SessionDB through this adapter,”
not “all possible Hermes memory providers.”

## Validation and excluded attempt

Validation included ten adapter unit tests, an unprivileged read-only Docker
smoke, native FTS cross-user isolation, live Qwen tool continuation through the
observed callback, and complete repository Go tests. The favorable run used the
same dataset hash as the baseline, and the committed aggregates were checked
against the retained local run, transcript, relay, and dataset artifacts.

One earlier run (`6967bd8c-3a76-4a42-9c84-49153dc708df`) remains excluded. It
was cancelled after exposing adapter transport defects that were fixed before
either published measurement. The raw anti-cheat datasets and transcripts are
not committed; content hashes bind the retained local evidence without
publishing fresh evaluation material.
