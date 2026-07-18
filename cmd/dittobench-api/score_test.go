package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// newScoreTestServer returns a server that skips the rate limiter (allowPrivate)
// so the precondition branches of handleScore are reached directly. The
// preconditions all return before submitRunSize, so no store/sandbox is needed.
func newScoreTestServer() *server {
	return &server{allowPrivate: true}
}

func TestValidateBenchVersionResultRejectsContradictions(t *testing.T) {
	if err := validateBenchVersionResult(3, 3, &protocol.RunDetails{BenchVersion: 3}); err != nil {
		t.Fatalf("matching v3 result rejected: %v", err)
	}
	for _, tc := range []struct {
		artifact int
		details  *protocol.RunDetails
	}{
		{artifact: 2, details: &protocol.RunDetails{BenchVersion: 3}},
		{artifact: 3, details: &protocol.RunDetails{BenchVersion: 2}},
		{artifact: 3, details: nil},
	} {
		if err := validateBenchVersionResult(3, tc.artifact, tc.details); err == nil {
			t.Fatalf("accepted contradictory result: artifact=%d details=%+v", tc.artifact, tc.details)
		}
	}
}

// TestHandleScorePreconditions pins the canonical validator path's required
// fields: a pinned seed, the platform dataset_sha256, a run_size, and exactly one
// harness source. Each missing/invalid field is a distinct 400.
func TestHandleScorePreconditions(t *testing.T) {
	s := newScoreTestServer()
	cases := []struct {
		name, body, wantSubstr string
	}{
		{"bad json", `{`, "invalid or oversized JSON body"},
		{"missing seed", `{"run_size":"full","dataset_sha256":"abc","harness_url":"http://h"}`, "seed is required"},
		{"missing hash", `{"seed":42,"run_size":"full","harness_url":"http://h"}`, "dataset_sha256 is required"},
		{"missing run_size", `{"seed":42,"dataset_sha256":"abc","harness_url":"http://h"}`, "run_size is required"},
		{"no source", `{"seed":42,"run_size":"full","dataset_sha256":"abc"}`, "exactly one of"},
		{"two sources", `{"seed":42,"run_size":"full","dataset_sha256":"abc","harness_url":"http://h","git_url":"http://g"}`, "exactly one of"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/score", strings.NewReader(c.body))
			s.handleScore(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (body %s)", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), c.wantSubstr) {
				t.Fatalf("expected error to mention %q, got %s", c.wantSubstr, rr.Body.String())
			}
		})
	}
}

func TestVersionedScoreRequiresSupportedBenchVersion(t *testing.T) {
	s := newScoreTestServer()
	base := `"seed":42,"run_size":"full","dataset_sha256":"abc","harness_url":"http://h"`
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "omitted", body: `{` + base + `}`, want: "bench_version is required"},
		{name: "old v1", body: `{"bench_version":1,` + base + `}`, want: "unsupported bench_version"},
		{name: "future", body: `{"bench_version":4,` + base + `}`, want: "unsupported bench_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v2/score", strings.NewReader(tc.body))
			s.handleVersionedScore(rr, req)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), tc.want) {
				t.Fatalf("expected 400 containing %q, got %d %s", tc.want, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestRequestedBenchVersionLegacyCompatibility(t *testing.T) {
	if got, msg := requestedBenchVersion(0, false); got != 2 || msg != "" {
		t.Fatalf("legacy omitted version must select exact v2, got (%d, %q)", got, msg)
	}
	for _, version := range []int{2, 3} {
		if got, msg := requestedBenchVersion(version, true); got != version || msg != "" {
			t.Fatalf("explicit v%d rejected: (%d, %q)", version, got, msg)
		}
	}
}
