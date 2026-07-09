# Handoff: DittoBench v2 transparency + single-generator consolidation

Written 2026-07-09 for a fresh agent picking up this work with no prior context.
It spans five repositories under `~/projects`. Read section 0 before doing
anything.

## 0. Security constraints (still in effect, do not violate)

- Nothing is made public without Nick's explicit approval. This is the hard gate
  for both the ditto-subnet publish (task #38) and the generator publish (the main
  pending item here). Publishing source is a one-way door.
- Keys and secrets are never printed to the terminal or committed. There is no key
  in any of the staged work; keep it that way.
- Repo visibility today: `ditto-platform`, `dittobench-api`, `ditto-subnet`,
  `ditto-data-pipeline` are PRIVATE. `dittobench-starter-kit`, `ditto-harness` are
  PUBLIC. `dittobench-datagen` does not exist on GitHub yet (staged locally only).
- Writing style for all docs and prose: no emdashes, no stacked semicolons, no
  bold-phrase lead-ins, dry and direct.

## 1. Mission and context

DittoBench is the benchmark for Bittensor Subnet 118. v2 scoring is a distributed
k=3 median: three distinct validators score the same platform-pinned dataset and
the platform takes the median composite. The active workstream is making the
benchmark maximally auditable while preserving anti-overfit. Anti-overfit now
rests on on-chain seed derivation (task #54, landed): the per-submission seed comes
from a block hash fixed after the miner commits, so it cannot be predicted, which
is what makes it safe to open the generator.

Grading is deterministic-first. The dataset generator is fully non-LLM and
byte-reproducible from `(seed, bench_version)`. The only remaining LLM component is
the judge, which grades a slice of cases.

## 2. Repo map and current git state

All paths under `/home/tetra/projects`.

| Repo | Visibility | Working branch | HEAD | Pushed? |
|---|---|---|---|---|
| ditto-platform | private | `dev` | `61e358d` | yes, origin/dev up to date |
| dittobench-api | private | `main` | `d283e9b` | yes |
| dittobench-api | private | `nick/single-generator` | `094b8ef` | NO, local only |
| dittobench-api | private | `nick/judge-determinism` | merged to main | merged |
| ditto-subnet | private | `main` | `17100e4` | yes |
| ditto-data-pipeline | private | `nick/single-generator` | `a8b5cbe` | NO, local only |
| dittobench-datagen | none yet | `main` | `7fb205b` | NO, never pushed |

The default branch you land dittobench-api and ditto-subnet work on is `main`.
ditto-platform lands on `dev` (its integration branch, called "platform-dev" by
Nick). Merges to dittobench-api `main` auto-deploy to Cloud Run (see section 7).

## 3. What is done and landed (already pushed)

These are complete and on their private remotes. Do not redo them.

1. v2 decentralization merged to integration branches:
   - ditto-platform `feat/k3-ticketing` merged to `dev` (`61e358d`). Full suite
     green, alembic head `e5b1c9d24f30`.
   - dittobench-api `feat/extract-data-pipeline` merged to `main` (deployed, v2
     verified live: `bench_version:2`, `/v1/sample` serving).
   - ditto-subnet `feat/validator-role-split` merged to `main`. 393 tests green.
     This is a private merge, NOT the public publish (#38 stays gated).

2. Deterministic judge (dittobench-api, merged to main):
   - Every judge request now sends `temperature 0 + top_p 1 + a fixed consensus
     seed (42)`, was temperature 0 only. See `internal/llm/llm.go`
     (`deterministicSeed`, `chatRequest`, `Complete`).
   - Judge base URL is configurable (`LLM_BASE_URL`) so the judge can run against a
     self-hosted vLLM or Ollama gateway where the knobs are actually honored.
   - Findings, limits, and the vLLM/Ollama recipe are in
     `dittobench-api/docs/judge-determinism.md`. Honest limit: on a hosted batched
     model this is best-effort; full reproducibility needs the judge self-hosted.

3. Validator model-hosting guide (ditto-subnet, merged to main):
   `ditto-subnet/docs/VALIDATOR-MODEL-HOSTING.md`, linked from RUNNING-A-VALIDATOR.
   Covers hosting Ollama or vLLM for both the harness model (the lock) and the
   judge (determinism).

4. Sampler un-redacted (dittobench-api, merged to main, `d283e9b`):
   `GET /v1/sample` now returns the full real `DatasetArtifact` including answer
   keys, from the reserved negative-seed namespace. Redaction was removed because
   with the generator open the full dataset is derivable anyway. See
   `cmd/dittobench-api/sample.go` and `sample_test.go`.

## 4. What is staged and NOT landed (the pending work)

The single-generator consolidation is fully built and validated locally but not
landed, because it depends on the public repo existing. There were three copies of
the generator; they are collapsed onto one canonical module.

- `dittobench-datagen` (local, `/home/tetra/projects/dittobench-datagen`, git
  initialized, 2 commits, never pushed). This is the extracted, MIT-licensed,
  stdlib-only public generator. Layout after the flatten: top-level importable
  packages `gen`, `persona`, `datagen`, `protocol`, `toolexec`, `catalog`, plus
  `cmd/generate`. Build, vet, tests, and determinism all pass. Known-vector anchor:
  seed 123456789 run_size full produces dataset_sha256
  `3586a8fb2211fbd785dda9fe55580135173059f4124ceeb4f616c70a09804047`, pinned by
  `gen/publicvector_test.go` and identical to what the private repos generate.

- `dittobench-api` branch `nick/single-generator` (`094b8ef`, local only): deletes
  the 6 in-tree generator packages and imports them from `dittobench-datagen`
  instead. Full suite green. Carries a local replace directive:
  `replace github.com/ditto-assistant/dittobench-datagen => /home/tetra/projects/dittobench-datagen`.

- `ditto-data-pipeline` branch `nick/single-generator` (`a8b5cbe`, local only):
  deletes its 3 duplicate packages, imports `dittobench-datagen`, and drops its
  dependency on `dittobench-api` entirely. Builds clean. Same local replace
  directive.

What stays private (NOT in the public module): the judge (`internal/scorer`,
`internal/llm`), the scoring formulas and weights, the sandbox and runner, all
keys, and the platform business logic.

## 5. The decision that blocks landing

Publishing `dittobench-datagen` is the one-way door. Nick chose "prep and show,
then I push", so the extraction was fully prepared and shown but not pushed. Get
explicit approval before section 6. Two implications Nick should sign off on:

1. The shared module includes `protocol`, `toolexec`, and `catalog`, so those
   wire-shape packages become public too, not just the generator. They are type
   definitions and tool fixtures, not scoring logic. This is consistent with the
   transparency goal but is more public surface than "just the generator". If Nick
   does not want the wire shapes public, the single-generator branches need
   rework (keep protocol private and duplicate only it, which partly defeats the
   single-source goal).
2. The private production scorer (dittobench-api) will depend on a public repo.
   Standard Go practice, and it makes generator byte-drift impossible, but it is a
   real build coupling.

If Nick declines the publish, the fallback is to abandon the two
`nick/single-generator` branches (mains are untouched) and keep the in-tree copies.
The transparency wins from tasks A/B/C and the sampler still stand without it.

## 6. Exact land sequence (only after approval)

### 6a. Publish the public module

```
cd /home/tetra/projects/dittobench-datagen
# create the public repo and push main
gh repo create ditto-assistant/dittobench-datagen --public --source=. --remote=origin --push
# tag a version to pin (the private repos require a real version, not a replace)
git tag v0.1.0 && git push origin v0.1.0
```

Sanity-check before you do this: `gofmt -l .` is empty, `go test ./...` passes,
and `git log` shows only the two intended commits with no secrets. Re-run the
secrets grep from section 7.

### 6b. Land dittobench-api

```
cd /home/tetra/projects/dittobench-api
git checkout nick/single-generator
go mod edit -dropreplace=github.com/ditto-assistant/dittobench-datagen
go mod edit -require=github.com/ditto-assistant/dittobench-datagen@v0.1.0
GOPROXY=direct go mod tidy      # GOPROXY=direct because a just-tagged repo may lag the module proxy
go build ./... && go test ./...
git commit -am "chore: pin dittobench-datagen v0.1.0, drop local replace"
git checkout main && git merge --no-ff nick/single-generator
git push origin main
```

Then verify the Cloud Run deploy actually ran (section 7, the CGO gotcha). The
merge to main triggers CI plus a deploy. Confirm the live service still serves
`bench_version:2` and that /v1/score still regenerates the known vector.

Important: CI and Cloud Build must be able to fetch the now-public
`dittobench-datagen`. It is public, so this works via the module proxy, but if the
build fails to resolve it immediately after tagging, set `GOFLAGS=-mod=mod` and
`GOPROXY=direct` in the build environment or wait for the proxy to catch up. Also
commit the updated `go.sum` (go mod tidy writes it).

### 6c. Land ditto-data-pipeline

```
cd /home/tetra/projects/ditto-data-pipeline
git checkout nick/single-generator
go mod edit -dropreplace=github.com/ditto-assistant/dittobench-datagen
go mod edit -require=github.com/ditto-assistant/dittobench-datagen@v0.1.0
GOPROXY=direct go mod tidy
go build ./... && go test ./...
git commit -am "chore: pin dittobench-datagen v0.1.0, drop local replace"
git checkout main && git merge --no-ff nick/single-generator
git push origin main
```

Check whether ditto-data-pipeline has its own deploy pipeline before pushing (its
generate service is deployed separately; confirm how). This is not yet verified.

## 7. Verification playbook

- Byte-parity across all three generators: build `dittobench-datagen/cmd/generate`
  and run `generate -seed 123456789 -run-size full -sha`. It must print
  `3586a8fb2211fbd785dda9fe55580135173059f4124ceeb4f616c70a09804047`. Since
  dittobench-api and ditto-data-pipeline import the same module, they produce the
  same bytes by construction.

- Per-repo test commands:
  - dittobench-api, ditto-data-pipeline, dittobench-datagen: `go test ./...`
    (dittobench-api uses cgo for `internal/astfp`; the default runner has cgo on).
  - ditto-platform: `uv run pytest -q` (681+ pass), `uv run ruff check ditto/`.
  - ditto-subnet: `uv run pytest -q` (393 pass).

- Secrets scan before any public push, from the dittobench-datagen root:
  `grep -rniE "api[_-]?key|secret|password|bearer|BEGIN (RSA|EC|OPENSSH|PRIVATE)|mnemonic|sk-[a-z0-9]|hotkey" --include=*.go --include=*.json .`
  Expect only benign hits on the word "token" in benchmark content and field
  names.

- Cloud Run deploy gotcha (dittobench-api): the v2 AST fingerprint imports a cgo
  tree-sitter wrapper. The Dockerfile builds with `CGO_ENABLED=1` plus musl static
  link (fixed, commit history). The CI test job builds with cgo on, so a broken
  image build can pass tests but silently skip the deploy job. After any merge to
  dittobench-api main, verify the deploy job ran:
  `gh run list --repo ditto-assistant/dittobench-api --branch main --limit 1` then
  `gh run view <id> --json jobs`. A green "CI" can still mean the deploy skipped.
  Live URL: `https://dittobench-api-22790208601.us-central1.run.app` (check
  `/health` and `/v1/sample?sample=0&run_size=small`).

## 8. Remaining open items (not part of the land sequence)

- Task #50 model lock: code is done and gated behind `DITTOBENCH_MODEL_LOCK`
  (default off). Only infra remains and is not codeable here: provision the
  Ollama/vLLM host serving the locked Qwen2.5 model, drop `openrouter.ai` from
  `EGRESS_PROXY_ALLOW`, and confirm the miner crate's provider contract for a local
  gateway. See `dittobench-api/docs/model-lock.md`.
- Task #38 publish ditto-subnet: gated on Nick's explicit approval. Separate from
  the generator publish.
- Judge hardening (documented in `docs/judge-determinism.md`, not yet built):
  JSON-schema structured output for the judge, wider deterministic rubric to shrink
  the judged surface, and residual-disagreement measurement via `SCORER_MODEL_B`.
- Generator plan prerequisites still open (`docs/open-generator-plan.md`): confirm
  the anti-seed-farming posture (per-hotkey submission cooldown or rate limit) is
  adequate, and consider future-block seed binding to remove platform block-choice
  grinding. Neither blocks the extraction but both were flagged for go-live.

## 9. Task IDs (in the session task list)

- Done and landed: #59, #60, #61 (merges), #63, #64 (judge), #66 (datagen
  restructure).
- In progress or staged, awaiting the publish decision: #62 (generator open), #67
  (dittobench-api depends on shared module), #68 (ditto-data-pipeline depends on
  shared module).
- Still gated on approval or infra: #38 (subnet publish), #50 (model-lock infra).

## 10. Key facts and pointers

- On-chain seed derivation lives in ditto-platform
  `ditto/api_server/onchain_seed.py` (`derive_seed(block_hash, agent_id)`), pinned
  on the agent so anyone can recompute and verify. This is why opening the
  generator is safe.
- Auditability endpoints already landed on ditto-platform `dev`: `/public/audit`
  (hash-chained log), `/public/agent/{id}/dataset` (finalized reveal, task A),
  `/public/bench/{version}/corpus` (retired corpus, task B), and per-case
  breakdown on `/public/agent/{id}/scores` (task C).
- dittobench-datagen carries a `.tool-versions` (golang 1.25.6) copied so the asdf
  shim resolves; its go.mod targets go 1.23. Harmless, adjust if the public repo
  should not pin a toolchain.
- Auto-memory index for this project:
  `/home/tetra/.claude/projects/-home-tetra-projects-dittobench-api/memory/MEMORY.md`.
  Relevant entries: benchmark-v2-scoring-model, dittobench-api-phase-status,
  dittobench-api-deploy, benchmark-v2-model-lock, deterministic-datagen,
  writing-style.
