# Harness model lock (v2)

Every miner harness is scored against one locked open-weight model, Qwen3-32B,
served in a Trusted Execution Environment (TEE) via `cmd/model-relay` fronting
Chutes (`Qwen/Qwen3-32B-TEE`). The model id is frozen in code (`internal/llm`),
not an env var: it is consensus-critical, so every validator scores against the
same model. A single locked model limits the exploit surface.

## Why

If each validator scores a harness against whatever model the harness chooses:

1. Scores are not comparable. The median of three independent validators scoring
   one submission only means something if the three ran the same model. Different
   models add noise the median cannot remove.
2. Model choice is an attack surface. A miner can route to a bigger model or
   exploit a provider quirk to lift its score without improving the harness.

Locking to one open-weight model removes both. The lock is Qwen3-32B, served by
Chutes in a TEE as `Qwen/Qwen3-32B-TEE`. Open weight also means the exact scoring
model is a public, reproducible fact, part of the auditability goal.

## The lock is the network, not an env var

Setting `DITTOBENCH_MODEL` inside the container only asks the harness politely;
an adversarial one can ignore it. The enforceable lock has two parts:

1. Serve the locked model on the host gateway. `cmd/model-relay` fronting Chutes
   serves the locked Qwen3-32B chat model on the host. The sandbox reaches the
   host gateway at `http://host.docker.internal` via the `NO_PROXY` bypass (the
   same bypass carries embeddings from a local Ollama; the lock routes the chat
   model through the relay).
2. Admit only the locked gateway on the egress allowlist. `EGRESS_PROXY_ALLOW`
   is deny-by-default (empty admits nothing); under the lock it lists only the
   relay's upstream (`llm.chutes.ai`), reached from the relay process, never the
   sandbox. With the host firewall dropping all forward egress except the proxy +
   host gateway (see [Sandbox egress](#sandbox-egress) below), the host gateway is
   the ONLY reachable LLM. A harness cannot route to any other model; it fails
   closed.

Bonus: no provider key is forwarded into the sandbox at all under the lock, so
the key-exfiltration threat (see [Sandbox egress](#sandbox-egress)) and the
BYOK-spend concern both disappear on the locked path.

This hard lock covers the sandbox path (`git_url`/`tarball_url`), where the
validator builds and runs the crate. The direct `harness_url` practice path runs
a harness the miner hosts, so the validator cannot force its model; practice
relies on the starter kit defaulting to the locked model (`qwen/qwen3-32b`), and
miners are told to keep it. Hard enforcement applies on-chain, where submissions
are built and run in the sandbox.

## Sandbox egress

On-chain submissions build and run an untrusted miner crate in a Docker sandbox,
so the container's egress is locked down independently of the model lock. The
container runs on an isolated `ditto-sandbox` network with a host firewall that
DROPs all forward egress except the host gateway and a CONNECT-only,
hostname-allowlisting forward proxy (`cmd/egress-proxy`). Enforcement is
fail-closed: a harness that ignores the proxy env and dials the internet
directly is dropped, surfacing as a scoring failure, never a silent full-egress
run.

This defends the sandbox against a malicious submission:

1. Key exfiltration. Under the model lock no OpenRouter (or provider) key is
   forwarded into the sandbox at all, so there is no key to steal on the locked
   path.
2. Eval-set exfiltration. The seeded haystack and eval cases cannot be shipped
   out to learn or game the dataset.
3. Call-home / attack-proxy. The sandbox cannot receive commands or be used to
   scan, DoS, or abuse third parties from our IP.

`--cap-drop ALL`, `--pids-limit`, `no-new-privileges`, and memory/CPU bounds
harden the container itself. The container config is env-driven
(`DITTOBENCH_SANDBOX_EGRESS_NETWORK`, `DITTOBENCH_SANDBOX_EGRESS_PROXY`,
`DITTOBENCH_SANDBOX_HARDEN`); the proxy allowlist and firewall are provisioned
on the validator host.

The full-run resource envelope is fixed at 3 GiB memory, 2 CPUs, 512 PIDs, and
a 512 MiB `noexec,nosuid,nodev` tmpfs at `/tmp`, with a read-only root
filesystem. Each scorer admits one full run at a time so the miner envelope
cannot multiply past host capacity. Resource-failure diagnostics are captured
before teardown and expose aggregate counters only.

## Enforcement in the engine

The engine forces the provider, model, and gateway on the sandbox and drops any
caller-supplied model or key. The locked values are applied after the request's
own environment, so a request cannot override them, and the run details report
the model that actually served the run.

## Config surface

The locked model id and the crate provider are both frozen in code
(`internal/llm` = `qwen/qwen3-32b`; `cmd/dittobench-api` provider = `chutes`, the
OpenAI-compatible path the whole fleet uses so scores stay comparable). The
remaining env is deployment config: where the gateway serves that model.

| Env | Default | Effect |
|-----|---------|--------|
| `HARNESS_GATEWAY_URL` | `http://host.docker.internal:11434` | the CHAT gateway base URL |
| `HARNESS_EMBED_URL` | `http://host.docker.internal:11434` | trusted scorer-to-Ollama upstream; scored harnesses receive the source-bound operation broker |
| `DITTOBENCH_EMBEDDING_UPSTREAM_URL` | `http://host.docker.internal:11434/api/embed` | broker-only upstream for locked `embeddinggemma` embedding calls |

Scoring is judge-free (see `docs/judge-determinism.md`), so the locked model is
the ONLY model in a run. The locked keys also cover the Chutes and OpenAI
provider selectors (`CHUTES_API_KEY`, `CHUTES_BASE_URL`, `OPENAI_API_KEY`,
`OPENAI_BASE_URL`), so a crate that supports those providers cannot route
around the lock either.

## Gateway backend

The gateway is `cmd/model-relay` fronting Chutes. The relay terminates the
sandbox's requests locally, forces the model field to the locked id, injects the
operator's Chutes key, and forwards upstream (`Qwen/Qwen3-32B-TEE`,
hardware-attested TEE serving). The sandbox never holds the key and cannot choose
the model. The egress allowlist then admits only the relay's upstream from the
relay process, nothing from the sandbox.

Fronting one hosted model (rather than each validator self-hosting on its own
GPUs) keeps the whole fleet on the same serving backend, so the k=3 validators'
scores of a submission stay comparable. No GPU is required.
