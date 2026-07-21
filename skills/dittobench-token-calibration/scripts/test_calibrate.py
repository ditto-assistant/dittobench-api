import importlib.util
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


if __name__ == "__main__":
    unittest.main()
