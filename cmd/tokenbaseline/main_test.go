package main

import (
	"reflect"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/efficiency"
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

func TestV7StarterKitRevisionMustBeCanonicalGitSHA(t *testing.T) {
	for _, revision := range []string{
		"", "not-a-revision", "60AAB4E5E2839DDB0FE8C80492BD7B76BA2668FD",
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"0000000000000000000000000000000000000000",
	} {
		if _, err := calibrationManifest(protocol.BenchVersionV7, revision); err == nil {
			t.Errorf("accepted noncanonical starter revision %q", revision)
		}
	}
	manifest, err := calibrationManifest(protocol.BenchVersionV7, "60aab4e5e2839ddb0fe8c80492bd7b76ba2668fd")
	if err != nil {
		t.Fatalf("rejected canonical starter revision: %v", err)
	}
	if manifest.DatasetKnownVector != "1cfc6e3b9f3f4c04afe04b058a6851f9357f6463170b879867e2cf4588f58fcf" {
		t.Fatalf("v7 known vector = %s", manifest.DatasetKnownVector)
	}
	for _, dataset := range manifest.Calibration {
		if dataset.DatasetSHA256 == "" {
			t.Fatalf("v7 dataset hash is empty for %s/%d", dataset.RunSize, dataset.Seed)
		}
	}
}

func TestSortBaselinesUsesFullRouteIdentityBeforeRunSize(t *testing.T) {
	baselines := []efficiency.Baseline{
		{Provider: "groq", ProfileRevision: "b", Model: "m1", RunSize: "small"},
		{Provider: "amazon", ProfileRevision: "z", Model: "m1", RunSize: "full"},
		{Provider: "groq", ProfileRevision: "a", Model: "m2", RunSize: "full"},
		{Provider: "groq", ProfileRevision: "a", Model: "m1", RunSize: "small"},
		{Provider: "groq", ProfileRevision: "a", Model: "m1", RunSize: "full"},
	}
	sortBaselines(baselines)
	got := make([]string, 0, len(baselines))
	for _, b := range baselines {
		got = append(got, b.Provider+"/"+b.ProfileRevision+"/"+b.Model+"/"+b.RunSize)
	}
	want := []string{
		"amazon/z/m1/full", "groq/a/m1/full", "groq/a/m1/small",
		"groq/a/m2/full", "groq/b/m1/small",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("baseline order = %v, want %v", got, want)
	}
}

func TestRefreshedDatasetManifestUsesCanonicalV7Profile(t *testing.T) {
	manifest, err := calibrationManifest(protocol.BenchVersionV7, "60aab4e5e2839ddb0fe8c80492bd7b76ba2668fd")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err = refreshedDatasetManifestForVersion(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.BenchVersion != protocol.BenchVersionV7 || manifest.ScoringEnabled || len(manifest.Baselines) != 0 {
		t.Fatalf("v7 refresh must fail closed: %#v", manifest)
	}
	if manifest.DatasetKnownVector != "1cfc6e3b9f3f4c04afe04b058a6851f9357f6463170b879867e2cf4588f58fcf" {
		t.Fatalf("v7 known vector = %s", manifest.DatasetKnownVector)
	}
	if len(manifest.Calibration) != 60 {
		t.Fatalf("v7 calibration datasets = %d, want 60", len(manifest.Calibration))
	}
}
