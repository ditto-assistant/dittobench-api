# Harness model lock (v2)

**Status: in-repo plumbing landed, env-gated OFF by default; enforcement
(host gateway serving the locked model + dropping openrouter.ai from the egress
allowlist) pending infra + one crate-contract confirmation.** Decision by Nick +
Dan (2026-07-09): v2 scores every miner harness against ONE locked open-weight
model to "limit the surface area for exploits" (Dan).

## Why

If each validator scores a harness against whatever model the harness chooses:

1. **Scores are not comparable.** Median-of-3 (three distinct validators scoring
   one submission, platform takes the median) only means something if the three
   ran the same model. Different models => noise the median cannot remove.
2. **Model choice is an attack surface.** A miner can route to a bigger model, a
   model the judge shares a family with (self-preference), or a provider quirk.

Locking to one open-weight model removes both. v2 starts with the **Qwen2.5
family** (strong open-weight tool-calling). "Open weight" also means the exact
scoring model is itself a public, reproducible fact — part of the auditability
goal (see the scoring-decentralization brief).

## The lock is the network, not an env var

`DITTOBENCH_HARNESS_MODEL` only sets `DITTOBENCH_MODEL` inside the container,
which an adversarial harness can ignore. The enforceable lock has two parts:

1. **Serve the locked model on the host gateway.** An OpenAI-compatible endpoint
   (Ollama/vLLM) on the host serves the Qwen2.5 model. The sandbox already
   reaches the host gateway at `OLLAMA_BASE_URL=http://host.docker.internal:11434`
   via the `NO_PROXY` bypass (today it serves embeddings only; the lock adds the
   chat model).
2. **Hard-drop openrouter.ai from the egress allowlist.** With
   `EGRESS_PROXY_ALLOW` no longer listing openrouter, and the host firewall
   dropping all forward egress except the proxy + host gateway
   (see sandbox-egress-hardening.md), the host gateway is the ONLY reachable
   LLM. A harness cannot route to any other model — it fails closed.

Bonus: no OpenRouter key is forwarded into the sandbox at all under the lock, so
the key-exfiltration threat (sandbox-egress-hardening.md threat #1) and the
BYOK-spend concern both disappear on the locked path.

## In-repo plumbing (landed)

- `internal/llm.HarnessModel()` — the model-routing indirection. Returns the
  locked model id (`HARNESS_MODEL` env or the Qwen2.5 default). ONE place to bump
  for v3.
- `cmd/dittobench-api` `harnessSandboxEnv(apiKey, reqEnv)` — builds the sandbox
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
| `HARNESS_MODEL` | `qwen/qwen2.5-72b-instruct` | the locked model id (must match what the gateway serves) |
| `HARNESS_PROVIDER` | `ollama` | the crate provider value pointing at the host gateway |
| `HARNESS_GATEWAY_URL` | `http://host.docker.internal:11434` | the gateway base URL (`OLLAMA_BASE_URL`) |

The judge follows the lock: `SCORER_MODEL` defaults to the locked
`HARNESS_MODEL`, so bumping the locked model carries the judge with it and the
whole scoring stack stays one frozen open-weight model. Point the judge at the
same gateway with `LLM_BASE_URL` for reproducible verdicts; see
`docs/judge-determinism.md`.

## Open item before flipping it on

**Confirm the miner crate's provider contract for a local gateway.** The crate
(the miner SDK, outside this repo) must honor `DITTOBENCH_PROVIDER=<gateway>` +
`OLLAMA_BASE_URL`/base-url to route chat at the host gateway. Today the crate is
driven with `DITTOBENCH_PROVIDER=openrouter`; the exact provider string it
accepts for an OpenAI-compatible local endpoint must be verified and
`HARNESS_PROVIDER` set to match. Everything else is config.

## Infra to provision (with the egress phase-2 work)

1. Run the host gateway (Ollama/vLLM) serving the Qwen2.5 model on
   `:11434` (OpenAI-compatible).
2. Remove `openrouter.ai` from `EGRESS_PROXY_ALLOW` (drop to deny-all internet;
   the gateway is reached via `NO_PROXY` + the firewall host-gateway allowance).
3. Set `DITTOBENCH_MODEL_LOCK=1`, `HARNESS_MODEL`, `HARNESS_PROVIDER`,
   `HARNESS_GATEWAY_URL` on the validator service.
