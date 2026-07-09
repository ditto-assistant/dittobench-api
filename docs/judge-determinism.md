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

## Why temperature 0 is necessary but not sufficient

Temperature 0 makes decoding greedy, but greedy is only reproducible if the logits
are bit-identical run to run. On a hosted, multi-tenant, continuously batched model
(the current default, `google/gemini-3.1-flash-lite` via OpenRouter) they are not:
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

## Further hardening (not yet implemented)

JSON-schema or structured output. The judge is prompted to emit JSON and parsed by
a tolerant hand parser (`internal/scorer/judge.go`). Adding `response_format`
(OpenAI and vLLM `guided_json`, Ollama `format`) would constrain the judge to
exactly the verdict fields, remove formatting variance, and narrow the token set at
each position. This is gated on confirming the serving stack supports it, since the
hosted default may reject an unknown `response_format`.

Wider deterministic rubric. Convert borderline judged cases into stable
exact-matches with alias and normalization checks before the judge fallback (the
`scorer.go` memory-miss path), which shrinks the judged surface further.

Residual-disagreement measurement. Use the `SCORER_MODEL_B` audit slice to log
verdict agreement across two runs or two validators, and confirm the residual k=3
disagreement is actually gone once the judge is self-hosted.

## Bottom line

`temperature 0 + top_p 1 + seed` is now on every judge request, so the judge is
reproducible the moment it runs on a serving stack that honors those knobs. On the
hosted default it is best-effort. The determinism guarantee is realized by
self-hosting the judge (`LLM_BASE_URL`) on a pinned vLLM or Ollama stack.
