import importlib.util
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock


MODULE_PATH = Path(__file__).with_name("calibrate.py")
SPEC = importlib.util.spec_from_file_location("token_calibrate", MODULE_PATH)
calibrate = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(calibrate)


class CalibrationToolTest(unittest.TestCase):
    def test_nearest_rank_p90_uses_eighteenth_of_twenty(self):
        summary = calibrate.metric_summary(list(range(1, 21)))
        self.assertEqual(summary["p50"], 10.5)
        self.assertEqual(summary["p90_nearest_rank"], 18)
        self.assertEqual(summary["min"], 1)
        self.assertEqual(summary["max"], 20)

    def test_next_dataset_skips_completed_provider_seed(self):
        manifest = {"calibration_datasets": []}
        for run_size in calibrate.RUN_SIZES:
            for index in range(1, 21):
                seed = index * 101
                manifest["calibration_datasets"].append(
                    {"run_size": run_size, "seed": seed, "dataset_sha256": f"{run_size}-{seed}"}
                )
        completed = {("chutes", "small", 101): Path("first.json")}
        with mock.patch.object(calibrate, "valid_reports", return_value=(completed, [])):
            self.assertEqual(
                calibrate.choose_dataset("chutes", "small", None, Path("."), {}, manifest),
                ("small", 202, "small-202"),
            )

    def test_request_binds_all_screened_artifact_fields(self):
        spec = {
            "source": {"path": "source.tgz", "sha256": "source-sha"},
            "screened_image": {
                "path": "image.tar",
                "sha256": "image-sha",
                "image_id": "sha256:image-id",
                "image_ref": "screened:ref",
                "size_bytes": 42,
            },
        }
        body = calibrate.request_body(Path("."), spec, "full", 2020, "dataset-sha", "10.0.0.2")
        self.assertEqual(body["bench_version"], 5)
        self.assertEqual(body["run_size"], "full")
        self.assertEqual(body["seed"], 2020)
        self.assertEqual(body["dataset_sha256"], "dataset-sha")
        self.assertEqual(body["screened_image_id"], "sha256:image-id")
        self.assertEqual(body["screened_image_size_bytes"], 42)

    def test_dotenv_reads_credentials_without_executing_shell(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "provider.env"
            path.write_text("CHUTES_API_KEY='literal-$DO_NOT_EXPAND'\n")
            self.assertEqual(calibrate.dotenv(path)["CHUTES_API_KEY"], "literal-$DO_NOT_EXPAND")

    def test_audit_excludes_canceled_requests_but_meters_every_success(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            reports = root / "reports"
            reports.mkdir()
            report = {
                "run_id": "run-1",
                "seed": 101,
                "details": {
                    "bench_version": 5,
                    "run_size": "small",
                    "dataset_sha256": "dataset-sha",
                    "token_usage": {
                        "source": "model_proxy_provider_response",
                        "status": "complete",
                        "provider": "openrouter",
                        "profile_revision": "profile-v1",
                        "model": "model-v1",
                        "requests": 11,
                        "successes": 10,
                        "usage_available": 10,
                        "usage_unavailable": 0,
                        "prompt_tokens": 100,
                        "completion_tokens": 10,
                        "total_tokens": 110,
                    },
                },
            }
            (reports / "openrouter-small-seed-101-report.json").write_text(json.dumps(report))
            spec = {"providers": {"openrouter": {"profile_revision": "profile-v1", "model": "model-v1"}}}
            with mock.patch.object(calibrate, "expected_datasets", return_value={("small", 101): "dataset-sha"}):
                found, problems = calibrate.valid_reports(root, spec, {})
            self.assertFalse(problems)
            self.assertIn(("openrouter", "small", 101), found)

    def test_enabled_baseline_candidate_requires_explicit_flag(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "api").mkdir()
            (root / "api" / "go.mod").touch()
            output = root / "candidate.json"
            found = {
                (provider, run_size, seed): root / f"{provider}-{run_size}-{seed}.json"
                for provider in ("chutes", "openrouter")
                for run_size in calibrate.RUN_SIZES
                for seed in range(20)
            }
            generated = json.dumps({
                "scoring_enabled": True,
                "baselines": [{"id": str(index)} for index in range(6)],
            })
            with mock.patch.object(calibrate, "valid_reports", return_value=(found, [])), \
                 mock.patch.object(calibrate, "run_output", return_value=generated) as run_output:
                calibrate.generate_baseline(
                    root,
                    {"providers": {"chutes": {}, "openrouter": {}}},
                    {},
                    output,
                    enable_scoring=True,
                )
            self.assertIn("-enable-scoring", run_output.call_args.args[0])
            self.assertTrue(json.loads(output.read_text())["scoring_enabled"])

    def test_summary_exports_observations_without_per_case_content(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            output = root / "summary.json"
            found = {}
            reports = {}
            for provider in ("chutes", "openrouter"):
                for run_size in calibrate.RUN_SIZES:
                    for seed in range(1, 21):
                        path = root / f"{provider}-{run_size}-{seed}.json"
                        found[(provider, run_size, seed)] = path
                        reports[path] = {
                            "seed": seed,
                            "raw_composite": 0.5,
                            "tool_mean": 0.6,
                            "memory_mean": 0.4,
                            "per_case": [{"private": "must not be exported"}],
                            "details": {
                                "dataset_sha256": f"dataset-{run_size}-{seed}",
                                "token_usage": {
                                    "prompt_tokens": 100 + seed,
                                    "completion_tokens": 10,
                                    "total_tokens": 110 + seed,
                                    "prompt_bytes": 400 + seed,
                                    "provider_latency_ms": 1_000 + seed,
                                    "requests": 2,
                                    "successes": 2,
                                    "usage_available": 2,
                                    "usage_unavailable": 0,
                                },
                            },
                        }
            spec = {
                "formula_version": "formula-v1",
                "scorer_revision": "scorer-sha",
                "starter_kit_revision": "starter-sha",
                "providers": {
                    "chutes": {"profile_revision": "chutes-v1", "model": "chutes-model"},
                    "openrouter": {"profile_revision": "openrouter-v1", "model": "openrouter-model"},
                },
            }
            with mock.patch.object(calibrate, "valid_reports", return_value=(found, [])), \
                 mock.patch.object(calibrate, "load_json", side_effect=lambda path: reports[path]):
                calibrate.generate_summary(root, spec, {}, output)
            summary = json.loads(output.read_text())
            self.assertEqual(summary["schema_version"], 2)
            self.assertEqual(len(summary["groups"]), 6)
            self.assertEqual(len(summary["groups"][0]["observations"]), 20)
            self.assertNotIn("per_case", json.dumps(summary))


if __name__ == "__main__":
    unittest.main()
