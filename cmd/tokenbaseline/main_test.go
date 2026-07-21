package main

import (
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func TestRefreshedDatasetManifestUsesCanonicalV5Profile(t *testing.T) {
	manifest, err := refreshedDatasetManifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ScoringEnabled || len(manifest.Baselines) != 0 {
		t.Fatalf("refresh must fail closed: %#v", manifest)
	}
	if manifest.DatasetKnownVector != "ee70387b2470bb72a7ce457cd76187b9d89819016f3d58276f895a55b30a9f1c" {
		t.Fatalf("known vector = %s", manifest.DatasetKnownVector)
	}
	if len(manifest.Calibration) != 60 {
		t.Fatalf("calibration datasets = %d, want 60", len(manifest.Calibration))
	}
	for _, dataset := range manifest.Calibration {
		if dataset.RunSize == "small" && dataset.Seed == 101 {
			if dataset.DatasetSHA256 != "e00ae322831f23e4b4cdeaaf895c1d5c2eed0f8f6f2871dcefd9cd6a30edf5fd" {
				t.Fatalf("small/101 hash = %s", dataset.DatasetSHA256)
			}
			return
		}
	}
	t.Fatalf("small/101 calibration dataset missing for bench v%d", protocol.BenchVersionV5)
}
