# DittoBench Wire Protocol

This is the shared contract between the **practice validator** (this repo) and
the **miner harness** (`dittobench-starter-kit`). The Go types live in the public
`github.com/ditto-assistant/dittobench-datagen/protocol` module and must match the
starter kit **byte-for-byte**.

All payloads are JSON over HTTP.

## Dataset

The validator generates a `Dataset` of tool-calling cases. The harness never
receives expected answers, only the prompt and the tool catalog.

```jsonc
// Dataset
{
  "seed": 1718500000000000000,
  "generated_at": "2026-06-16T00:00:00Z",
  "tool_cases": [ /* ToolCase */ ]
}
```

```jsonc
// ToolCase
{
  "id": "web_search-1718...-0003",
  "category": "web_search",
  "prompt": "What's the latest on quantum computing?",
  "expected_tools": [ { "name": "search_web" } ],  // hidden from harness at run time
  "max_tool_calls": 1,
  "allow_extra_tools": false,
  "expected_behavior": "call search_web exactly once"
}
```

```jsonc
// ToolSpec
{
  "name": "search_web",
  "required_args": { "query": "string" },   // optional
  "forbidden_args": ["foo"]                  // optional
}
```

## `GET /health` (harness)

Returns any 2xx to signal readiness. Probed before each evaluation.

## `POST /run` (harness)

The validator sends one `RunRequest` per case; the harness returns a
`RunResponse`.

```jsonc
// RunRequest
{
  "case_id": "web_search-1718...-0003",
  "system_prompt": "You are Ditto, ...",
  "user_input": "What's the latest on quantum computing?",
  "tools": [ /* ToolDefinition */ ],
  "tool_endpoint": "http://host.docker.internal:49207/tool", // optional (observed tool execution); see below
  "user_id": "miner"                                         // optional: memory graph to answer from
}
```

```jsonc
// ToolDefinition
{
  "name": "search_web",
  "description": "Search the public web for current information.",
  "parameters": { "type": "object", "properties": { "query": { "type": "string" } }, "required": ["query"] }
}
```

```jsonc
// RunResponse
{
  "final_text": "Here's what I found...",
  "tool_calls": [
    { "name": "search_web", "args": { "query": "quantum computing" }, "hop": 0 }
  ],
  "prompt_tokens": 320,
  "output_tokens": 64,
  "latency_ms": 42,         // ignored; the validator measures latency itself
  "answer": "Lisbon",       // optional: the bare value final_text asserts.
                            // The deterministic grader matches this slot when
                            // present and falls back to final_text containment.
  "abstain": false          // optional: a grounded decline (the asked fact is
                            // not in memory). Correct on needle-absent cases;
                            // abstaining on an answerable case scores 0.
}
```

```jsonc
// ObservedToolCall
{ "name": "search_web", "args": { /* raw JSON */ }, "hop": 0 }
```

## Observed tool execution (`bench_version` 2)

Two optional `RunRequest` fields let the validator **observe** what a harness
actually does, instead of trusting its self-reported `tool_calls`.

**`tool_endpoint`**: a validator-served mock tool-execution URL. A harness that
supports observed execution should EXECUTE each non-memory catalog tool call by
POSTing a `ToolExecRequest` to this URL and using the returned
`ToolExecResponse.result`, rather than stubbing the tool locally. Doing so lets
the validator (a) score the **observed** trajectory (self-report is untrusted),
and (b) check that the answer **incorporates the returned content**: some cases
ask for a value that exists *only* in the served result (a fabricated per-seed
number), so it cannot be answered without executing the tool. **Memory tools**
(`search_memories`, `search_subjects`, `fetch_memories`,
`search_memories_in_subjects`) are NOT served here; answer those from your own
seeded memory. The field is **additive-optional**: a harness that ignores it
still scores, but selection-only and at a **capped ceiling (0.5)** on the
categories the endpoint would have served.

```jsonc
// ToolExecRequest  (harness → validator tool_endpoint)
{ "case_id": "web_search-…-0003", "user_id": "miner", "name": "search_web",
  "args": { "query": "…" }, "hop": 0 }

// ToolExecResponse (validator → harness)
{ "result": "Top result from Torva Daily: the Veltrix index reached 3,418 points. …" }
// or, for a tool this endpoint does not serve:
{ "error": "tool not available via this endpoint: search_memories" }
```

**`user_id`**: the memory graph the case must be answered from. The haystack is
seeded per user (`SeedRequest.user_id`); some runs seed a **second** persona
under a different `user_id`, and isolation cases query one user while the other
holds a conflicting value. A harness must answer only from the requested user's
memory and never leak another user's facts.

Old harnesses that ignore both fields keep working (scored selection-only, capped
on affected tool categories).

## Score report

After running every case, the validator produces a `ScoreReport`.

```jsonc
// ScoreReport
{
  "run_id": "uuid",
  "generated_at": "2026-06-16T00:00:00Z",
  "composite": 0.93,
  "tool_mean": 0.93,
  "median_ms": 42,
  "n": 30,
  "per_case": [ /* CaseScore */ ],
  // Advisory anti-copy metadata (omitted on the local harness_url path). An
  // AST-level shingle MinHash sketch of the built crate: the *shape* of the
  // parse tree, never identifier/literal text, so it survives renaming +
  // reformatting. The validator forwards it, UNSIGNED, to the platform's
  // anti-copy gate as one signal among several (see "Anti-copy signals"
  // below); it never affects the score computed here. Byte-compatible with the
  // platform's own fingerprint sketch (v: format version, k: bottom-k budget,
  // card: true shingle count, m: sorted bottom-k shingle hashes).
  "structural_fingerprint": { "v": 1, "k": 256, "card": 812, "m": ["0f1a…", "…"] }
}
```

```jsonc
// CaseScore
{
  "case_id": "web_search-1718...-0003",
  "category": "web_search",
  "tool_score": 1.0,
  "latency_ms": 42,
  "called": ["search_web"],
  "expected": ["search_web"],
  "notes": ["..."]    // optional
}
```

## Scoring rules

Grading is deterministic and judge-free.

Per tool case, scored on the trajectory the validator observed execute (a
self-reported trajectory is capped, since it proves nothing):

- `tool_score` = `0.4·name-F1 + 0.4·arg-F1 + 0.2·(order/extra-call discipline)`.
  name-F1 and arg-F1 score tool selection and argument grounding against the
  expected calls (both missing a needed call and making extra ones lose points);
  the last term scores call order and extra-call discipline, and its penalty
  scales with the call count.
- No-expected-tool cases score `1.0` iff the harness called nothing, else `0.0`.

Per memory case, graded by typed `answer_kind` (value, number, list, ordered
list, duration, activity, decline, and so on) with normalized matching against
the answer key. Surfacing a forbidden value (another user's fact, a decoy, or a
planted canary bait) or declining an answerable question scores the case `0`.

`composite = (0.5·tool_mean + 0.5·memory_mean)` scaled by three bounded integrity
factors, each `1.0` when it does not apply:

- **tool-efficiency**: penalizes overshooting the expected call budget on
  correctly-answered cases the validator watched execute through `tool_endpoint`.
- **canary-integrity**: a canary breach drops the composite. Echoing a planted
  bait value (a leak) multiplies by `0.5`; an honest miss by `0.85`; failed
  canaries compound.
- **metamorphic-consistency**: penalizes answering paraphrased twins of the same
  fact inconsistently.

`tool_mean`, `memory_mean`, and `per_category` stay pure accuracy; the factors
touch only the composite. `median_ms` is the median per-case latency, **measured
by the validator** (the `/run` round trip); a self-reported `latency_ms` is
ignored and latency stays out of the composite.

> This local scope is tool-calling accuracy + efficiency; latency is measured and
> reported but advisory. Memory recall and the memory/tool composite are scored by
> the full `run_size` pipeline (and the on-chain validator), not here.

Memory cases (full pipeline) are graded deterministically per `answer_kind`
(value, number, list, ordered list, duration, reversal, decline) against the
response's `answer` slot with `final_text` fallback, with distractor and
forbidden-value zeroing. There is no LLM judge anywhere in scoring; the grader
is the public `dittobench-datagen/grade` package, so any published transcript
can be re-graded offline. See `docs/judge-determinism.md`.

## Anti-copy signals

On-chain, the platform runs a duplicate-detection gate that compares each
uploaded crate against other miners' eligible submissions across exact bytes,
normalized source, and lexical and structural fingerprints. None of it runs in
this practice API, and none of it affects a score. This validator contributes
two inputs to that gate, both carried out-of-band and never folded into the
composite:

- **`structural_fingerprint`**: the AST-shape MinHash sketch above, forwarded
  UNSIGNED with the `ScoreReport`. It is the parse-tree shape only (no
  identifier or literal text), so reformatting and renaming do not change it.
- **Observed tool-call trajectory**: the ordered sequence of observed tool
  **names** per case (`CaseScore.called`), captured when a harness executes
  through `tool_endpoint` (see *Observed tool execution* above). Because it is
  what the agent *did* at runtime, not source text, it is a copy signal a
  source-level edit cannot forge; the platform's behavioral check compares these
  name-sequences on a shared dataset seed. Each call's full `(name, args, hop)`
  is recorded server-side during execution; the forwarded score report carries
  the per-case name order (`called`), not the arguments.

The gate holds exact/near-exact copies for review (the earlier upload wins by
first-seen) and requires agreement across independent signals before flagging
the softer similarity band, so independent convergence on the shared reference
harness is not penalized. The miner-facing summary is in the
[starter kit](https://github.com/ditto-assistant/dittobench-starter-kit) README
(*Mining on SN118 → Originality*).
