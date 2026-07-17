package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// transcriptCase is one graded case's full graded inputs: the RunResponse
// exactly as the grader saw it (for memory cases the validator-observed
// trajectory is already substituted into tool_calls) plus the raw observed
// trajectory from the mock tool endpoint. Together with the regenerable
// dataset, this is everything a third party needs to re-run the public grader
// and reproduce the case's score.
type transcriptCase struct {
	CaseID   string                      `json:"case_id"`
	Kind     string                      `json:"kind"`
	UserID   string                      `json:"user_id,omitempty"`
	Response protocol.RunResponse        `json:"response"`
	Observed []protocol.ObservedToolCall `json:"observed,omitempty"`
}

// transcriptArtifact is the canonical, content-addressed record of a run's
// graded inputs. The digest of its canonical bytes is published with the run
// (run status `transcript_sha256`; the platform embeds it in the signed score
// payload) so the artifact cannot be swapped after the fact.
type transcriptArtifact struct {
	RunID         string           `json:"run_id"`
	Seed          int64            `json:"seed"`
	BenchVersion  int              `json:"bench_version"`
	DatasetSHA256 string           `json:"dataset_sha256"`
	Cases         []transcriptCase `json:"cases"`
}

// canonicalBytes returns the artifact's canonical JSON encoding and its
// SHA-256 hex digest. Cases are sorted by case_id so the bytes are independent
// of per-case completion order under bounded concurrency.
func (a transcriptArtifact) canonicalBytes() (string, []byte, error) {
	sort.Slice(a.Cases, func(i, j int) bool { return a.Cases[i].CaseID < a.Cases[j].CaseID })
	body, err := json.Marshal(a)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), body, nil
}
