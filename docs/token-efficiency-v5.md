# DittoBench v5 token efficiency

DittoBench v5 adjusts an eligible raw chat-quality score using model tokens
observed by the validator-owned relay. A miner's `prompt_tokens` and
`output_tokens` fields are never used. The relay reads the upstream provider's
non-streaming `usage` object, validates it, and exposes monotonic counters. The
scorer serializes scored sandbox lifetimes and takes a before/after delta, so a
run cannot report its own usage or overlap another scored run's counter window.
The relay also records post-pinning serialized prompt bytes as a provider-
independent context-size signal; it is audit telemetry, while provider prompt
tokens are the scored context-stuffing cost.

The v2-v4 score contracts are unchanged. V5 is not advertised by
`/v1/capabilities` until the embedded manifest contains a reviewed baseline for
small, medium, and full on both certified provider profiles.

## Frozen transform

For a positive observed total and a matching baseline:

```text
ratio      = baseline_total_tokens / observed_total_tokens
multiplier = clamp(sqrt(ratio), 0.75, 1.25)
adjusted   = round_6(raw_composite * multiplier)
```

The transform is continuous and monotonic: any usage above baseline is a
penalty, any usage below baseline is a reward, equality is neutral, and extreme
values are bounded. Since raw quality can reach 1 and the reward ceiling is
1.25, the adjusted score may exceed 1 and tops out at 1.25.

For a raw score of `0.9` and a `1,000`-token baseline:

| Observed | Multiplier | Adjusted |
| ---: | ---: | ---: |
| 640 | 1.25 | 1.125 |
| 1,000 | 1.0 | 0.9 |
| 1,600 | 0.790569… | 0.711512 |
| 100,000 | 0.75 (floor) | 0.675 |

The record preserves raw quality, prompt/completion/total tokens, baseline id
and counts, multiplier, adjusted score, and raw/adjusted standard error as
separate fields. Context stuffing therefore loses through provider-observed
input tokens. Normal helpful output is represented in the stock starter-kit
baseline rather than charged a second time by a separate output-length term.

## Quality floor and fail-neutral behavior

An efficiency reward is available only when all of these hold:

- raw composite at least `0.50`;
- tool mean and memory mean each at least `0.25`;
- at least `90%` of cases produced visible answer text.

These are eligibility floors, not new quality points. Empty, mostly empty, or
low-correctness terse answers keep multiplier `1.0`; low token use cannot rescue
them. The broader v5 friendliness/chat-quality evaluator can tighten this gate
without putting style tokens directly into the efficiency formula.

Missing, malformed, zero, inconsistent, or extreme provider usage also keeps a
neutral multiplier and is reported as unavailable. It does not turn a valid
quality run into a validator-infrastructure failure. A relay restart, provider
profile change, or upstream infrastructure failure still invalidates the run
because attribution or delivery is no longer trustworthy.

The relay reports provider end-to-end milliseconds separately from token usage.
TTFT is explicitly `unavailable_non_streaming`; it is not guessed. Latency is
not in the v5 token multiplier because it mixes provider/model capacity with
harness behavior. Existing per-case latency and relay provider latency remain
available for Pareto analysis and later calibration.

## Baseline contract and calibration

[`internal/efficiency/baselines_v5.json`](../internal/efficiency/baselines_v5.json)
pins:

- bench version and v5 dataset known vector;
- exact stock starter-kit commit;
- five public seed/dataset hashes for every run size;
- formula and manifest schema versions;
- one baseline per run size, provider, immutable relay profile revision, and
  exact upstream model id.

This prevents comparing a miner on one provider/model revision to starter-kit
usage measured on another. Each baseline is the complete prompt/completion
breakdown from the median-total-token run over the five pinned seeds. Five is
odd, so no interpolation or rounding choice enters the contract.

To calibrate or refresh:

1. Check out the manifest's starter-kit commit and build it through the same
   screened-image path used for miners.
2. For each certified relay profile, run all 15 pinned v5 datasets (five seeds
   for each of small, medium, and full) through canonical `/v1/score` execution.
3. Save each completed run JSON. It must contain
   `details.token_usage.source=model_proxy_provider_response` and complete usage.
4. Run `go run ./cmd/tokenbaseline <report.json>...`. The command rejects wrong
   datasets, missing trusted usage, duplicate seeds, mixed/incomplete groups,
   and emits a deterministic manifest with content-derived baseline ids.
5. Review the token distributions and quality of all starter runs, replace the
   manifest in a PR, and rerun the full suite. Never edit a numeric baseline in
   place without changing the pinned starter/profile/model contract and audit
   evidence.

At this commit the calibration datasets are pinned but numeric baselines are
deliberately empty. That keeps v5 capability negotiation inactive until both
providers have real measurements; fabricated or cross-provider numbers cannot
silently activate scoring.
