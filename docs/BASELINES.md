# DittoBench baselines

Reference-harness scores under the locked model, for tracking benchmark
difficulty across `bench_version` and as the target a miner submission must beat.
Regenerate with `scratchpad/baseline_qwen.py` (24 distinct seeds, `run_size=full`).

## Run 1: bench_version 2, v0.7.0

- Date: 2026-07-12
- Config: v0.7.0 scorer + datagen (bounded canary, multi-family metamorphic), `bench_version` 2
- Harness: stock reference (dittobench starter kit baseline, unmodified)
- Model: Qwen3-32B. Backend: OpenRouter `qwen/qwen3-32b`. Same weights as the scored
  Chutes `Qwen/Qwen3-32B-TEE`; the TEE is the exact scored backend and may differ slightly.
- Method: 24 distinct seeds (`run_size=full`), so the spread is the real
  run-to-run variance a leaderboard score carries (dataset difficulty plus model noise).
  SE is the standard error of the mean (sd / sqrt(N)). 0 errored runs.

### Headline

| metric | mean | SE | 95% CI | sd | min / max |
| --- | --- | --- | --- | --- | --- |
| composite | 0.492 | 0.013 | [0.467, 0.517] | 0.063 | 0.385 / 0.619 |
| tool_mean | 0.793 | 0.007 | [0.779, 0.806] | 0.034 | 0.726 / 0.862 |
| memory_mean | 0.419 | 0.019 | [0.382, 0.456] | 0.093 | 0.185 / 0.574 |

### Gates and latency

- Canary pass rate: 0.333
- Metamorphic consistency: 0.347 +/- 0.065 SE (graded over the run's invariance families)
- Mean within-run `composite_stderr`: 0.0408
- Median latency: 14.3 s per case (reported, not scored)

### Memory categories (weakest first)

The memory half is where the composite lives and where a submission differentiates.
The weakest categories (injection-resistance, computed-answer, temporal-reasoning,
knowledge-update) are the reference harness's real gaps and the highest-value levers.
The memory/tool split below is approximate (a few memory-tool routing categories
sit either side).

| category | mean | SE | n |
| --- | --- | --- | --- |
| injection-resistance | 0.069 | 0.035 | 24 |
| computed-answer | 0.083 | 0.058 | 24 |
| recipe_create | 0.175 | 0.073 | 24 |
| knowledge-update | 0.181 | 0.053 | 24 |
| preference-application | 0.181 | 0.045 | 24 |
| temporal-reasoning | 0.219 | 0.031 | 24 |
| memory_save_not_search | 0.271 | 0.052 | 24 |
| memory_update | 0.292 | 0.095 | 24 |
| assistant-recall | 0.319 | 0.062 | 24 |
| canary | 0.333 | 0.098 | 24 |
| memory-write-read | 0.333 | 0.060 | 24 |
| multi-session | 0.373 | 0.040 | 24 |
| point-in-time | 0.417 | 0.064 | 24 |
| abstention | 0.433 | 0.049 | 24 |
| single-session-recall | 0.554 | 0.036 | 24 |
| isolation | 0.625 | 0.052 | 24 |
| preference | 0.625 | 0.101 | 24 |
| memory-write | 0.681 | 0.062 | 24 |
| contradiction | 0.722 | 0.052 | 24 |
| aggregation-count | 0.875 | 0.069 | 24 |
| memory_subject | 0.875 | 0.054 | 24 |
| memory_lookup | 0.917 | 0.039 | 24 |
| memory_delete | 1.000 | 0.000 | 24 |
| memory_fetch | 1.000 | 0.000 | 24 |
| recipe_apply | 1.000 | 0.000 | 24 |

### Tool categories (weakest first)

A 0.000 here is a real reference-harness gap, not a broken case: the model never
selects the calendar-create tool though `calendar_create_event` is offered every
run (`calendar_search` scores 0.85, so the tool path works; the model just does
not invoke create).

| category | mean | SE | n |
| --- | --- | --- | --- |
| calendar_create | 0.000 | 0.000 | 24 |
| agent_run_not_read | 0.146 | 0.047 | 24 |
| image_edit_not_create | 0.500 | 0.067 | 24 |
| multi_job_status | 0.528 | 0.102 | 24 |
| email_send | 0.533 | 0.086 | 24 |
| job_chain_result_usage | 0.553 | 0.102 | 24 |
| agent_workflow | 0.572 | 0.019 | 24 |
| capability_discovery | 0.583 | 0.103 | 24 |
| set_tool_prefs | 0.633 | 0.023 | 24 |
| multi_web_read | 0.694 | 0.053 | 24 |
| workflow_not_job | 0.717 | 0.029 | 24 |
| agent_job | 0.735 | 0.066 | 24 |
| feedback | 0.800 | 0.042 | 24 |
| multi_web_result_usage | 0.828 | 0.053 | 24 |
| calendar_search | 0.850 | 0.040 | 24 |
| arg_hallucination | 0.875 | 0.069 | 24 |
| automation_not_job | 0.875 | 0.051 | 24 |
| automation_list | 0.903 | 0.058 | 24 |
| set_effort | 0.917 | 0.058 | 24 |
| set_font | 0.917 | 0.034 | 24 |
| route_web_not_memory | 0.917 | 0.026 | 24 |
| agent_read_not_run | 0.938 | 0.035 | 24 |
| parallel_web_image | 0.938 | 0.042 | 24 |
| multi_image_edit | 0.944 | 0.043 | 24 |
| artifacts_create | 0.958 | 0.029 | 24 |
| multi_subject_scope | 0.958 | 0.042 | 24 |
| web_search | 0.971 | 0.014 | 24 |
| image_create | 1.000 | 0.000 | 24 |
| link_read | 1.000 | 0.000 | 24 |
| no_tool | 1.000 | 0.000 | 24 |
| route_memory_not_web | 1.000 | 0.000 | 24 |
| set_accent | 1.000 | 0.000 | 24 |
| set_model | 1.000 | 0.000 | 24 |
| settings | 1.000 | 0.000 | 24 |
| web_recovery_result_usage | 1.000 | 0.000 | 24 |
| web_result_usage | 1.000 | 0.000 | 24 |

