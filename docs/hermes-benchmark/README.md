# Hermes Agent on DittoBench v6

This directory records a full, fresh-seed DittoBench v6 measurement of the
unmodified native memory engine in Nous Research Hermes Agent. The adapter is
under [`integrations/hermes`](../../integrations/hermes/README.md); the
machine-readable, sanitized result is in
[`results/2026-07-22-hermes-openrouter-v6.json`](results/2026-07-22-hermes-openrouter-v6.json).

## Result

| Metric | Hermes |
| --- | ---: |
| Composite | **0.241189** |
| Tool mean | 0.862424 |
| Memory mean | **0.239583** |
| Composite standard error | 0.011398 |
| Median case latency | 8,410 ms |
| Metamorphic consistency | 0.818182 |
| Tool efficiency | 1.0 |
| Conversational sanity | 0.0 |

The memory result is the useful comparison. Hermes remained competent at
ordinary tool routing, so the low memory score is not explained by a generally
broken tool loop. Its strongest memory categories were curated/declarative
writes and benign stored instructions, while native FTS recall scored poorly on
single-session recall (0.20), temporal depth (0.0), multi-hop relations (0.0),
multi-session recall (0.0), lifecycle write/read (0.20), and isolation recall
(0.0). It also failed every injection-resistance case.

At the 2026-07-22T07:55:17Z public v6 leaderboard snapshot, all 20 finalized
miners had a higher memory mean: the range was 0.500000–0.937500 and the median
was 0.732299. The leader, `killer-4`, had a 0.937500 memory mean, 3.91 times the
Hermes result.

That leaderboard comparison is contextual, not a controlled paired test. The
miner values are three-validator production medians served by the locked Chutes
backend and use independent on-chain seeds. Hermes is one local practice-scope
run on OpenRouter automatic routing. A causal claim about code alone still
requires replaying a specific miner artifact and Hermes on the same seeds and
provider route. The present result does establish a large observed separation
on the same public v6 contract.

## Exact contract

- DittoBench API: `a0a0bca0460620cb5dcd381b1802a04567a6c762`
- DittoBench datagen: `55343ff961e36ebaa1c7db378dd7e5e0663ff5f3`
- adapter source used for the image: `d952e0a9e38df9898bfa8d6ce4b9df5f59c7a61a`
- Hermes Agent: `e57918ac800121cf9c2956fe55e27df3ea80b562`
  (`0.19.0`)
- benchmark: v6, `full`, 206 cases (110 tool and 96 memory)
- seed: `3058240546919425205`
- dataset SHA-256: `f9b8e6eea10f20316a6fc82c2f6b0af0f0cf0bc3f263cf96a760e68cdaa3ea2d`
- transcript SHA-256: `0e263ad754b7fdec35d196d44d3cfcf571c7b2a2f1465a25413d6a9d1c30b00b`
- model: `qwen/qwen3-32b`, thinking disabled, non-streaming
- route: OpenRouter automatic routing; 404 responses from SiliconFlow and one
  from DeepInfra
- run: `712a8c3c-860c-4962-ac07-0d2e83a57b59`, completed in 11m33s

The host-side relay kept the OpenRouter key out of the container and recorded
409 requests, 2,001,492 tokens, four upstream errors, and $0.286825 provider
cost. Practice scope cannot bind trusted relay accounting into the score, so
the v5+ token-efficiency multiplier remained neutral. The memory mean itself is
pure deterministic accuracy and is unaffected by that limitation.

## Adapter boundary

Hermes documents two memory layers: small curated `MEMORY.md`/`USER.md` files
and full session history searched through SQLite FTS5. Importing hundreds of
benchmark pairs into the curated files would evaluate an adapter-written
summarizer, not Hermes. The adapter therefore:

- imports each benchmark conversation as a native Hermes session;
- maps the benchmark memory-read tools to native `session_search` without
  embeddings, reranking, a graph index, or answer extraction;
- uses one physical Hermes database per benchmark user;
- forwards every non-memory tool to the validator-observed endpoint;
- keeps evaluation questions out of seeded history;
- accepts subjects and links without inventing support Hermes does not have;
- does not reinterpret lifecycle tool calls as edits to imported history.

Those unsupported capabilities are product differences DittoBench is intended
to expose. Claims should say “Hermes Agent 0.19.0 native session history via
this adapter,” not “all possible Hermes memory providers.”

## Validation and excluded attempt

Validation included eight adapter unit tests, an unprivileged read-only Docker
smoke, native FTS cross-user isolation, observed tool execution through the
validator callback, and complete `go test ./...` runs in both DittoBench repos.

One earlier run (`6967bd8c-3a76-4a42-9c84-49153dc708df`) was cancelled and is
not scored. It exposed two adapter transport defects: cases were unnecessarily
serialized behind a per-user lock, and Hermes requested SSE while the
benchmark-style relay returned non-streaming JSON. Both defects were fixed and
live-smoked before the fresh-seed run above. The cancelled run's seed and
partial results were not reused.

The raw anti-cheat dataset and transcript are intentionally not committed.
Their content hashes are sufficient to bind the local evidence without
publishing fresh evaluation material.
