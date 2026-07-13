# Harness model lock (v2)

Status: the current validation setup enforces the lock. Every miner harness is
scored against ONE locked open-weight model, Qwen3-32B, served in a Trusted
Execution Environment via `cmd/model-relay` fronting Chutes
(`Qwen/Qwen3-32B-TEE`). The `DITTOBENCH_MODEL_LOCK` switch ships off by default
in code and is set on for the deployed validators. Decision by Nick + Dan
(2026-07-09): a single locked model limits the exploit surface.

## Why

If each validator scores a harness against whatever model the harness chooses:

1. Scores are not comparable. Median-of-3 (three distinct validators scoring
   one submission, platform takes the median) only means something if the three
   ran the same model. Different models => noise the median cannot remove.
2. Model choice is an attack surface. A miner can route to a bigger model, a
   model the judge shares a family with (self-preference), or a provider quirk.

Locking to one open-weight model removes both. The lock is Qwen3-32B (strong
open-weight tool-calling; Chutes serves it in a TEE as `Qwen/Qwen3-32B-TEE`).
Open weight also means the exact scoring model is a public, reproducible fact,
part of the auditability goal.

## The lock is the network, not an env var

`DITTOBENCH_HARNESS_MODEL` only sets `DITTOBENCH_MODEL` inside the container,
which an adversarial harness can ignore. The enforceable lock has two parts:

1. Serve the locked model on the host gateway. An OpenAI-compatible endpoint
   (Ollama/vLLM) on the host serves the locked Qwen3-32B model. The sandbox already
   reaches the host gateway at `OLLAMA_BASE_URL=http://host.docker.internal:11434`
   via the `NO_PROXY` bypass (today it serves embeddings only; the lock adds the
   chat model).
2. Hard-drop openrouter.ai from the egress allowlist. With
   `EGRESS_PROXY_ALLOW` no longer listing openrouter, and the host firewall
   dropping all forward egress except the proxy + host gateway
   (see sandbox-egress-hardening.md), the host gateway is the ONLY reachable
   LLM. A harness cannot route to any other model; it fails closed.

Bonus: no OpenRouter key is forwarded into the sandbox at all under the lock, so
the key-exfiltration threat (sandbox-egress-hardening.md threat #1) and the
BYOK-spend concern both disappear on the locked path.

This hard lock covers the sandbox path (`git_url`/`tarball_url`), where the
validator builds and runs the crate. The direct `harness_url` practice path runs
a harness the miner hosts, so the validator cannot force its model; practice
relies on the starter kit defaulting to the locked model (`qwen/qwen3-32b`), and
miners are told to keep it. Hard enforcement applies on-chain, where submissions
are built and run in the sandbox.

## In-repo plumbing (landed)

- `internal/llm.HarnessModel()`: the model-routing indirection. Returns the
  locked model id (`HARNESS_MODEL` env or the Qwen3-32B default). ONE place to bump
  for v3.
- `cmd/dittobench-api` `harnessSandboxEnv(apiKey, reqEnv)`: builds the sandbox
  env. Under the lock it forces provider/model/gateway and drops the OpenRouter
  key; the locked keys (`lockedEnvKeys`) are applied AFTER the caller-supplied
  `req.Env` and any caller attempt to set them is discarded, so `req.Env` can
  never override the lock. `RunDetails.Models.Harness` reports the locked model.
- Gated by `DITTOBENCH_MODEL_LOCK` (default off, matching the egress-hardening
  phase-1 pattern) so nothing breaks until the gateway + firewall are provisioned.

## Config surface

| Env | Default | Effect |
|-----|---------|--------|
| `DITTOBENCH_MODEL_LOCK` | `false` | master switch for the lock |
| `HARNESS_MODEL` | `qwen/qwen3-32b` | the locked model id (must match what the gateway serves) |
| `HARNESS_PROVIDER` | `ollama` | the crate provider value pointing at the host gateway (`chutes` for the relay) |
| `HARNESS_GATEWAY_URL` | `http://host.docker.internal:11434` | the CHAT gateway base URL |
| `HARNESS_EMBED_URL` | _(the gateway URL)_ | the embeddings Ollama, when it differs from the chat gateway (relay setups) |

Scoring is judge-free (see `docs/judge-determinism.md`), so the locked model is
the ONLY model in a run. The locked keys also cover the Chutes and OpenAI
provider selectors (`CHUTES_API_KEY`, `CHUTES_BASE_URL`, `OPENAI_API_KEY`,
`OPENAI_BASE_URL`), so a crate that supports those providers cannot route
around the lock either.

## Gateway backends

The gateway can be anything OpenAI-compatible that serves exactly the locked
model:

- Local Ollama or vLLM on the validator's GPUs. Qwen3-32B at Q4_K_M is about
  20 GB, one 24 GB card.
- `cmd/model-relay` fronting Chutes, for a GPU-less validator. The relay
  terminates the sandbox's requests locally, forces the model field to the
  locked id, injects the operator's Chutes key, and forwards upstream
  (`Qwen/Qwen3-32B-TEE`, hardware-attested TEE serving). The sandbox never
  holds the key and cannot choose the model, so the lock's semantics are
  unchanged. The egress allowlist then admits only the relay's upstream from
  the relay process, nothing from the sandbox.

## Local-gateway alternative (GPU validators)

A validator can serve the locked model on its own GPUs instead of the Chutes
relay. Open item for that path: confirm the miner crate's provider contract for
a local gateway. The crate (the miner SDK, outside this repo) must honor
`DITTOBENCH_PROVIDER=<gateway>` + `OLLAMA_BASE_URL`/base-url to route chat at the
host gateway. The deployed relay path uses `DITTOBENCH_PROVIDER=chutes`; a local
gateway needs the crate's OpenAI-compatible provider string verified and
`HARNESS_PROVIDER` set to match.

Setup:

1. Run the host gateway (Ollama/vLLM) serving the Qwen3-32B model on
   `:11434` (OpenAI-compatible).
2. Remove `openrouter.ai` from `EGRESS_PROXY_ALLOW` (drop to deny-all internet;
   the gateway is reached via `NO_PROXY` + the firewall host-gateway allowance).
3. Set `DITTOBENCH_MODEL_LOCK=1`, `HARNESS_MODEL`, `HARNESS_PROVIDER`,
   `HARNESS_GATEWAY_URL` on the validator service.
