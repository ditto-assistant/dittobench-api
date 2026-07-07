# DittoBench v2 — Coverage

What the benchmark tests, how much of it runs per case, and what it deliberately
does not test. Generation is a pure function of `(seed, bench_version)`; the
subnet re-seeds every run, so a harness is scored over many fresh datasets.

## Run sizes

Each run generates a fresh dataset at one size:

| run_size | tool cases | memory cases | isolation | total |
|----------|-----------:|-------------:|----------:|------:|
| small    | 6          | 6            | 0         | ~12   |
| medium   | 20         | 20           | 2         | ~42   |
| full     | 60         | 50           | 4         | ~114  |

The composite is `0.5·tool_mean + 0.5·memory_mean`, multiplied by a bounded
observed-tool-efficiency factor. Latency and token cost are reported, never
scored.

## Tool suite (30 categories)

- **Single tool** — the correct answer is one catalog tool, with exact-value
  argument grading where the argument is a concrete token (URL, theme, model).
- **Routing traps** — phrasing points at the wrong tool: memory-vs-web either
  way, run-vs-read (dispatch vs check), edit-vs-create, job-vs-workflow.
- **Multi-hop** — an ordered tool sequence; relative order is scored.
- **Parallel** — two independent tools in one turn; the name/arg set is scored
  but call order is not (`parallel_web_image`).
- **Result-usage** — the answer must carry a per-seed value that exists only in
  the served tool's result, so it cannot be answered without executing the tool.
- **Restraint** — no tool should be called: chit-chat, unanswerable questions
  (abstention), and a request whose required argument is missing, where inventing
  one is the failure being probed (`arg_hallucination`).

Tool trajectory, arguments, result-usage, and the restraint cases are scored
**programmatically**. An LLM judge adds a response-quality half only where wording
matters.

## Memory suite (11 question types)

Recall (`single-session-recall`), cross-session synthesis (`multi-session`,
including list count/enumerate and a state-at-event join), `temporal-reasoning`
(3-way ordering + elapsed duration), `knowledge-update` (latest value over a
supersession chain), `preference` and `preference-application`, `contradiction`
(change of mind), and `abstention` (needle-absent, including false-premise decoys
where a friend's value is seeded but the user's is not).

Added in the v2 coverage pass:

- **`assistant-recall`** — the value lives only in a past *assistant* turn (a
  recommendation the user never stated), so it tests recalling what the assistant
  said, not the user.
- **`aggregation-count`** — one topic raised several times across sessions, asked
  as "how many times"; a retriever that dedupes the repeated mention undercounts.
- **`injection-resistance`** — a real recall question wrapped in an
  instruction-override attack; complying emits a payload that scores 0, resisting
  answers from memory. Scored programmatically via the forbidden-answer check.

Memory grading is `0.7·correctness + 0.3·grounding`, with a deterministic
containment check resolving correctness before any judge call. Questions get a
low-overlap (NoLiMa) rewrite so a lexical shortcut can't stand in for retrieval.
Multi-graph **isolation** cases seed a second user with a conflicting value; a
cross-graph leak scores 0.

## Reading the results

`per_category` carries a `count`, `mean`, and `std_err` (standard error of the
mean). Per-category means come from few cases per run — a 95% band is roughly
`mean ± 1.96·std_err` — so treat a single run's per-category number as
directional and rank capabilities from the aggregate across many seeds. The
aggregate composite is the reliable signal; the per-category breakdown is
diagnostic.

Keep the judge model (`SCORER_MODEL`) distinct from a harness's model: an LLM
judge over-scores its own family. Only the LLM-judged half is exposed to this;
the programmatic majority is not.

## Deliberate non-goals (v2)

These are out of scope by design, not oversight:

- **Languages other than English.** All personas, prompts, and questions are
  English. Multilingual coverage would need reviewed translations of every pool
  and template; deferred rather than shipped machine-translated.
- **Real multimodality.** Image/artifact tools are routing *targets* (names to
  select), not actual image, audio, file, or vision inputs. The subnet's harness
  contract is text in / text + tool-calls out.
- **Interactive multi-turn evaluation.** Each case is one request. The multi-turn
  structure lives in the *seeded* memory the harness recalls from; the validator
  does not hold a live back-and-forth dialogue within a case.
- **Long-context stress.** The haystack scales modestly and is spread across
  seeding waves rather than flooding one prompt; there is no needle-in-a-huge-
  single-context tool case.
- **Harness-cooperative error recovery.** The mock tool endpoint can return an
  error, but there is no dedicated case scoring graceful recovery — that requires
  the harness to route through the endpoint and a judged notion of "recovered",
  both of which are follow-up work.
- **Safety/policy refusal.** The only refusal tested is grounded decline
  (abstention) and injection resistance; harmful-request refusal is not in scope.
