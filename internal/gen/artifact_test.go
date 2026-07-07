package gen

import (
	"context"
	"testing"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// artifactFor assembles the DatasetArtifact the pipeline hashes, from a
// deterministic (nil-LLM) generation of the same (seed, n).
func artifactFor(seed int64, n int) DatasetArtifact {
	suite := GenerateMemorySuite(context.Background(), NewRNG(11), seed, n, 0, 2, 0.3, nil, "")
	flat := make([]protocol.MemoryCase, 0, len(suite.Cases))
	for _, sc := range suite.Cases {
		flat = append(flat, sc.Case)
	}
	return DatasetArtifact{
		Seed:         seed,
		BenchVersion: protocol.BenchVersion,
		GeneratedAt:  protocol.DatasetEpochRFC3339,
		MemoryWaves:  suite.Waves,
		MemoryCases:  flat,
	}
}

// TestDatasetHashStable checks a hash is stable across repeated hashing of the
// same artifact bytes (the trivial dispute-replay direction of gate 6).
func TestDatasetHashStable(t *testing.T) {
	a := artifactFor(1234, 20)
	h1, b1, err := a.SHA256Hex()
	if err != nil {
		t.Fatal(err)
	}
	h2, b2, err := a.SHA256Hex()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || string(b1) != string(b2) {
		t.Fatal("hash/bytes not stable across repeated hashing")
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64-hex-char sha256, got %d", len(h1))
	}
}

// TestDatasetHashReproducibleFromSeed is gate 6 (deterministic-render side):
// same (seed, bench_version) with no LLM surface variation ⇒ identical
// dataset ⇒ identical dataset_sha256.
func TestDatasetHashReproducibleFromSeed(t *testing.T) {
	h1, _, _ := artifactFor(999, 30).SHA256Hex()
	h2, _, _ := artifactFor(999, 30).SHA256Hex()
	if h1 != h2 {
		t.Fatal("same seed produced different dataset hashes")
	}
	// A different seed must (overwhelmingly) produce a different hash.
	h3, _, _ := artifactFor(1000, 30).SHA256Hex()
	if h1 == h3 {
		t.Fatal("distinct seeds produced identical dataset hashes")
	}
}
