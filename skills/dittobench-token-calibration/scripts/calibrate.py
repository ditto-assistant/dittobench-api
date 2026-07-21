#!/usr/bin/env python3
"""Run and audit the pinned DittoBench v5 starter-kit token calibration."""

from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import json
import os
import re
import shutil
import signal
import socket
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request
from collections import defaultdict
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[3]
DEFAULT_ROOT = REPO_ROOT / "calibration/token-efficiency-v5"
DEFAULT_CREDENTIAL_ENV = Path.home() / ".config/dittobench/calibration.env"
RUN_SIZES = ("small", "medium", "full")
TERMINAL = {"done", "failed", "cancelled"}


class CalibrationError(RuntimeError):
    pass


def load_json(path: Path):
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise CalibrationError(f"cannot read {path}: {exc}") from exc


def write_json(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    temporary.replace(path)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def run_output(command: list[str], *, cwd: Path | None = None, env=None) -> str:
    try:
        return subprocess.run(
            command,
            cwd=cwd,
            env=env,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        ).stdout.strip()
    except FileNotFoundError as exc:
        raise CalibrationError(f"required command is missing: {command[0]}") from exc
    except subprocess.CalledProcessError as exc:
        detail = (exc.stderr or exc.stdout).strip()
        raise CalibrationError(f"command failed ({' '.join(command)}): {detail}") from exc


def contract(root: Path, *, require_runtime: bool = False) -> tuple[dict, dict]:
    spec = load_json(root / "contract.json")
    pinned_api_checkout = root / "api"
    api_checkout = pinned_api_checkout
    if not (api_checkout / "internal/efficiency/baselines_v5.json").is_file():
        api_checkout = REPO_ROOT
    manifest = load_json(api_checkout / "internal/efficiency/baselines_v5.json")
    if spec.get("bench_version") != 5 or manifest.get("bench_version") != 5:
        raise CalibrationError("contract and manifest must both target benchmark v5")
    if spec.get("formula_version") != manifest.get("formula_version"):
        raise CalibrationError("contract formula does not match the scorer manifest")
    if spec.get("starter_kit_revision") != manifest.get("starter_kit_revision"):
        raise CalibrationError("starter-kit revision does not match the scorer manifest")
    checkouts = []
    if (pinned_api_checkout / ".git").exists():
        checkouts.append((pinned_api_checkout, spec["scorer_revision"]))
    elif require_runtime:
        raise CalibrationError(f"missing pinned scorer checkout: {pinned_api_checkout}")
    if (root / "starter-kit/.git").exists():
        checkouts.append((root / "starter-kit", spec["starter_kit_revision"]))
    elif require_runtime:
        raise CalibrationError(f"missing pinned starter-kit checkout: {root / 'starter-kit'}")
    for checkout, revision in checkouts:
        actual = run_output(["git", "rev-parse", "HEAD"], cwd=checkout)
        if actual != revision:
            raise CalibrationError(f"{checkout} is at {actual}, expected {revision}")
    for item in (spec["source"], spec["screened_image"]):
        path = root / item["path"]
        if not path.is_file() and require_runtime:
            raise CalibrationError(f"missing pinned artifact: {path}")
        if not path.is_file():
            continue
        actual = sha256(path)
        if actual != item["sha256"]:
            raise CalibrationError(f"artifact digest mismatch for {path}: {actual}")
    image_path = root / spec["screened_image"]["path"]
    if image_path.is_file() and image_path.stat().st_size != spec["screened_image"]["size_bytes"]:
        raise CalibrationError("screened image archive size does not match contract")
    return spec, manifest


def expected_datasets(manifest: dict) -> dict[tuple[str, int], str]:
    result = {}
    for row in manifest["calibration_datasets"]:
        key = (row["run_size"], int(row["seed"]))
        if key in result:
            raise CalibrationError(f"duplicate calibration dataset {key}")
        result[key] = row["dataset_sha256"]
    if len(result) != 60:
        raise CalibrationError(f"expected 60 pinned datasets, found {len(result)}")
    return result


def valid_reports(root: Path, spec: dict, manifest: dict) -> tuple[dict, list[str]]:
    datasets = expected_datasets(manifest)
    found = {}
    problems = []
    for path in sorted((root / "reports").glob("*-report.json")):
        try:
            report = load_json(path)
            details = report["details"]
            usage = details["token_usage"]
            provider = usage["provider"]
            key = (provider, details["run_size"], int(report["seed"]))
            expected_sha = datasets[(details["run_size"], int(report["seed"]))]
            profile = spec["providers"][provider]
            checks = {
                "bench v5": details.get("bench_version") == 5,
                "dataset digest": details.get("dataset_sha256") == expected_sha,
                "trusted usage source": usage.get("source") == "model_proxy_provider_response",
                "complete usage": usage.get("status") == "complete",
                "provider profile": usage.get("profile_revision") == profile["profile_revision"],
                "provider model": usage.get("model") == profile["model"],
                "all requests metered": usage.get("requests", 0) > 0
                and usage.get("usage_available") == usage.get("requests")
                and usage.get("usage_unavailable") == 0,
            }
            failures = [label for label, okay in checks.items() if not okay]
            if failures:
                raise CalibrationError(", ".join(failures))
            if key in found:
                raise CalibrationError(f"duplicates {found[key].name}")
            found[key] = path
        except (KeyError, TypeError, ValueError, CalibrationError) as exc:
            problems.append(f"{path.name}: {exc}")
    return found, problems


def print_status(root: Path, spec: dict, manifest: dict) -> int:
    found, problems = valid_reports(root, spec, manifest)
    print(f"Pinned contract: scorer {spec['scorer_revision'][:12]}, starter {spec['starter_kit_revision'][:12]}")
    print(f"Formula: {spec['formula_version']} (scoring_enabled={str(manifest['scoring_enabled']).lower()})")
    print("provider    run_size  complete  remaining")
    for provider in spec["providers"]:
        for run_size in RUN_SIZES:
            count = sum(1 for key in found if key[0] == provider and key[1] == run_size)
            print(f"{provider:<11} {run_size:<9} {count:>2}/20     {20 - count:>3}")
    total = len(spec["providers"]) * len(RUN_SIZES) * 20
    print(f"TOTAL                 {len(found):>3}/{total:<3}    {total - len(found):>3}")
    if problems:
        print("\nInvalid reports:", file=sys.stderr)
        for problem in problems:
            print(f"- {problem}", file=sys.stderr)
        return 1
    return 0


def choose_dataset(provider: str, run_size: str | None, seed: int | None, root: Path, spec: dict, manifest: dict):
    datasets = expected_datasets(manifest)
    found, problems = valid_reports(root, spec, manifest)
    if problems:
        raise CalibrationError("fix invalid reports before choosing the next run")
    if seed is not None:
        if run_size is None:
            raise CalibrationError("--seed requires --run-size")
        key = (run_size, seed)
        if key not in datasets:
            raise CalibrationError(f"{run_size} seed {seed} is not in the pinned manifest")
        if (provider, run_size, seed) in found:
            raise CalibrationError(f"{provider}/{run_size}/seed-{seed} is already complete")
        return run_size, seed, datasets[key]
    sizes = (run_size,) if run_size else RUN_SIZES
    for size in sizes:
        for row in manifest["calibration_datasets"]:
            row_seed = int(row["seed"])
            if row["run_size"] == size and (provider, size, row_seed) not in found:
                return size, row_seed, row["dataset_sha256"]
    scope = run_size or "all run sizes"
    raise CalibrationError(f"no remaining {provider} calibration runs in {scope}")


def dotenv(path: Path) -> dict[str, str]:
    values = {}
    try:
        lines = path.read_text().splitlines()
    except OSError as exc:
        raise CalibrationError(f"cannot read credential env file {path}: {exc}") from exc
    pattern = re.compile(r"^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$")
    for raw in lines:
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        match = pattern.match(line)
        if not match:
            continue
        value = match.group(2).strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        values[match.group(1)] = value
    return values


def lan_ip() -> str:
    override = os.environ.get("DITTOBENCH_CALIBRATION_HOST_IP", "").strip()
    if override:
        return override
    probe = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        probe.connect(("8.8.8.8", 80))
        address = probe.getsockname()[0]
    finally:
        probe.close()
    if address.startswith("127.") or address == "0.0.0.0":
        raise CalibrationError("could not detect a non-loopback host IP; set DITTOBENCH_CALIBRATION_HOST_IP")
    return address


def url_json(url: str, *, payload=None, timeout=10):
    data = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request(url, data=data)
    if data is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as exc:
        body = exc.read().decode(errors="replace")
        raise CalibrationError(f"HTTP {exc.code} from {url}: {body[:1000]}") from exc
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as exc:
        raise CalibrationError(f"request failed for {url}: {exc}") from exc


def wait_json(url: str, *, timeout=30):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            return url_json(url, timeout=2)
        except CalibrationError as exc:
            last = exc
            time.sleep(0.25)
    raise CalibrationError(f"service did not become ready at {url}: {last}")


def port_open(port: int) -> bool:
    with socket.socket() as probe:
        probe.settimeout(0.25)
        return probe.connect_ex(("127.0.0.1", port)) == 0


class Services:
    def __init__(self):
        self.started: list[tuple[subprocess.Popen, object]] = []

    def start(self, command: list[str], log_path: Path, *, cwd=None, env=None):
        log_path.parent.mkdir(parents=True, exist_ok=True)
        log = log_path.open("w")
        process = subprocess.Popen(
            command,
            cwd=cwd,
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        self.started.append((process, log))
        return process

    def stop(self):
        for process, _ in reversed(self.started):
            if process.poll() is None:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
        for process, log in reversed(self.started):
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=5)
            log.close()


def build_binaries(root: Path, revision: str) -> tuple[Path, Path]:
    binary_dir = root / "bin" / revision
    relay = binary_dir / "model-relay"
    api = binary_dir / "dittobench-api"
    if relay.is_file() and api.is_file():
        return relay, api
    binary_dir.mkdir(parents=True, exist_ok=True)
    subprocess.run(["go", "build", "-o", relay, "./cmd/model-relay"], cwd=root / "api", check=True)
    subprocess.run(["go", "build", "-o", api, "./cmd/dittobench-api"], cwd=root / "api", check=True)
    return relay, api


def ensure_tools() -> None:
    for tool in ("git", "go", "docker", "ollama"):
        if shutil.which(tool) is None:
            raise CalibrationError(f"required command is missing: {tool}")
    run_output(["docker", "info", "--format", "{{.ServerVersion}}"])


def request_body(root: Path, spec: dict, run_size: str, seed: int, dataset_sha: str, host: str, artifact_port: int = 18080) -> dict:
    source = spec["source"]
    image = spec["screened_image"]
    return {
        "bench_version": 5,
        "tarball_url": f"http://{host}:{artifact_port}/{source['path']}",
        "tarball_sha256": source["sha256"],
        "screened_image_url": f"http://{host}:{artifact_port}/{image['path']}",
        "screened_image_sha256": image["sha256"],
        "screened_image_id": image["image_id"],
        "screened_image_ref": image["image_ref"],
        "screened_image_size_bytes": image["size_bytes"],
        "run_size": run_size,
        "seed": seed,
        "dataset_sha256": dataset_sha,
    }


def ensure_ollama(services: Services, root: Path, host: str, logs: Path) -> None:
    lan_url = f"http://{host}:11434/api/tags"
    try:
        tags = url_json(lan_url, timeout=2)
    except CalibrationError:
        if port_open(11434):
            raise CalibrationError("Ollama is running but is not reachable on the LAN address; restart it with OLLAMA_HOST=0.0.0.0:11434")
        env = os.environ.copy()
        env["OLLAMA_HOST"] = "0.0.0.0:11434"
        services.start(["ollama", "serve"], logs / "ollama.log", env=env)
        tags = wait_json(lan_url, timeout=30)
    names = {model.get("name", "").split(":")[0] for model in tags.get("models", [])}
    if "embeddinggemma" not in names:
        env = os.environ.copy()
        env["OLLAMA_HOST"] = "http://127.0.0.1:11434"
        subprocess.run(["ollama", "pull", "embeddinggemma"], env=env, check=True)
        wait_json(lan_url, timeout=10)


def run_calibration(args, root: Path, spec: dict, manifest: dict) -> int:
    run_size, seed, dataset_sha = choose_dataset(args.provider, args.run_size, args.seed, root, spec, manifest)
    host = lan_ip()
    offset = getattr(args, "port_offset", 0)
    relay_port, api_port, artifact_port = 11435 + offset, 18000 + offset, 18080 + offset
    body = request_body(root, spec, run_size, seed, dataset_sha, host, artifact_port)
    label = f"{args.provider}-{run_size}-seed-{seed}"
    print(f"Selected {label}")
    if args.dry_run:
        print(json.dumps(body, indent=2))
        return 0

    ensure_tools()
    for port in (relay_port, api_port, artifact_port):
        if port_open(port):
            raise CalibrationError(f"required port {port} is already in use")
    profile = spec["providers"][args.provider]
    credentials = dotenv(args.credential_env) if args.credential_env.is_file() else {}
    api_key = os.environ.get(profile["credential_env"]) or credentials.get(profile["credential_env"])
    if not api_key:
        raise CalibrationError(f"missing {profile['credential_env']} in environment or {args.credential_env}")

    relay_binary, api_binary = build_binaries(root, spec["scorer_revision"])
    logs = root / "logs" / label
    services = Services()
    run_id = None
    status = None
    started = time.monotonic()
    try:
        ensure_ollama(services, root, host, logs)

        relay_env = os.environ.copy()
        relay_env.update({"RELAY_PROVIDER": args.provider, "RELAY_API_KEY": api_key, "PORT": str(relay_port)})
        services.start([str(relay_binary)], logs / "model-relay.log", env=relay_env)
        health = wait_json(f"http://{host}:{relay_port}/health", timeout=30)
        if health.get("provider") != args.provider or health.get("profile_revision") != profile["profile_revision"]:
            raise CalibrationError("relay started with the wrong certified profile")

        services.start(
            [sys.executable, "-m", "http.server", str(artifact_port), "--bind", "0.0.0.0"],
            logs / "artifact-server.log",
            cwd=root,
        )
        wait_json(f"http://{host}:{artifact_port}/contract.json", timeout=10)

        api_env = os.environ.copy()
        api_env.update(
            {
                "DITTOBENCH_ALLOW_PRIVATE_HARNESS": "1",
                "DITTOBENCH_ALLOW_SCREENED_IMAGES": "1",
                "DITTOBENCH_ARTIFACT_DIR": str(root / "artifacts"),
                "DITTOBENCH_SOURCE_SHA": spec["scorer_revision"],
                "HARNESS_GATEWAY_URL": f"http://{host}:{relay_port}",
                "HARNESS_EMBED_URL": f"http://{host}:11434",
                "PORT": str(api_port),
            }
        )
        services.start([str(api_binary)], logs / "dittobench-api.log", cwd=root / "api", env=api_env)
        wait_json(f"http://127.0.0.1:{api_port}/health", timeout=30)

        write_json(root / "reports" / f"{label}-request.json", body)
        accepted = url_json(f"http://127.0.0.1:{api_port}/v2/score", payload=body, timeout=30)
        run_id = accepted["run_id"]
        print(f"Run {run_id} accepted; polling {run_size} benchmark…")
        deadline = time.monotonic() + args.timeout_minutes * 60
        last_stage = None
        while time.monotonic() < deadline:
            job = url_json(f"http://127.0.0.1:{api_port}/v1/runs/{run_id}", timeout=15)
            status = job.get("status")
            stage = (job.get("progress") or {}).get("stage", status)
            if stage != last_stage:
                print(f"  {stage}")
                last_stage = stage
            if status in TERMINAL:
                break
            time.sleep(1)
        else:
            raise CalibrationError(f"run exceeded {args.timeout_minutes} minute timeout")
        if status != "done":
            write_json(root / "reports" / f"{label}-job.json", job)
            raise CalibrationError(f"run ended with status {status}: {job.get('failure')}")

        report = job["report"]
        write_json(root / "reports" / f"{label}-job.json", job)
        write_json(root / "reports" / f"{label}-report.json", report)
        transcript = url_json(f"http://127.0.0.1:{api_port}/v1/runs/{run_id}/transcript", timeout=30)
        write_json(root / "reports" / f"{label}-transcript.json", transcript)
        relay_health = url_json(f"http://127.0.0.1:{relay_port}/health", timeout=10)
        write_json(root / "reports" / f"{label}-relay-health.json", relay_health)

        found, problems = valid_reports(root, spec, manifest)
        if problems or (args.provider, run_size, seed) not in found:
            raise CalibrationError("saved run did not pass the pinned report audit: " + "; ".join(problems))
        usage = report["details"]["token_usage"]
        elapsed = time.monotonic() - started
        print(
            f"Done in {elapsed:.1f}s: raw_quality={report['raw_composite']:.6f}, "
            f"prompt={usage['prompt_tokens']:,}, completion={usage['completion_tokens']:,}, "
            f"total={usage['total_tokens']:,}"
        )
        print(f"Report: {root / 'reports' / (label + '-report.json')}")
        return 0
    except KeyboardInterrupt:
        if run_id and status not in TERMINAL:
            try:
                request = urllib.request.Request(f"http://127.0.0.1:{api_port}/v1/runs/{run_id}", method="DELETE")
                urllib.request.urlopen(request, timeout=5).read()
            except Exception:
                pass
        raise CalibrationError("interrupted")
    finally:
        services.stop()


def generate_baseline(root: Path, spec: dict, manifest: dict, output: Path) -> int:
    found, problems = valid_reports(root, spec, manifest)
    if problems:
        raise CalibrationError("invalid reports: " + "; ".join(problems))
    expected = len(spec["providers"]) * len(RUN_SIZES) * 20
    if len(found) != expected:
        raise CalibrationError(f"baseline generation requires all {expected} reports; have {len(found)}, need {expected - len(found)} more")
    command = ["go", "run", "./cmd/tokenbaseline"] + [str(path) for path in sorted(found.values())]
    api_checkout = root / "api"
    if not (api_checkout / "go.mod").is_file():
        api_checkout = REPO_ROOT
    generated = run_output(command, cwd=api_checkout)
    parsed = json.loads(generated)
    expected_groups = len(spec["providers"]) * len(RUN_SIZES)
    if len(parsed.get("baselines", [])) != expected_groups:
        raise CalibrationError(f"baseline generator did not emit all {expected_groups} provider/run-size groups")
    write_json(output, parsed)
    print(f"Wrote audited {expected_groups}-group p90 baseline: {output}")
    return 0


def metric_summary(values: list[int | float]) -> dict:
    ordered = sorted(values)
    p90_index = int(len(ordered) * 0.9 + 0.999999) - 1
    return {
        "min": ordered[0],
        "p50": statistics.median(ordered),
        "p90_nearest_rank": ordered[p90_index],
        "max": ordered[-1],
        "mean": round(statistics.fmean(ordered), 6),
        "sample_stddev": round(statistics.stdev(ordered), 6),
    }


def generate_summary(root: Path, spec: dict, manifest: dict, output: Path) -> int:
    found, problems = valid_reports(root, spec, manifest)
    if problems:
        raise CalibrationError("invalid reports: " + "; ".join(problems))
    expected = len(spec["providers"]) * len(RUN_SIZES) * 20
    if len(found) != expected:
        raise CalibrationError(f"summary requires all {expected} reports; have {len(found)}, need {expected - len(found)} more")
    groups = defaultdict(list)
    for (provider, run_size, _), path in found.items():
        groups[(provider, run_size)].append(load_json(path))
    result = {
        "schema_version": 1,
        "bench_version": 5,
        "formula_version": spec["formula_version"],
        "scorer_revision": spec["scorer_revision"],
        "starter_kit_revision": spec["starter_kit_revision"],
        "samples": len(found),
        "groups": [],
    }
    for provider in sorted(spec["providers"]):
        for run_size in RUN_SIZES:
            reports = groups[(provider, run_size)]
            usages = [report["details"]["token_usage"] for report in reports]
            selected = sorted(
                ((usage["total_tokens"], report["seed"]) for report, usage in zip(reports, usages))
            )[17]
            result["groups"].append(
                {
                    "provider": provider,
                    "profile_revision": spec["providers"][provider]["profile_revision"],
                    "model": spec["providers"][provider]["model"],
                    "run_size": run_size,
                    "samples": len(reports),
                    "p90_selected_seed": selected[1],
                    "tokens": {
                        "prompt": metric_summary([usage["prompt_tokens"] for usage in usages]),
                        "completion": metric_summary([usage["completion_tokens"] for usage in usages]),
                        "total": metric_summary([usage["total_tokens"] for usage in usages]),
                    },
                    "prompt_bytes": metric_summary([usage["prompt_bytes"] for usage in usages]),
                    "provider_latency_ms": metric_summary([usage["provider_latency_ms"] for usage in usages]),
                    "quality": {
                        "raw_composite": metric_summary([report["raw_composite"] for report in reports]),
                        "tool_mean": metric_summary([report["tool_mean"] for report in reports]),
                        "memory_mean": metric_summary([report["memory_mean"] for report in reports]),
                    },
                    "metering": {
                        "requests": sum(usage["requests"] for usage in usages),
                        "usage_available": sum(usage["usage_available"] for usage in usages),
                        "usage_unavailable": sum(usage["usage_unavailable"] for usage in usages),
                    },
                }
            )
    write_json(output, result)
    print(f"Wrote audited calibration distribution summary: {output}")
    return 0


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--root", type=Path, default=Path(os.environ.get("DITTOBENCH_CALIBRATION_ROOT", DEFAULT_ROOT)))
    sub = result.add_subparsers(dest="command", required=True)
    sub.add_parser("status", help="validate reports and show completion by provider/run size")
    run = sub.add_parser("run", help="run exactly one missing pinned calibration case")
    run.add_argument("--provider", required=True, help="provider key pinned in contract.json")
    run.add_argument("--run-size", choices=RUN_SIZES)
    run.add_argument("--seed", type=int, help="explicit pinned seed; requires --run-size")
    run.add_argument("--next", action="store_true", help="select first missing seed (the default when --seed is omitted)")
    run.add_argument("--dry-run", action="store_true", help="validate and print the selected request without provider calls")
    run.add_argument("--credential-env", type=Path, default=Path(os.environ.get("DITTOBENCH_CREDENTIAL_ENV", DEFAULT_CREDENTIAL_ENV)))
    run.add_argument("--timeout-minutes", type=int, default=45)
    batch = sub.add_parser("batch", help="resume all missing pinned cases after explicit authorization")
    batch.add_argument("--provider", required=True, help="provider key pinned in contract.json, or all")
    batch.add_argument("--remaining", action="store_true", help="confirm all remaining paid cases")
    batch.add_argument("--credential-env", type=Path, default=Path(os.environ.get("DITTOBENCH_CREDENTIAL_ENV", DEFAULT_CREDENTIAL_ENV)))
    batch.add_argument("--timeout-minutes", type=int, default=45)
    batch.add_argument("--max-attempts", type=int, default=3)
    batch.add_argument("--workers", type=int, default=8, help="isolated concurrent relay/API lanes")
    baseline = sub.add_parser("baseline", help="emit the audited p90 manifest after all 120 runs")
    baseline.add_argument("--output", type=Path)
    summary = sub.add_parser("summary", help="summarize all six token and quality distributions")
    summary.add_argument("--output", type=Path)
    return result


def main() -> int:
    args = parser().parse_args()
    root = args.root.expanduser().resolve()
    try:
        require_runtime = args.command == "batch" or (args.command == "run" and not args.dry_run)
        spec, manifest = contract(root, require_runtime=require_runtime)
        if args.command == "status":
            return print_status(root, spec, manifest)
        if args.command == "run":
            if args.provider not in spec["providers"]:
                raise CalibrationError(f"provider {args.provider!r} is not pinned in contract.json")
            if args.next and args.seed is not None:
                raise CalibrationError("use either --next or --seed, not both")
            return run_calibration(args, root, spec, manifest)
        if args.command == "batch":
            if args.provider != "all" and args.provider not in spec["providers"]:
                raise CalibrationError(f"provider {args.provider!r} is not pinned in contract.json")
            if not args.remaining:
                raise CalibrationError("batch requires --remaining to confirm paid provider work")
            if args.max_attempts < 1:
                raise CalibrationError("--max-attempts must be positive")
            if args.workers < 1 or args.workers > 12:
                raise CalibrationError("--workers must be between 1 and 12")
            completed, problems = valid_reports(root, spec, manifest)
            if problems:
                raise CalibrationError("fix invalid reports before batching: " + "; ".join(problems))
            providers = list(spec["providers"]) if args.provider == "all" else [args.provider]
            jobs = []
            datasets = expected_datasets(manifest)
            for row in manifest["calibration_datasets"]:
                for provider in providers:
                    key = (provider, row["run_size"], int(row["seed"]))
                    if key not in completed:
                        jobs.append((provider, row["run_size"], int(row["seed"]), datasets[(row["run_size"], int(row["seed"]))]))
            build_binaries(root, spec["scorer_revision"])

            def run_one(index_job):
                index, (provider, run_size, seed, _) = index_job
                last_error = None
                for attempt in range(1, args.max_attempts + 1):
                    run_args = argparse.Namespace(
                        provider=provider,
                        run_size=run_size,
                        seed=seed,
                        dry_run=False,
                        credential_env=args.credential_env,
                        timeout_minutes=args.timeout_minutes,
                        port_offset=(index + 1) * 200,
                    )
                    try:
                        run_calibration(run_args, root, spec, manifest)
                        return
                    except CalibrationError as exc:
                        last_error = exc
                        print(f"{provider}/{run_size}/{seed} attempt {attempt}/{args.max_attempts} failed: {exc}", file=sys.stderr)
                raise CalibrationError(f"persistent calibration failure for {provider}/{run_size}/{seed}: {last_error}")

            with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as executor:
                futures = [executor.submit(run_one, item) for item in enumerate(jobs)]
                for future in concurrent.futures.as_completed(futures):
                    future.result()
            return print_status(root, spec, manifest)
        if args.command == "baseline":
            output = args.output or root / "reports/baselines_v5.generated.json"
            return generate_baseline(root, spec, manifest, output.expanduser().resolve())
        output = args.output or root / "summary.json"
        return generate_summary(root, spec, manifest, output.expanduser().resolve())
    except (CalibrationError, KeyError, subprocess.CalledProcessError, json.JSONDecodeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
