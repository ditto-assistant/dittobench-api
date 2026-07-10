# Judge determinism (v2)

The scorer's composite is partly LLM-judged (see the breakdown below). Under the
k=3 median model, three distinct validators grade the same submission and the
platform takes the median composite. That median is only meaningful if the three
judges agree, so judge non-determinism shows up directly as k=3 score
disagreement. This note covers what we send to make the judge reproducible, why
temperature 0 alone is not enough, and how to run a judge that actually
reproduces.

## How much of the score is judged

Grading is deterministic-first. The judge is a fallback and a quality signal, not
the primary grader.

Tool cases: result-usage tool cases are fully deterministic and never call the
judge (the answer must carry a fabricated needle value only the executed tool
could reveal). Every other tool case composite is
`0.5*tool_accuracy + 0.5*quality`, where `tool_accuracy` is deterministic (name-F1,
arg-F1, trajectory) and `quality` is the LLM judge. So half of a non-result-usage
tool case is judged.

Memory cases: a deterministic containment and forbidden-answer check resolves
first, and on a hit the judge is skipped. The judge runs only when that check
misses, plus always for abstention and isolation cases. When judged the case is
`0.7*correct + 0.3*grounded`.

The judge is therefore load-bearing near category boundaries: near-miss answers,
abstention, and isolation. That is exactly where a token-level flip changes the
score, so pinning it matters.

## What we send

Every request from `internal/llm.Client.Complete` carries, on the wire:

- `temperature: 0` for greedy argmax decoding.
- `top_p: 1` so the full vocabulary is considered before the argmax and a provider
  default (for example 0.9) cannot silently truncate the distribution first.
- `seed: 42`, a fixed consensus constant (`deterministicSeed` in
  `internal/llm/llm.go`). It is deliberately not configurable, because every
  validator must send the same seed for their judges to reproduce one another.
  Changing it is a scoring change and follows the bench-version bump policy.

All three are always emitted (no `omitempty`) so intent is explicit and a default
can never reintroduce sampling.

Judge calls additionally send `response_format: {"type":"json_object"}` when
JSON mode is on, constraining the verdict to exactly a JSON object and removing
formatting variance (one less place a token-level flip can change a score).
JSON mode defaults on when `LLM_BASE_URL` is set (vLLM and Ollama's
OpenAI-compatible endpoints both support it) and off on the hosted default,
which may reject the field; `LLM_RESPONSE_FORMAT=json_object|off` overrides
either way. Generator calls are free text and never send it
(`llm.Client.CompleteJSON` vs `Complete`).

## Which model judges

`SCORER_MODEL` defaults to the locked open-weight harness model
(`llm.ScorerModel` falls back to `llm.HarnessModel`, `qwen/qwen2.5-72b-instruct`
for v2): one frozen model, one source constant, no closed-model version drift.
The exact judge is a public, reproducible fact. Same-family self-preference
bias does not threaten the ranking under the model lock, because every miner's
harness runs the identical model and a uniform bias is a constant offset; the
de-correlated cross-check is `SCORER_MODEL_B` on the audit slice. Without the
lock (legacy BYOK), pick a judge from a different family than the harness
model; the run warns when they match.

## Why temperature 0 is necessary but not sufficient

Temperature 0 makes decoding greedy, but greedy is only reproducible if the logits
are bit-identical run to run. On a hosted, multi-tenant, continuously batched
serving stack (any model reached via OpenRouter, including the hosted practice
deploy's `gemini-3.1-flash-lite` override) they are not:
batch composition, kernel and hardware routing, and silent model-version bumps all
perturb the logits enough to flip the argmax at a near-tie token boundary. The
hosted route may also ignore `seed` entirely. So on the hosted default the knobs
above are best-effort, and residual k=3 disagreement is expected.

Full reproducibility needs a serving stack we control. Point the judge at a
self-hosted OpenAI-compatible gateway with `LLM_BASE_URL` and pin the stack:

```
# self-hosted judge (OpenAI-compatible gateway: vLLM or Ollama)
LLM_BASE_URL=http://host.docker.internal:11434/v1/chat/completions
SCORER_MODEL=qwen2.5:72b-instruct       # must name what the gateway serves
OPENROUTER_API_KEY=local                 # any non-empty token; local gateways ignore it
```

The client sends the same OpenAI Chat Completions shape to any base URL, so both
vLLM and Ollama's OpenAI-compatible endpoint work without a code change.

### vLLM vs Ollama for reproducibility

Both honor a top-level `seed`. Greedy plus a fixed seed reproduces only if the
numerics are stable, which means pinning the batching and parallelism too.

For vLLM, run `--enforce-eager` (disables CUDA graphs), fix the tensor-parallel
size, and keep batches stable (serialize the judge, or cap `--max-num-seqs`).
Continuous batching under variable concurrent load otherwise changes the reduction
order and flips the argmax at ties.

For Ollama, set `options.seed` and `options.temperature: 0`, and pin `num_gpu`,
`num_thread`, and `num_ctx`. Thread count and GPU-layer split change the reduction
order. Ollama is the easiest to make bit-stable on a single pinned host at low
concurrency, which suits a judge.

Recommendation: Ollama on one pinned host at low concurrency is the simplest
reproducible judge. Use vLLM if throughput demands it, with eager mode and fixed
batching. Run the same locked open-weight model the harness is locked to, or a
dedicated judge model, so the exact judge is a public, reproducible fact.

## Residual-disagreement measurement

When `SCORER_MODEL_B` is set, the audit slice (~1 in 5 judged cases) runs both
judges and records whether they agreed: any correct/grounded flip counts as a
memory disagreement, and a quality gap of ≥ 0.2 (a two-point swing on one 1–5
dimension) counts as a tool disagreement (`scorer.JudgeOutcome`,
`toolAuditDisagreeDelta`). The run loop logs `judge audit slice: X/Y
disagreement(s)` per run. This is the live measure of the judge noise the k=3
median is exposed to; it should trend to ~0 once the judge is self-hosted, and
a persistent nonzero rate on a pinned stack means the rubric (not the serving)
is the noise source. An errored second judge is an outage, not a disagreement,
and is excluded. The counts ride the wire as `RunDetails.judge_audited` /
`judge_disagreed` (dittobench-datagen ≥ v0.3.0), so the platform and the public
leaderboard can display per-run judge agreement alongside the score.

## Further hardening (not yet implemented)

Wider deterministic rubric. Convert borderline judged cases into stable
exact-matches with alias and normalization checks before the judge fallback (the
`scorer.go` memory-miss path), which shrinks the judged surface further.

JSON *schema* (beyond JSON mode). `json_object` constrains the shape, not the
fields. vLLM `guided_json` / Ollama `format` with the exact verdict schema would
also narrow the token set at each position.

## Bottom line

`temperature 0 + top_p 1 + seed` is on every judge request, judge calls run in
JSON mode on a self-hosted stack, and the default judge is the locked
open-weight harness model. The judge is therefore a frozen, public artifact that
is reproducible the moment it runs on a serving stack that honors those knobs
(self-hosted `LLM_BASE_URL` on a pinned vLLM or Ollama). Routed through a
hosted provider it is best-effort, and the `SCORER_MODEL_B` disagreement log
tells you how far from deterministic it actually is.
