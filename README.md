# DittoBench Off-Chain Practice API (hosted, BYOK)

> **Hosted practice validator for Bittensor SN118 (the Ditto subnet).** It
> rotates a **fresh anti-cheat dataset per submission** (paraphrased tool cases +
> a freshly assembled LongMemEval memory haystack), seeds your harness, runs
> every case, and scores it with an LLM judge — mirroring the on-chain run+score
> loop **without TAO or the blockchain** so miners can iterate.

**Official practice endpoint:** `https://dittobench-api-22790208601.us-central1.run.app` (see below).

**Bring Your Own Key (BYOK).** The hosted API stores no credentials — every
scored submission carries **your** OpenRouter key, which the validator uses for
the generator (paraphrase) and the LLM judge. See [BYOK usage](#byok-usage).

On the live subnet, miners submit an agent harness and validators run it in a
Docker sandbox, scoring it on **DittoBench**. This service reproduces that loop
with a **freshly randomized dataset per request** — no two evaluations are
identical, so you can't overfit or build a lookup table against the practice set.

## Scope

The hosted practice API covers **tool-calling correctness + memory recall + tool
efficiency**, using a self-contained slim LongMemEval bundle baked into the
service. Crate **building** is the on-chain
validator's job (the hosted service has no Docker daemon) — to practice, expose
a reachable harness and submit its URL.

| Dimension              | Off-chain practice (this repo) | On-chain validator |
| ---------------------- | ------------------------------ | ------------------ |
| Tool-calling accuracy  | ✅                             | ✅                 |
| Tool efficiency        | ✅ (observed cases)            | ✅                 |
| Latency (measured)     | ✅ (reported)                  | ✅                 |
| Memory / embeddings    | ✅ (`run_size`)                | ✅                 |
| Fresh anti-cheat data  | ✅                             | ✅                 |
| Crate Docker build     | ❌ (use `harness_url`)         | ✅                 |
| TAO / chain            | ❌                             | ✅                 |

## BYOK usage

The `run_size` practice flow needs an OpenRouter key (generator + judge). Send
it **per request** — the server never stores it. Either:

- request body: `"openrouter_key": "sk-or-..."`, or
- header: `Authorization: Bearer sk-or-...`

```sh
curl -X POST https://dittobench-api-22790208601.us-central1.run.app/v1/submit \
  -H 'Content-Type: application/json' \
  -d '{"harness_url":"https://<your-reachable-harness>","run_size":"small",
       "openrouter_key":"sk-or-..."}'
# {"run_id":"...","status":"queued","poll":"/v1/runs/..."}
```

`harness_url` must be reachable from the hosted API (e.g. a deployed harness or
a tunnel like `ngrok http 9000`). Locally you can also run this API yourself and
point it at `http://localhost:9000` — see [Run it](#run-it).

## How it fits with the starter kit

The companion repo [`dittobench-starter-kit`](https://github.com/ditto-assistant/dittobench-starter-kit)
defines the **miner harness**: an HTTP server exposing `POST /run`
(`RunRequest` → `RunResponse`), `POST /seed` (load a fresh memory haystack), and
`GET /health`. You build your harness there, run it (`serve`), expose it, then
point this API at its URL.

```
miner harness (starter-kit)              this API (practice validator)
  POST /seed <───────────────────────────  install a fresh memory haystack
  POST /run  <───────────────────────────  run a fresh anti-cheat dataset
  GET  /health                              health-check + judge + score
```

## Run it

```sh
go run ./cmd/dittobench-api          # listens on :8000 (or $PORT; -port to change)
```

The binary is self-contained (LongMemEval seeds are embedded). For a `run_size`
practice run, supply a BYOK key per request (see [BYOK usage](#byok-usage)).

## Deploy (Cloud Run)

The service is stateless and self-contained, so a source deploy is enough:

```sh
gcloud run deploy dittobench-api \
  --source . --project ditto-app-dev --region us-central1 \
  --allow-unauthenticated \
  --memory 1Gi --cpu 1 --timeout 3600 \
  --set-env-vars GENERATOR_MODEL=google/gemini-3.1-flash-lite,SCORER_MODEL=google/gemini-3.1-flash-lite
```

(`GENERATOR_MODEL`/`SCORER_MODEL` are configurable — the code default generator
is `qwen/qwen3-32b`; the deploy above pins both to the cheaper
`gemini-3.1-flash-lite` validated end-to-end.)

**CI/CD** (`.github/workflows/ci.yml`): every PR runs `build` / `vet` / `test`;
a **merge to `main` auto-deploys** to Cloud Run. CI authenticates to GCP via the
org's Workload Identity Federation (the same provider/SA the backend uses) — no
secrets stored in the repo.

No secrets are configured on the service — miners bring their own OpenRouter key
per request (BYOK). The repo stays **private**; the deployed URL is public so
miners can practice. The `git_url` Docker-build path is intentionally inert here
(Cloud Run has no Docker daemon).

## Security (public endpoint hardening)

The hosted service is public + unauthenticated and dials caller-supplied
harness URLs, so it ships guards against abuse:

- **SSRF** — `harness_url` must be an `http(s)` URL resolving to a **public**
  address. Loopback, RFC1918 private, link-local (incl. the `169.254.169.254`
  metadata IP), CGNAT, and multicast are rejected up front (`internal/netguard`),
  and the outbound dialer re-checks the connected IP to defeat DNS-rebinding.
- **Rate limiting** — a per-IP sliding window on `/v1/submit`
  (`internal/ratelimit`) plus a global cap on concurrent `run_size` jobs; both
  return `429`. A request body cap rejects oversized payloads.
- **`DITTOBENCH_ALLOW_PRIVATE_HARNESS`** — set truthy for **local dev / the
  Docker sandbox** (loopback containers) to relax the SSRF guard. Leave unset in
  production; the guard is on by default.

## Endpoints

### `GET /health`
```sh
curl localhost:8000/health
# {"status":"ok"}
```

### `GET /v1/dataset?n=&seed=`
Pull a fresh randomized dataset to practice against. `seed` is random unless
pinned (pinning is only for reproducing a specific set); `n` defaults to 30.
```sh
curl 'localhost:8000/v1/dataset?n=10'
```

### `GET /v1/catalog`
The Ditto tool catalog the harness is given on every `/run`.
```sh
curl localhost:8000/v1/catalog
```

### `POST /v1/submit`
Generate a **fresh random dataset (rotating seed)**, run the harness over it,
score, store, and return. Two modes:

**Direct** — you run your own harness, the API just scores it (synchronous):
```sh
curl -X POST localhost:8000/v1/submit \
  -H 'Content-Type: application/json' \
  -d '{"harness_url":"http://localhost:9000","n":30}'
# {"run_id":"...","status":"done","composite":0.93,"tool_mean":0.93,"median_ms":42,"n":30,"seed":...}
```

**Sandbox** — the API builds your submission in Docker and runs it, closer to
the on-chain validator (asynchronous; build is slow). Returns `202` + a
`run_id`; poll `GET /v1/runs/{id}` for status (`queued → building → running →
scoring → done`/`failed`). `env` is forwarded to the container (model + keys
the harness reads):
```sh
curl -X POST localhost:8000/v1/submit \
  -H 'Content-Type: application/json' \
  -d '{"git_url":"https://github.com/<you>/<harness>","git_ref":"main","n":30,
       "env":{"OPENROUTER_API_KEY":"sk-or-...","DITTOBENCH_MODEL":"openai/gpt-5.4-nano"}}'
# {"run_id":"...","status":"queued","poll":"/v1/runs/..."}
```
**Full pipeline (`run_size`)** — the complete SN118 evaluation: generate a
**fresh anti-cheat dataset** (paraphrased tool cases + a freshly assembled
LongMemEval memory haystack), push the haystack to the harness's `POST /seed`,
run every tool + memory case, score with the deterministic tool-accuracy half
**plus an LLM judge** (tool response-quality + memory yes/no), and aggregate.
Requires a **BYOK OpenRouter key** per request (see [BYOK usage](#byok-usage)).
Target a reachable `harness_url` (hosted), or `git_url` for the Docker-build path
(local/on-chain only). Asynchronous; returns `202` + a `run_id`. Poll
`GET /v1/runs/{id}` for
`queued → building → generating → seeding → running → scoring → done`/`failed`,
with live `progress` + `partial` per-case scores.

```sh
# Hosted (BYOK): point at a reachable harness.
curl -X POST https://dittobench-api-22790208601.us-central1.run.app/v1/submit \
  -H 'Content-Type: application/json' \
  -d '{"harness_url":"https://<your-harness>","run_size":"small",
       "openrouter_key":"sk-or-..."}'   # small | medium | full ; "seed":N pins the dataset
# {"run_id":"...","status":"queued","poll":"/v1/runs/..."}
```

| run_size | tool cases | memory cases | distractor pairs | paraphrase frac |
| -------- | ---------- | ------------ | ---------------- | --------------- |
| small    | 6          | 6            | 20               | 0.3             |
| medium   | 20         | 20           | 100              | 0.5             |
| full     | 60         | 50           | 300              | 0.7             |

`small` is intentionally cheap (few LLM calls) for fast iteration.

**Key + config** for the `run_size` path:

| Source                | Default                         | Purpose                                       |
| --------------------- | ------------------------------- | --------------------------------------------- |
| `openrouter_key` (req)| _(required, BYOK)_              | generator + judge (per request; never stored) |
| `GENERATOR_MODEL` env | `qwen/qwen3-32b`                | paraphrases tool prompts + memory pairs       |
| `SCORER_MODEL` env    | `google/gemini-3.1-flash-lite`  | LLM judge (tool quality + memory yes/no)      |
| `SCORER_MODEL_B` env  | _(unset)_                       | optional second judge; audit-slice cross-check |
| `DITTOBENCH_SEED_DIR` env | _(embedded bundle)_         | override LongMemEval seeds with on-disk copies |
| `DITTOBENCH_ORACLE` env   | _(embedded bundle)_         | override the oracle with an on-disk copy       |

Keep `SCORER_MODEL` distinct from the miner's harness model: an LLM judge tends
to over-score responses from its own model family. Most of the score is
programmatic (tool trajectory/args, result-usage needle, isolation leak,
injection payload) and so is unaffected; only the LLM-judged half is exposed to
this bias. When the harness model is known (operator sets `DITTOBENCH_HARNESS_MODEL`),
the run warns if it matches the judge. `SCORER_MODEL_B` adds a de-correlated
second judge on an audit slice.

The LongMemEval corpus ships **embedded** in the binary (a slim, text-only
bundle under `internal/gen/seeddata/`, ~18 MB, embeddings stripped) so the
service is self-contained — no external files or `DITTOBENCH_*` env needed. Set
those env vars only to point at the full on-disk assets for local development.

### Crate build (on-chain only)

The `git_url` Docker-build mode (`internal/sandbox`) clones a submission, builds
it (a `gh_token` BuildKit secret authenticates the private `ditto-harness`
dependency — see [ditto-harness#1](https://github.com/ditto-assistant/ditto-harness/issues/1)),
runs the container, and scores it. This needs a Docker daemon, so it is **not
available on the hosted (Cloud Run) service** — it is the on-chain validator's
path. To practice against the hosted API, submit a reachable `harness_url`
instead.

### `GET /v1/runs/{id}`
Fetch the job: `status`, `mode`, and (when `done`) the full `ScoreReport` with
per-case breakdown. 404 if unknown.
```sh
curl localhost:8000/v1/runs/<run_id>
```

## Scoring

**Tool cases** (both modes): per case, `matched / total_expected` minus `0.1`
per unexpected extra call (unless the case allows extras), clamped to `[0, 1]`;
a no-expected-tool case scores `1.0` only if the harness called nothing. In the
full `run_size` pipeline each tool case also earns an LLM response-quality half
(`0.5·accuracy + 0.5·quality`).

**Memory cases** (`run_size` only): graded credit `0.7·correctness +
0.3·grounding`, resolved by a deterministic answer match where possible and an
LLM judge otherwise.

**Composite**: the mean tool score in direct mode; in the full pipeline
`0.5·tool_mean + 0.5·memory_mean`, then multiplied by an observed tool-efficiency
factor (`≤1`) that gently penalizes harnesses whose observed tool trajectories
overshoot the expected call budget on cases they answered correctly. Efficiency
applies only to cases the validator watched execute through the tool endpoint; a
remote harness that never routes through it is scored on accuracy alone.

**Latency** is measured by the validator (the `/run` round trip, never
self-reported) and reported as `median_ms`. It is advisory telemetry — accuracy
and efficiency drive the score, not wall-clock, which on a remote harness mostly
reflects network and hardware.

## Anti-overfit note

Every `/v1/submit` (and `/v1/dataset` without a pinned seed) rotates the seed,
so the dataset is fresh each time. Memorizing answers doesn't help — only a
genuinely correct tool-routing harness scores well. The authoritative, larger
evaluation (including memory) lives on the **on-chain** subnet validator.

## Anti-copy note

Duplicate detection is a **platform-side** gate, not part of any score computed
here. This service only forwards two inputs to it: the `structural_fingerprint`
sketch in the `ScoreReport` and the observed tool-call trajectory (see
`PROTOCOL.md` → *Anti-copy signals*). On-chain the gate compares uploads across
exact, normalized-source, lexical, structural, prompt, semantic-embedding, and
behavioral dimensions, holds copies for review (first-seen protects the
original), and requires agreement across independent signals before flagging
near-duplicates — so independent convergence on the shared reference harness is
not penalized.

## See also

- `PROTOCOL.md` — the shared wire contract (dataset, `/run`, score report).

---
Proprietary — Ditto Assistant.
