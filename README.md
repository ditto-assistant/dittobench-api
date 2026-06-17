# DittoBench Off-Chain Practice API

A self-contained practice validator for **Bittensor SN118** (the Ditto subnet).
It mirrors the on-chain run+score loop **without TAO or the blockchain** so
miners can iterate on their agent harness locally.

On the live subnet, miners submit a Go agent harness and validators run it in a
Docker sandbox, scoring it on **DittoBench** (tool-calling correctness, token
cost, wall-clock). This service reproduces that loop with a **small, freshly
randomized dataset per request** — no two evaluations are identical, so you
can't overfit the practice set.

## Scope

Practice covers **tool-calling correctness + speed only**. Memory store /
embedding recall is part of the *full* on-chain evaluation and is intentionally
**not** included here. This keeps the practice service laptop-runnable.

| Dimension              | Off-chain practice (this repo) | On-chain validator |
| ---------------------- | ------------------------------ | ------------------ |
| Tool-calling accuracy  | ✅                             | ✅                 |
| Wall-clock / latency   | ✅                             | ✅                 |
| Memory / embeddings    | ❌                             | ✅                 |
| Dataset size           | small (default 30)             | large              |
| TAO / chain            | ❌                             | ✅                 |

## How it fits with the starter kit

The companion repo [`dittobench-starter-kit`](.) defines the **miner harness**:
an HTTP server exposing `POST /run` (`RunRequest` → `RunResponse`) and
`GET /health`. You build your harness there, run it (`serve`), grab its URL,
then point this API at it.

```
miner harness (starter-kit)              this API (practice validator)
  POST /run  <───────────────────────────  RunHarness over a fresh dataset
  GET  /health                              health-check + score + store
```

## Run it

```sh
go run ./cmd/dittobench-api          # listens on :8000 (use -port to change)
```

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
The full practice loop: health-check the harness, generate a **fresh random
dataset (rotating seed)**, run the harness over it, score, store, and return a
summary. Runs synchronously for v1 (small `n`).
```sh
curl -X POST localhost:8000/v1/submit \
  -H 'Content-Type: application/json' \
  -d '{"harness_url":"http://localhost:9000","n":30}'
# {"run_id":"...","composite":0.93,"tool_mean":0.93,"median_ms":42,"n":30,"seed":...}
```

### `GET /v1/runs/{id}`
Fetch the full stored `ScoreReport` (per-case breakdown). 404 if unknown.
```sh
curl localhost:8000/v1/runs/<run_id>
```

## Scoring

Per case: `matched / total_expected`, minus `0.1` per unexpected extra call
(unless the case allows extras), clamped to `[0, 1]`. No-expected-tool cases
score `1.0` only if the harness called nothing. The composite is the mean tool
score; latency is reported as the median across cases.

## Anti-overfit note

Every `/v1/submit` (and `/v1/dataset` without a pinned seed) rotates the seed,
so the dataset is fresh each time. Memorizing answers doesn't help — only a
genuinely correct tool-routing harness scores well. The authoritative, larger
evaluation (including memory) lives on the **on-chain** subnet validator.

## See also

- `PROTOCOL.md` — the shared wire contract (dataset, `/run`, score report).

---
Proprietary — Ditto Assistant.
