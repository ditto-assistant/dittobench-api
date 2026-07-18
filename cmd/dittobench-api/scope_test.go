package main

import (
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/scorer"
)

// runScope classifies the canonical /v1/score path (dataset_sha256 pinned) as
// SCORED and a plain /v1/submit run_size as PRACTICE. This is the scored-vs-
// practice boundary that drives mandatory observed execution and the closed
// parser free points.
func TestRunScope(t *testing.T) {
	if got := runScope(submitRequest{RunSize: "full", HarnessURL: "http://h"}); got != scorer.ScopePractice {
		t.Fatalf("run_size submit without dataset_sha256 must be practice, got %v", got)
	}
	if got := runScope(submitRequest{RunSize: "full", HarnessURL: "http://h", ExpectedDatasetSHA256: "abc123"}); got != scorer.ScopeScored {
		t.Fatalf("pinned dataset_sha256 (/v1/score) must be scored, got %v", got)
	}
	// Whitespace-only hash is not a real pin.
	if got := runScope(submitRequest{RunSize: "full", ExpectedDatasetSHA256: "   "}); got != scorer.ScopePractice {
		t.Fatalf("blank dataset_sha256 must be practice, got %v", got)
	}
}
