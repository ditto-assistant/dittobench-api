# DittoBench Wire Protocol

This is the shared contract between the **practice validator** (this repo) and
the **miner harness** (`dittobench-starter-kit`). The Go types live in
`pkg/protocol/protocol.go` and must match the starter kit **byte-for-byte**.

All payloads are JSON over HTTP.

## Dataset

The validator generates a `Dataset` of tool-calling cases. The harness never
receives expected answers — only the prompt and the tool catalog.

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
  "tools": [ /* ToolDefinition */ ]
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
  "latency_ms": 42
}
```

```jsonc
// ObservedToolCall
{ "name": "search_web", "args": { /* raw JSON */ }, "hop": 0 }
```

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
  "per_case": [ /* CaseScore */ ]
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

- `matched` = Σ over expected tools of `min(expected_count, observed_count)`.
- `base` = `matched / total_expected`.
- `penalty` = `0.1` per unexpected/extra call (skipped if `allow_extra_tools`).
- `tool_score` = `clamp(base - penalty, 0, 1)`.
- No-expected-tool cases score `1.0` iff the harness called nothing, else `0.0`.
- `composite` = mean `tool_score`; `median_ms` = median per-case latency.

> Practice scope is tool-calling + speed only. Token cost and memory recall are
> scored by the on-chain validator, not here.
