# DittoBench Off-Chain Practice API

> **Hosted practice validator for Bittensor SN118 (the Ditto subnet).** It
> generates a **fresh anti-cheat dataset per submission** (procedural tool
> cases + a procedural persona memory haystack), seeds your harness, runs every
> case, and scores it **deterministically** — mirroring the on-chain run+score
> loop **without TAO or the blockchain** so miners can iterate.

**Official practice endpoint:** `https://dittobench-api-22790208601.us-central1.run.app` (see below).

**No API key needed.** Generation is non-LLM and scoring is judge-free (the
deterministic grader in the public
[`dittobench-datagen`](https://github.com/ditto-assistant/dittobench-datagen)
module), so a practice run against a reachable `harness_url` requires no
credentials at all. Your harness brings whatever model access it needs.

On the live subnet, miners submit an agent harness and validators run it in a
Docker sandbox, scoring it on **DittoBench**. This service reproduces that loop
with a **freshly randomized dataset per request** — no two evaluations are
identical, so you can't overfit or build a lookup table against the practice set.

## Scope

The hosted practice API covers **tool-calling correctness + memory recall + tool
efficiency**. Crate **building** is the on-chain validator's job (the hosted
service has no Docker daemon) — to practice, expose a reachable harness and
submit its URL.

| Dimension              | Off-chain practice (this repo) | On-chain validator |
| ---------------------- | ------------------------------ | ------------------ |
| Tool-calling accuracy  | ✅                             | ✅                 |
| Tool efficiency        | ✅ (observed cases)            | ✅                 |
| Latency (measured)     | ✅ (reported)                  | ✅                 |
| Memory / embeddings    | ✅ (`run_size`)                | ✅                 |
| Fresh anti-cheat data  | ✅                             | ✅                 |
| Crate Docker build     | ❌ (use `harness_url`)         | ✅                 |
| TAO / chain            | ❌                             | ✅                 |

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
  GET  /health                              health-check + score
```

## Run it

```sh
go run ./cmd/dittobench-api          # listens on :8000 (or $PORT; -port to change)
```

The binary is self-contained: dataset generation, execution, and grading all
run locally with no external services.

## Deploy (Cloud Run)

The service is stateless and self-contained, so a source deploy is enough:

```sh
gcloud run deploy dittobench-api \
  --source . --project ditto-app-dev --region us-central1 \
  --allow-unauthenticated \
  --memory 1Gi --cpu 1 --timeout 3600
```

**CI/CD** (`.github/workflows/ci.yml`): every PR runs `build` / `vet` / `test`;
a **merge to `main` auto-deploys** to Cloud Run. CI authenticates to GCP via the
org's Workload Identity Federation (the same provider/SA the backend uses) — no
secrets stored in the repo.

No secrets are configured on the service and none are accepted per request for
practice. The repo stays **private**; the deployed URL is public so miners can
practice. The `git_url` Docker-build path is intentionally inert here (Cloud Run
has no Docker daemon).

> **The on-chain scoring path runs this same binary elsewhere.** The Cloud Run
> deployment above is the *practice* endpoint (`harness_url` only). The subnet
> validator co-locates a second instance on a **Docker-capable host** (a VM, not
> Cloud Run) so the `git_url` / `tarball_url` build-and-score path is live
> there — that is the deployment miners are actually graded on.

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
score, store, and return. Modes:

**Direct** — you run your own harness, the API just scores it (synchronous):
```sh
curl -X POST localhost:8000/v1/submit \
  -H 'Content-Type: application/json' \
  -d '{"harness_url":"http://localhost:9000","n":30}'
# {"run_id":"...","status":"done","composite":0.93,"tool_mean":0.93,"median_ms":42,"n":30,"seed":...}
```

**Sandbox** — the API builds your submission in Docker and runs it, closer to
the on-chain validator (asynchronous; build is slow). Returns `202` + a
`run_id`; poll `GET /v1/runs/{id}`. Under the model lock the container reaches
only the locked gateway; on the legacy path `env` is forwarded to the container
(model + keys the harness reads):
```sh
curl -X POST localhost:8000/v1/submit \
  -H 'Content-Type: application/json' \
  -d '{"git_url":"https://github.com/<you>/<harness>","git_ref":"main","n":30,
       "env":{"OPENROUTER_API_KEY":"sk-or-...","DITTOBENCH_MODEL":"openai/gpt-5.4-nano"}}'
# {"run_id":"...","status":"queued","poll":"/v1/runs/..."}
```

**Full pipeline (`run_size`)** — the complete SN118 evaluation: generate a
fresh anti-cheat dataset (procedural tool cases + persona memory haystack),
push the haystack to the harness's `POST /seed`, run every tool + memory case,
grade deterministically, and aggregate. No key needed. Target a reachable
`harness_url` (hosted), or `git_url` for the Docker-build path (local/on-chain
only). Asynchronous; returns `202` + a `run_id`. Poll `GET /v1/runs/{id}` for
`queued → building → generating → seeding → running → scoring → done`/`failed`,
with live `progress` + `partial` per-case scores.

```sh
curl -X POST https://dittobench-api-22790208601.us-central1.run.app/v1/submit \
  -H 'Content-Type: application/json' \
  -d '{"harness_url":"https://<your-harness>","run_size":"small"}'
  # small | medium | full ; "seed":N pins the dataset
# {"run_id":"...","status":"queued","poll":"/v1/runs/..."}
```

| run_size | tool cases | memory cases | seeding waves | raw-pairs frac | isolation |
| -------- | ---------- | ------------ | ------------- | -------------- | --------- |
| small    | 6          | 6            | 1             | 0              | 0         |
| medium   | 20         | 20           | 2             | 0.3            | 2         |
| full     | 60         | 50           | 2             | 0.35           | 4         |

**Config** for the `run_size` path (all optional):

| Source                  | Default            | Purpose                                              |
| ----------------------- | ------------------ | ---------------------------------------------------- |
| `openrouter_key` (body) | _(unset)_          | legacy Docker path only: forwarded to the crate's agent when the model lock is off |
| `DITTOBENCH_MODEL_LOCK` | `false`            | score every harness against the locked model (docs/model-lock.md) |
| `HARNESS_MODEL` env     | `qwen/qwen3-32b`   | the locked model id (must name what the gateway serves) |

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

Scoring is fully deterministic: a pure function of (dataset, transcript),
reproducible by anyone from the public `dittobench-datagen` module. See
`docs/judge-determinism.md` for the full grading rules.

**Tool cases**: deterministic trajectory + argument accuracy
(0.4 name-F1 + 0.4 arg-F1 + 0.2 order and extra-call discipline), scored on the
validator-observed trajectory; an observable case the harness didn't
execute through the tool endpoint is capped at 0.5. Result-usage cases also
require the served needle value in the answer.

**Memory cases** (`run_size` only): per-`answer_kind` deterministic grading
(value, number, list, ordered list, duration, reversal, decline) with
distractor zeroing, over the response's answer slot with prose fallback.

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
- `docs/judge-determinism.md` — why scoring is judge-free and how each case
  kind is graded.
- `docs/model-lock.md` — the locked harness model and gateway backends
  (local Ollama/vLLM or `cmd/model-relay` fronting Chutes).

## Independent validators

This is the same scoring engine a subnet validator runs, published so any
validator can score submissions itself and third parties can verify the composite
(no central scorer). See the subnet's
[`VALIDATOR-ONBOARDING.md`](https://github.com/ditto-assistant/ditto-subnet/blob/main/docs/VALIDATOR-ONBOARDING.md)
for how it slots into the validator worker.

## License

MIT — see [`LICENSE`](LICENSE).
