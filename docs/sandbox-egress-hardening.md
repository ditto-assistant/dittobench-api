# Sandbox egress allowlist + isolation (C-ISO)

**Status: design + phase-1 plumbing landed; enforcement (proxy + firewall)
pending infra.** This is the SN118 roadmap item "sandbox egress allowlist +
seccomp/isolation" — the top robustness gap before running real scoring at
volume.

## Problem

`internal/sandbox` builds an **untrusted miner tarball** and `docker run`s it on
the host daemon so dittobench can drive its `/run` + `/health` contract. Today
that container runs on the **default bridge network with unrestricted egress**
(the `docker run` line has no `--network`), holding a **real BYOK
`OPENROUTER_API_KEY`** and the seeded eval haystack. A malicious harness can:

1. **Exfiltrate the OpenRouter key** → run up arbitrary LLM spend on our dime.
2. **Exfiltrate the seed haystack / eval cases** → learn or game the eval set.
3. **Call home / receive commands** → use the sandbox as a foothold.
4. **Use the sandbox as an attack proxy** → scan, DoS, or abuse third parties
   from our IP (reputational + legal risk).

`no-new-privileges` + `--memory`/`--cpus` are set, but there is no egress control
and no capability/seccomp hardening. `internal/netguard` guards the *dittobench
process's own* outbound fetches (SSRF on `harness_url`/tarball URL); it does
nothing for the *container's* egress. This doc covers the container layer.

## What egress the container legitimately needs

From `cmd/dittobench-api/main.go` the sandbox env is:

| Destination | Why | Allow? |
|-------------|-----|--------|
| `openrouter.ai:443` | the miner's agent makes its own LLM calls during `/run` (BYOK `OPENROUTER_API_KEY`) | **yes** |
| `host.docker.internal:11434` | the host Ollama embeddings server (`OLLAMA_BASE_URL`) | **yes** (host-gateway, not internet) |
| loopback `127.0.0.1` | dittobench reaches the harness on a published loopback port (inbound, not egress) | n/a |
| everything else | — | **deny** |

So the allowlist is tiny: **OpenRouter (by hostname)** + **the host Ollama
gateway**. Everything else is denied.

## Why Docker alone can't do this

Docker has no per-container egress allowlist by hostname. OpenRouter sits behind
Cloudflare (rotating IPs), so an IP allowlist is fragile. The robust, standard
solution is **network isolation + an allowlisting forward proxy**:

```
 ┌─────────────── sandbox container (untrusted miner) ───────────────┐
 │  HTTPS_PROXY=http://egress-proxy:3128   NO_PROXY=host.docker.internal,127.0.0.1
 │  agent → OpenRouter  ─────────────► (via proxy only)              │
 │  embeddings → host.docker.internal:11434  (direct, host-gateway) │
 └────────────┬──────────────────────────────────┬──────────────────┘
              │ on: ditto-sandbox network         │
     ┌────────▼─────────┐               host firewall (iptables/nft):
     │ egress-proxy     │  CONNECT       DROP all forward from the sandbox
     │ (allowlist:      │  allowlist ──► subnet EXCEPT → proxy and → host-gateway
     │  openrouter.ai)  │────────────────────► openrouter.ai:443
     └──────────────────┘
```

Two enforcement layers, both required:

1. **Forward proxy (hostname allowlist).** A small HTTP CONNECT proxy that only
   permits `CONNECT openrouter.ai:443` (config-driven allowlist). Handles the
   CDN-IP problem — allowlisting is by hostname at CONNECT time. The container is
   pointed at it via `HTTPS_PROXY`/`HTTP_PROXY`.
2. **Host firewall (enforcement).** The proxy env is advisory — a malicious
   client can ignore it. So the sandbox network's subnet gets an
   iptables/nftables rule set that **DROPs all forward traffic except to the
   proxy and the host-gateway**. A harness that ignores `HTTPS_PROXY` and dials
   the internet directly simply fails (fail-closed), it does not leak.

`NO_PROXY=host.docker.internal,127.0.0.1,localhost` so embeddings + loopback
bypass the proxy; the firewall separately allows the host-gateway.

### Fail-closed

If the proxy or firewall is misconfigured/unreachable, egress **fails** (the run
errors) rather than falling back to open egress. A blocked run surfaces as a
scoring failure with a clear reason, never a silent full-egress run.

## Configuration surface (phase-1, landed)

`LocalDocker` gains config (all env-driven, defaults preserve today's behavior so
nothing breaks until infra provisions the proxy + firewall):

| Field / env | Default | Effect |
|-------------|---------|--------|
| `EgressNetwork` / `DITTOBENCH_SANDBOX_EGRESS_NETWORK` | `""` | when set, `docker run --network <name>` (the isolated sandbox network) instead of the default bridge |
| `EgressProxy` / `DITTOBENCH_SANDBOX_EGRESS_PROXY` | `""` | when set, injects `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` into the container |
| `Harden` / `DITTOBENCH_SANDBOX_HARDEN` | `false` | when true, `--cap-drop ALL` (a userland HTTP server needs no Linux caps) + a seccomp profile |
| `PidsLimit` / `DITTOBENCH_SANDBOX_PIDS_LIMIT` | `512` | `--pids-limit` (fork-bomb bound) — **always on**; a safe unconditional add |

Empty egress config = current full-egress behavior, so dev/CI keep working; infra
turns it all on together.

## Infra to provision (phase-2, follow-up — the enforcement)

Ansible on the validator/dittobench VM:

1. Create the `ditto-sandbox` user-defined bridge network (fixed subnet).
2. Run the **egress proxy** (a small allowlisting CONNECT proxy — a ~50-line Go
   binary or a hardened `tinyproxy`/`squid` with `openrouter.ai` allowlisted),
   reachable from the sandbox network, not from the internet.
3. Install **iptables/nftables** rules: for the `ditto-sandbox` subnet, `DROP`
   all `FORWARD` egress except → the proxy and → the host-gateway (Ollama).
4. Set `DITTOBENCH_SANDBOX_EGRESS_NETWORK=ditto-sandbox`,
   `DITTOBENCH_SANDBOX_EGRESS_PROXY=http://<proxy>:3128`,
   `DITTOBENCH_SANDBOX_HARDEN=true` in the dittobench service env.

## Deeper isolation (later)

- **seccomp** profile (default-deny syscalls) — ship a JSON profile via
  `--security-opt seccomp=`.
- **gVisor / Kata** runtime (`--runtime=runsc`) for kernel isolation of the
  untrusted build+run — the strongest step; a bigger infra lift.
- **read-only rootfs** (`--read-only` + tmpfs for `/app` writable paths) — needs
  the harness to tolerate a writable-only tmpfs for its Turso DB; validate first.

## Testing

- **Unit (phase-1):** assert `Run()` builds the expected `docker run` args for
  each config (network attached, proxy env injected, cap-drop/pids-limit present).
- **Integration (phase-2):** on a provisioned host, a harness that curls a
  non-allowlisted host times out/fails; a harness that calls OpenRouter succeeds;
  a harness that ignores `HTTPS_PROXY` and dials direct is dropped.

## Threat model note

This defends against a **malicious miner submission** (exfiltration, call-home,
attack-proxy). It does **not** by itself stop abuse of the OpenRouter key for
non-eval calls *to OpenRouter* (the host is allowlisted) — that is bounded
separately by the per-run token budget (`LLM_MAX_TOKENS`/`LLM_RUN_TOKEN_BUDGET`,
roadmap C3) and, ideally later, per-run scoped keys.
