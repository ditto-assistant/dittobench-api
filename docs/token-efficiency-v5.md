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

## Versioned transform

Prompt tokens carry four times the score weight of completion tokens:

```text
weighted_tokens = prompt_tokens + 0.25 * completion_tokens
ratio           = baseline_weighted_tokens / observed_weighted_tokens

if ratio >= 1:
    multiplier = ratio ^ 0.25
else:
    multiplier = max(0.75, ratio ^ 0.50)

adjusted = round_6(raw_composite * multiplier)
```

The transform is continuous and monotonic. Above-baseline usage receives the
existing square-root penalty with a `0.75` floor. Below-baseline usage receives
a slowly growing fourth-root reward with no score ceiling, so a genuinely novel
agent can keep improving past an arbitrary token threshold. Provider accounting
and the fixed benchmark size still impose a positive observed-token minimum.

For a raw score of `0.9` and a `1,000` weighted-token baseline:

| Observed | Multiplier | Adjusted |
| ---: | ---: | ---: |
| 640 | 1.118034… | 1.006231 |
| 1,000 | 1.0 | 0.9 |
| 1,600 | 0.790569… | 0.711512 |
| 100,000 | 0.75 (floor) | 0.675 |
| 250 | 1.414214… | 1.272792 |
| 10 | 3.162278… | 2.846050 |

The record preserves raw quality, prompt/completion/total tokens, baseline id
and counts, both weighted costs, token weights, curve exponents, multiplier,
adjusted score, and raw/adjusted standard error as separate fields. Context
stuffing therefore loses primarily through provider-observed prompt tokens.
Completion tokens retain one-quarter weight so pathological verbosity is not
free, but concise helpful prose is not the main route to an efficiency reward.

## Quality floor and fail-neutral behavior

An efficiency reward is available only when all of these hold:

- raw composite at least `0.80`;
- tool mean and memory mean each at least `0.70`;
- at least `95%` of cases produced visible answer text.

These are eligibility floors, not new quality points. Empty, mostly empty, or
low-correctness terse answers keep multiplier `1.0`; low token use cannot rescue
them. These deliberately high reward gates must be recalibrated with the v5
friendliness/chat-quality distributions before activation and can be tightened
without putting style tokens directly into the efficiency formula. The floors
block rewards only: above-baseline usage still receives its stuffing penalty.

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
breakdown from the median-weighted-token run over the five pinned seeds. Five
is odd, so no interpolation or rounding choice enters the contract.

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
