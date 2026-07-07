package gen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// DatasetArtifact is the canonical, hashable snapshot of a fully-rendered run
// dataset (BENCHMARK-V2 §4.2). Its SHA-256 goes into ScoreReport.details as
// dataset_sha256 and pins the exact bytes a dispute re-scores. Field order is
// fixed and the payload contains no Go maps, so json.Marshal is byte-stable —
// hashing the same artifact always yields the same digest, and (with no LLM
// surface variation) the same (seed, bench_version) yields the same digest.
type DatasetArtifact struct {
	Seed         int64                  `json:"seed"`
	BenchVersion int                    `json:"bench_version"`
	GeneratedAt  string                 `json:"generated_at"`
	ToolCases    []protocol.ToolCase    `json:"tool_cases"`
	MemoryWaves  []protocol.SeedRequest `json:"memory_waves"`
	MemoryCases  []protocol.MemoryCase  `json:"memory_cases"`
	// ToolFixtures pins the Phase C mock-tool content the validator served per
	// case (§7). The content is a pure function of (seed, case), but recording the
	// coined "needle" facts makes the served world explicit in the dispute
	// artifact so a re-score sees identical tool results. Sorted by CaseID (no Go
	// map) so the JSON stays byte-stable. Omitted before Phase C.
	ToolFixtures []FixtureDigest `json:"tool_fixtures,omitempty"`
}

// FixtureDigest is the hashable snapshot of one case's mock-tool environment: the
// case id and the coined needle its content tools embed (empty for cases whose
// served tools return only a confirmation).
type FixtureDigest struct {
	CaseID string `json:"case_id"`
	Needle string `json:"needle,omitempty"`
}

// Marshal returns the canonical JSON bytes of the artifact.
func (a DatasetArtifact) Marshal() ([]byte, error) {
	return json.Marshal(a)
}

// SHA256Hex returns the hex SHA-256 of the artifact's canonical JSON, plus those
// bytes (for optional persistence/upload). A marshal error is returned rather
// than panicking so hashing never crashes a sweep.
func (a DatasetArtifact) SHA256Hex() (string, []byte, error) {
	b, err := a.Marshal()
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), b, nil
}
