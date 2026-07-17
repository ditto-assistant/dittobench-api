# DittoBench scoring engine

DittoBench is the benchmark for Bittensor SN118, the Ditto subnet. This repo is
the engine that runs it: per submission it generates a fresh anti-cheat dataset
(procedural tool cases and a procedural persona memory haystack), runs a miner's
harness over every case, and scores the result deterministically.

The same binary runs in two places:

- On-chain, each subnet validator runs it to score miner submissions on its own
  hardware. See [What a validator does](#what-a-validator-does).
- Off-chain, the team hosts it as a keyless practice endpoint so miners can
  iterate without TAO or the chain. See [Off-chain practice](#off-chain-practice).

Generation is non-LLM and scoring is judge-free: a score is a pure function of
(dataset, transcript), graded by the deterministic checker in the public
[`dittobench-datagen`](https://github.com/ditto-assistant/dittobench-datagen)
module. Anyone can reproduce a composite from the dataset seed and the
transcript, so there is no central scorer to trust.

## What a validator does

A subnet validator scores miner submissions and sets on-chain weights from the
results. This repo is the scoring half of that job. Per submission it:

1. Loads the exact image built by the trusted screener when screened-image
   metadata is present. Legacy records fall back to building the crate in a
   Docker sandbox (`git_url` or `tarball_url`). The source tarball is still
   unpacked for structural anti-copy fingerprinting.
2. Generates a fresh randomized DittoBench dataset, so no two evaluations match
   and a memorized lookup table cannot score.
3. Seeds the harness, runs every tool and memory case, and grades each case
   deterministically.
4. Returns a `ScoreReport`: the composite, the per-case breakdown, and the
   anti-copy sketch the platform's duplicate gate consumes.

The validator worker in
[`ditto-subnet`](https://github.com/ditto-assistant/ditto-subnet) drives the
engine end to end: it leases a scoring ticket from the platform, runs this engine
on a Docker-capable host, submits the signed score to the public ledger, and
recomputes the deterministic weights it sets on-chain. Because scoring is
judge-free and reproducible from the seed, any validator or third party can
re-derive a composite independently; no validator has to trust another's number.

Loading or building an image needs a Docker daemon, so a validator runs the engine on a VM or
similar host, not on a daemon-less platform. Run it directly:

```sh
go run ./cmd/dittobench-api   # listens on :8000 (or $PORT; -port to override)
```

Or build the validator image, which adds the Docker CLI and Git to the same
statically linked API binary:

```sh
docker build --target sandbox -t dittobench-api:sandbox .
docker run --rm \
  --user 65532:65532 \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -p 127.0.0.1:8000:8000 \
  dittobench-api:sandbox
```

The image runs the API as a non-root UID. The supplemental group must match the
Docker socket's group on the host; in Compose, use `group_add` with that numeric
GID. Docker Desktop users may need the socket GID exposed inside its VM instead
of the macOS host's value.

> **Security:** mounting the raw Docker socket gives this service
> host-root-equivalent control even when its process is non-root. Use this image
> only on an isolated validator host, never on a shared or public application
> host. The resource and capability limits applied to miner containers do not
> reduce the API container's control of the daemon.

The binary is self-contained: dataset generation, execution, and grading run
locally with no external services. For how it slots into the worker, see the
subnet's
[`VALIDATOR-ONBOARDING.md`](https://github.com/ditto-assistant/ditto-subnet/blob/main/docs/VALIDATOR-ONBOARDING.md).

## Off-chain practice

The team hosts the same engine as a practice endpoint so miners can iterate
against a fresh dataset without the chain:

```
https://dittobench-api-22790208601.us-central1.run.app
```

Practice covers tool-calling correctness, memory recall, and tool efficiency. It
does not build crates (the hosted instance has no Docker daemon), so to practice
you expose a reachable harness and submit its URL. No API key is needed; your
harness brings whatever model access it uses. Every request rotates the dataset
seed, so you cannot overfit to the practice set. The authoritative evaluation is
the on-chain validator run.

| Dimension              | Off-chain practice | On-chain validator |
| ---------------------- | ------------------ | ------------------ |
| Tool-calling accuracy  | yes                | yes                |
| Tool efficiency        | yes (observed)     | yes                |
| Latency (measured)     | yes (reported)     | yes                |
| Memory / embeddings    | yes (`run_size`)   | yes                |
| Fresh anti-cheat data  | yes                | yes                |
| Crate Docker build     | no (use `harness_url`) | yes            |
| TAO / chain            | no                 | yes                |

## How it fits with the starter kit

The companion repo
[`dittobench-starter-kit`](https://github.com/ditto-assistant/dittobench-starter-kit)
defines the miner harness: an HTTP server exposing `POST /run` (`RunRequest` to
`RunResponse`), `POST /seed` (load a fresh memory haystack), and `GET /health`.
You build your harness there, run it, expose it, then point this engine at its
URL.

```
miner harness (starter-kit)              scoring engine (this repo)
  POST /seed <───────────────────────────  install a fresh memory haystack
  POST /run  <───────────────────────────  run a fresh anti-cheat dataset
  GET  /health                              health check + score
```

## Endpoints

### `GET /health`
```sh
curl localhost:8000/health
# {"status":"ok"}
```

### `GET /v1/dataset?n=&seed=`
Pull a fresh randomized dataset to practice against. `seed` is random unless
pinned (pinning only reproduces a specific set); `n` defaults to 30.
```sh
curl 'localhost:8000/v1/dataset?n=10'
```

### `GET /v1/catalog`
The Ditto tool catalog the harness receives on every `/run`.
```sh
curl localhost:8000/v1/catalog
```

### `POST /v1/submit`
Generate a fresh dataset (rotating seed), run the harness over it, score, store,
and return. Three modes:

**Direct**: you run your own harness and the engine scores it (synchronous):
```sh
curl -X POST localhost:8000/v1/submit \
  -H 'Content-Type: application/json' \
  -d '{"harness_url":"http://localhost:9000","n":30}'
# {"run_id":"...","status":"done","composite":0.93,"tool_mean":0.93,"median_ms":42,"n":30,"seed":...}
```

**Sandbox**: the engine builds your submission in Docker and runs it, matching
the on-chain path (asynchronous; the build is slow). Returns `202` and a
`run_id`; poll `GET /v1/runs/{id}`. The container reaches only the locked gateway;
any model or provider key in `env` is dropped, so you cannot route around the
locked model:
```sh
curl -X POST localhost:8000/v1/submit \
  -H 'Content-Type: application/json' \
  -d '{"git_url":"https://github.com/<you>/<harness>","git_ref":"main","n":30}'
# {"run_id":"...","status":"queued","poll":"/v1/runs/..."}
```

**Full pipeline (`run_size`)**: the complete SN118 evaluation. Generate a fresh
anti-cheat dataset, push the haystack to the harness's `POST /seed`, run every
tool and memory case, grade deterministically, and aggregate. No key needed.
Target a reachable `harness_url`, or `git_url` for the Docker-build path (local
or on-chain only). Asynchronous; returns `202` and a `run_id`. Poll
`GET /v1/runs/{id}` for `queued`, `building`, `generating`, `seeding`,
`running`, `scoring`, then `done` or `failed`, with live `progress` and
`partial` per-case scores.

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

Config for the `run_size` path (all optional):

| Source                  | Default            | Purpose                                              |
| ----------------------- | ------------------ | ---------------------------------------------------- |
| `HARNESS_GATEWAY_URL` env | `http://host.docker.internal:11434` | the chat gateway serving the locked model |
| `HARNESS_EMBED_URL` env | _(the gateway URL)_ | the embeddings Ollama, when it differs from the chat gateway |

The locked model is scored against every harness; its id and provider are frozen
in code (`internal/llm`, `cmd/dittobench-api`), not env-tunable
(`docs/model-lock.md`).

### Crate build (on-chain only)

The `git_url` Docker-build mode (`internal/sandbox`) clones a submission, builds
it (resolving its `ditto-harness` git dependency), runs the container, and scores
it. It needs a Docker daemon, so it is unavailable on the hosted practice
instance and is the validator's path. To practice against the hosted endpoint,
submit a reachable `harness_url` instead.

### `GET /v1/runs/{id}`
Fetch the job: `status`, `mode`, and (when `done`) the full `ScoreReport` with
the per-case breakdown. Returns `404` if unknown.
```sh
curl localhost:8000/v1/runs/<run_id>
```

Failed sandbox runs may also include a bounded `failure` object. Only
validator-owned resource exhaustion is marked `kind=validator_infrastructure`
with `retryable=true`; ordinary non-zero miner exits remain non-retryable. Its
diagnostics are limited to Docker state, exit code, OOM/cgroup counters, and
aggregate `/tmp` usage. Container IDs, environment, output, paths, and source
are never returned.

## Scoring

Scoring is deterministic: a pure function of (dataset, transcript), reproducible
by anyone from the public `dittobench-datagen` module. See
`docs/judge-determinism.md` for the grading rules.

Tool cases: deterministic trajectory and argument accuracy
(0.4 name-F1 + 0.4 arg-F1 + 0.2 order and extra-call discipline), scored on the
validator-observed trajectory. An observable case the harness did not execute
through the tool endpoint is capped at 0.5. Result-usage cases also require the
served needle value in the answer.

Memory cases (`run_size` only): per-`answer_kind` deterministic grading (value,
number, list, ordered list, duration, reversal, decline) with distractor
zeroing, over the response's answer slot with a prose fallback.

Composite: the mean tool score in direct mode; in the full pipeline
`0.5·tool_mean + 0.5·memory_mean`, then multiplied by three bounded integrity
factors (each at most 1): observed tool-efficiency (penalizes overshooting the
expected call budget on correctly-answered cases the validator watched execute
through the tool endpoint), canary-integrity (a canary leak multiplies by 0.5 and
compounds; an honest miss carries no composite penalty, since it is already
reflected in the case's own accuracy), and metamorphic-consistency (penalizes answering
paraphrased twins of the same fact inconsistently). tool_mean, memory_mean, and
per_category stay pure accuracy; the factors touch only the composite.

Latency is measured by the validator (the `/run` round trip, never
self-reported) and reported as `median_ms`. It is advisory telemetry: accuracy
and efficiency drive the score, not wall-clock, which on a remote harness mostly
reflects network and hardware.

## Anti-overfit

Every `/v1/submit` (and `/v1/dataset` without a pinned seed) rotates the seed, so
the dataset is fresh each time. Memorizing answers does not help; only a
genuinely correct harness scores well. The authoritative, larger evaluation lives
on the on-chain validator.

## Anti-copy

Duplicate detection is a platform-side gate, not part of any score computed here.
This engine forwards two inputs to it: the `structural_fingerprint` sketch in the
`ScoreReport` and the observed tool-call trajectory (see `PROTOCOL.md`, Anti-copy
signals). The gate holds a suspected copy for review, with first-seen protecting
the original author, and requires agreement across independent signals before
flagging, so independent convergence on the shared reference harness is not
penalized.

## Public endpoint hardening

The hosted practice instance is public, unauthenticated, and dials
caller-supplied harness URLs, so it guards against abuse:

- SSRF: `harness_url` must be an `http(s)` URL resolving to a public address.
  Loopback, RFC1918 private, link-local (including the `169.254.169.254` metadata
  IP), CGNAT, and multicast are rejected up front (`internal/netguard`), and the
  outbound dialer re-checks the connected IP to defeat DNS rebinding.
- Rate limiting: a per-IP sliding window on `/v1/submit` (`internal/ratelimit`)
  plus a single active `run_size` job per scorer; both return `429`. A full
  miner container is capped at 3 GiB RAM, 2 CPUs, 512 PIDs, and a 512 MiB
  writable `/tmp`; the root filesystem remains read-only. Keeping concurrency
  at one prevents those limits from overcommitting the validator's documented
  16 GiB host alongside Ollama, Docker, the relay, Pylon, and the worker. A
  request-body cap rejects oversized payloads.
- `DITTOBENCH_ALLOW_PRIVATE_HARNESS`: set truthy for local dev or the Docker
  sandbox (loopback containers) to relax the SSRF guard. Leave it unset in
  production; the guard is on by default.
- Screened-image metadata pins integrity but is not an authentication token.
  The prebuilt-image path is therefore rejected on the public practice API and
  is only enabled on validator-owned sandbox deployments by the narrow
  `DITTOBENCH_ALLOW_SCREENED_IMAGES=1` opt-in. The validator must keep that API
  private. `DITTOBENCH_ALLOW_PRIVATE_HARNESS` remains separate and is only
  needed when local source/image URLs themselves resolve to private addresses.
  Imported archive and local runner tags are removed after each run so validator
  disks do not accumulate submission images.
- Validators set `DITTOBENCH_REQUIRE_SCREENED_IMAGE=1`. This removes the
  source-build fallback from the untrusted path: only the screener-built,
  digest- and image-ID-bound archive may run. Practice deployments may leave
  it unset when they intentionally build their own submissions.
- Miner containers run as an unprivileged UID with a read-only root filesystem,
  an ephemeral no-exec `/tmp`, all capabilities dropped, no-new-privileges,
  bounded CPU/memory/PIDs/file descriptors, and request-scoped cleanup. Set
  `DITTOBENCH_SANDBOX_SECCOMP_PROFILE` and
  `DITTOBENCH_SANDBOX_APPARMOR_PROFILE` to reviewed host profiles where those
  Linux security modules are available.

## See also

- `PROTOCOL.md`: the shared wire contract (dataset, `/run`, score report).
- `docs/judge-determinism.md`: why scoring is judge-free and how each case kind
  is graded.
- `docs/model-lock.md`: the locked harness model and the gateway
  (`cmd/model-relay` fronting Chutes).

## License

MIT. See [`LICENSE`](LICENSE).
