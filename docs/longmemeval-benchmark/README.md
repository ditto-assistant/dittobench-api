# Top five DittoBench miners on LongMemEval-S

This directory records two LongMemEval-S measurements of the five
highest-ranked finalized DittoBench v6 submissions in the public leaderboard
snapshot at `2026-07-23T05:37:19.615917Z`:

- the preferred fair condition is frozen in
  [`results/2026-07-23-top-five-longmemeval-s-native-tools-gpt41.json`](results/2026-07-23-top-five-longmemeval-s-native-tools-gpt41.json);
- the first-pass Qwen/no-tool ablation remains frozen in
  [`results/2026-07-23-top-five-longmemeval-s.json`](results/2026-07-23-top-five-longmemeval-s.json).

These are third-party research measurements, not DittoBench submissions,
validator scores, KOTH candidates, weights, or payout inputs.

## Results

| DittoBench v6 rank | Agent | First pass | Fair condition | Change | Fair empty answers |
| ---: | --- | ---: | ---: | ---: | ---: |
| 1 | whitycatboss v1 | 101/500 (0.202) | 166/500 (0.332) | +0.130 | 0 |
| 2 | killer-4 v2 | 265/500 (0.530) | 338/500 (0.676) | +0.146 | 0 |
| 3 | ditto-max-v1 v1 | 288/500 (0.576) | **350/500 (0.700)** | +0.124 | 0 |
| 4 | avadakedavra v1 | 31/500 (0.062) | 347/500 (0.694) | **+0.632** | 2 |
| 5 | zeus_v12 v1 | 178/500 (0.356) | 188/500 (0.376) | +0.020 | 0 |

The preferred condition raises every submission's score. Most importantly, the
rank-4 submission's 476 empty first-pass answers fall to two. The rank-3
submission still leads, but rank 4 is now within 0.006 rather than appearing to
have almost no memory capability.

LongMemEval is an end-to-end memory-system benchmark. Each value measures the
submission's memory implementation, native retrieval policy, and shared reader
model together. The improvement cannot be attributed causally to either the
tool catalog or reader change alone because the fair condition intentionally
fixes both first-pass disadvantages.

## Why the first pass was too harsh

The first pass sent `tools: []`, although a real DittoBench memory case
advertises four memory-reading tools. Some harnesses perform retrieval
internally anyway, but the empty catalog can suppress a submission's intended
agent loop.

The shared Qwen route also imposed a reader/context failure on the rank-4
submission. Its native retrieval path assembled at least 26,215 prompt tokens
while its harness reserved 14,746 output tokens. The combined 40,961-token
request exceeded that route's 40,960-token limit by one token, and the harness
returned an empty answer. A canary with the native tool catalog but the same
Qwen reader reproduced the failure. A GPT-4.1 canary produced five non-empty
answers for every submission, so the full fair rerun used that reader uniformly.

The first-pass aggregate is retained as an ablation and evidence of this failure
mode; it is no longer presented as the best estimate of native memory quality.

## Fair adapter boundary

The adapter uses the public DittoBench harness protocol without modifying a
submission:

- each LongMemEval question gets a fresh isolated `user_id` and its complete
  timestamped conversation history through `/seed`;
- every distinct non-empty user and assistant turn is retained in chronological
  dataset order, including assistant-first, user-only, and same-role-adjacent
  turns; reused pair identities resolve deterministically to their final
  occurrence, matching the public harness's upsert semantics;
- answer labels, answer-session IDs, question types, and `has_answer` flags never
  cross the harness boundary;
- `/run` receives the question, question date, and the exact memory-reading
  subset of the public catalog: `search_memories`, `search_subjects`,
  `fetch_memories`, and `search_memories_in_subjects`; descriptions, required
  fields, and array-valued argument schemas match the pinned public catalog;
- the submission chooses its own native storage, embeddings, retrieval, tool
  loop, and final answer path;
- no sidecar retriever, external vector database, reranker, query rewriter,
  answer labels, or benchmark-specific skill is added.

Some submissions execute native retrieval inside the harness rather than
returning it as outer-protocol `tool_calls`. Therefore a zero outer call count is
not evidence that the memory path was unused; the scored output is the complete
native harness response.

All five images used `openai/gpt-4.1` through the same model-pinning OpenRouter
proxy. It permits only OpenRouter's OpenAI provider, disables fallbacks and
provider data collection, and rejects any other model or endpoint. Only the
public LongMemEval dataset was sent to this reader.

The fair condition retains each harness's native 768-dimensional
`embeddinggemma` path. An OpenRouter-backed embedding experiment is tracked
separately in [issue #77](https://github.com/ditto-assistant/dittobench-api/issues/77)
because changing the embedding model or transport would be a distinct condition.

## Official evaluation

Hypotheses use LongMemEval's official `question_id` / `hypothesis` JSONL shape.
The unmodified official evaluator grades all 500 answers with its frozen
`gpt-4o-2024-08-06` prompt and label logic. A narrow proxy maps only that model
identifier to `openai/gpt-4o-2024-08-06` on OpenRouter; it rejects other models
and endpoints.

The committed JSON files contain only aggregates, public agent names, immutable
artifact/image hashes, and hashes of the retained local hypothesis and judge
files. Raw miner source, generated answers, reference labels, and evaluator
transcripts remain local.

## Exact contract

- dataset: cleaned LongMemEval-S, 500 questions
- dataset SHA-256:
  `d6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442`
- Hugging Face dataset revision:
  `98d7416c24c778c2fee6e6f3006e7a073259d48f`
- LongMemEval source revision:
  `9e0b455f4ef0e2ab8f2e582289761153549043fc`
- public DittoBench harness revision:
  `c3caa8e2c19f8a41a0610b9f7db774f97643dd9c`
- public tool-catalog dependency revision: `ef3af0387b46`
- preferred condition: `longmemeval-s-cleaned-native-memory-tools-v2`
- preferred answer model profile: `longmemeval-openrouter-gpt41-reader-v1`
- first-pass condition: `longmemeval-s-cleaned-native-dittobench-memory-v1`
- first-pass answer model profile: `openrouter-nebius-qwen3-32b-no-think-v1`
- official judge profile: `longmemeval-official-gpt4o-openrouter-v1`

## Validation and interruption handling

Validation covers irregular-role conversion, removal of contentless records,
last-write-wins normalization for reused pair identities, reference-label
exclusion, isolated resume namespaces, pair-ID namespacing, exact public tool
schemas, reader and judge model locking, exact 500-ID cardinality, duplicate-ID
rejection, and category aggregation.

The adapter streams the 265 MB dataset and keeps selected rows compact until a
worker needs them. A local full-selection probe reduced peak coordinator memory
from about 2.93 GB to 0.92 GB without changing decoded entries. Completed
hypotheses were never rerun. After an interruption, only unfinished questions
were reseeded under fresh user and pair-ID namespaces, then mechanically
redistributed across otherwise identical frozen-image shards. Scheduling and
container-memory changes cannot alter a question's history, model, tools, or
scored answer contract.

No partial aggregate is published. The result generator rejects missing,
duplicate, unexpected, or non-officially-judged question records. Native empty
answers are reported explicitly; they are not retried or relabeled into a more
favorable result.
