package gen

import (
	"context"
	"sort"
	"testing"

	"github.com/ditto-assistant/dittobench-api/pkg/toolexec"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// artifactFor assembles the full DatasetArtifact the pipeline hashes, from a
// deterministic (nil-LLM) generation of the same (seed, n): tool cases, their
// mock-tool fixtures, the memory waves and cases, and the multi-graph isolation
// layer. It mirrors runSizeJob's assembly so the reproducibility test exercises
// every generator whose output the digest depends on — not memory cases alone.
func artifactFor(seed int64, n int) DatasetArtifact {
	ctx := context.Background()
	r := NewRNG(seed)
	toolCases, _ := GenerateTools(ctx, r, seed, n, 0, nil, "")
	suite := GenerateMemorySuite(ctx, r, seed, n, 0, 2, 0.3, nil, "")
	iso := GenerateIsolation(ctx, r, seed, n, 2, 4)
	suite.Cases = append(suite.Cases, iso.Cases...)

	flat := make([]ArtifactCase, 0, len(suite.Cases))
	for _, sc := range suite.Cases {
		flat = append(flat, ArtifactCase{MemoryCase: sc.Case, UserID: sc.UserID, RunAfterWave: sc.RunAfterWave})
	}
	fixtures := make([]FixtureDigest, 0, len(toolCases))
	for _, c := range toolCases {
		f := toolexec.BuildFixture(seed, c)
		fixtures = append(fixtures, FixtureDigest{CaseID: c.ID, Needle: f.NeedleText()})
	}
	sort.Slice(fixtures, func(i, j int) bool { return fixtures[i].CaseID < fixtures[j].CaseID })
	memWaves := suite.Waves
	if len(iso.SecondaryWave.Pairs) > 0 {
		memWaves = append(append([]protocol.SeedRequest{}, suite.Waves...), iso.SecondaryWave)
	}
	return DatasetArtifact{
		Seed:         seed,
		BenchVersion: protocol.BenchVersion,
		GeneratedAt:  protocol.DatasetEpochRFC3339,
		ToolCases:    toolCases,
		MemoryWaves:  memWaves,
		MemoryCases:  flat,
		ToolFixtures: fixtures,
	}
}

// TestDatasetHashStable checks a hash is stable across repeated hashing of the
// same artifact bytes (the trivial dispute-replay direction).
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

// TestDatasetHashReproducibleFromSeed is the deterministic-render side:
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
