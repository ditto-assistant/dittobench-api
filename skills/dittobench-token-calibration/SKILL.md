---
name: dittobench-token-calibration
description: Run, resume, and audit the reproducible DittoBench v5 reference starter-harness token calibration. Use when measuring relay-observed starter-kit token use, checking progress across the 60 pinned OpenRouter run-size/seed contracts, running exactly one next calibration case, or generating the three-group p90 baseline manifest.
---

# DittoBench Token Calibration

Use the bundled script rather than recreating scorer, relay, Docker, and artifact-server commands by hand. It preserves the immutable scorer/starter/dataset/provider contract and rejects self-reported or incomplete token telemetry.

## Safety and scope

- Treat this as paid provider work. Run one case unless the user explicitly authorizes a batch.
- Never print, copy, or persist provider credentials. The script reads them into the relay process environment.
- Do not modify the pinned `api/` or `starter-kit/` checkouts. A changed HEAD fails validation.
- Do not activate scoring or edit the production manifest. Calibration writes only reports and a generated candidate manifest.
- Provider/profile/model identity is part of every baseline key. The approved
  production contract currently contains OpenRouter only; the runner derives
  providers from `contract.json` rather than accepting an unpinned profile.

## Quick workflow

From this repository, the calibration root defaults to `calibration/token-efficiency-v5`. Override it with `DITTOBENCH_CALIBRATION_ROOT` when using a prepared runtime root containing the pinned `api/`, `starter-kit/`, `source/`, and `image/` inputs.

1. Inspect and validate progress:

   ```sh
   python3 skills/dittobench-token-calibration/scripts/calibrate.py status
   ```

2. Preview the next paid run without making provider calls:

   ```sh
   python3 skills/dittobench-token-calibration/scripts/calibrate.py run --provider openrouter --next --dry-run
   ```

3. Run exactly one next case:

   ```sh
   DITTOBENCH_CALIBRATION_ROOT=/path/to/prepared-root \
     python3 skills/dittobench-token-calibration/scripts/calibrate.py run --provider openrouter --next
   ```

   Add `--run-size medium` to advance only that group, or use `--run-size small --seed 202` for a specific pinned case. The script builds revision-keyed scorer binaries once, reuses the screened starter image, starts isolated relay/API/artifact services, polls to completion, audits trusted usage, writes evidence, and cleans up its processes.

   After one audited end-to-end case, an explicitly authorized full calibration
   is one resumable command:

   ```sh
   DITTOBENCH_CALIBRATION_ROOT=/path/to/prepared-root \
     python3 skills/dittobench-token-calibration/scripts/calibrate.py \
     batch --provider all --remaining --workers 8
   ```

   `batch` always resumes from valid reports, gives every worker an isolated
   relay/API counter boundary, retries a failed case up to three times, and
   stops rather than skipping a persistently failing contract.

4. Re-run `status`. A valid report counts only when benchmark version, dataset digest, relay source, usage completeness, provider profile, and model all match the pinned contract.

5. After all 60 OpenRouter runs, generate the review artifact:

   ```sh
   python3 skills/dittobench-token-calibration/scripts/calibrate.py baseline
   ```

   This invokes the scorer's `cmd/tokenbaseline`, requires exactly 20 samples in each of three run-size groups, and writes `reports/baselines_v5.generated.json`. Review and version that candidate separately; do not copy it into production automatically.

6. Generate the reviewable token, latency, metering, and quality distributions:

   ```sh
   python3 skills/dittobench-token-calibration/scripts/calibrate.py summary
   ```

   This writes `calibration/token-efficiency-v5/summary.json` and identifies the exact seed selected by nearest-rank p90 in each group.

## Expected local dependencies

Docker, Go, Ollama, Python, and Git must be available. The script ensures `embeddinggemma` exists. It may reuse a LAN-reachable Ollama, but it refuses to kill or reconfigure a pre-existing localhost-only service. Ports 11435, 18000, and 18080 must be free.

Credentials come from `CHUTES_API_KEY` / `OPENROUTER_API_KEY`, or from `~/.config/dittobench/calibration.env`; set `DITTOBENCH_CREDENTIAL_ENV` or pass `--credential-env` for a different dotenv file.

If host-address detection is wrong, set `DITTOBENCH_CALIBRATION_HOST_IP` to an address reachable both from the host scorer and Docker sandbox.

## Reporting

For each run, report:

- provider/profile/model, run size, seed, dataset digest, starter and scorer revisions;
- raw quality, prompt/completion/total tokens, request metering completeness, and elapsed time;
- report and transcript paths;
- completed/remaining counts by provider and run size;
- that `scoring_enabled=false` and `baseline_unavailable` is expected until audited budgets are installed.

Do not claim a p90 from a single run. One completed case is process proof; each group needs exactly 20 pinned seeds.
