package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newScoreTestServer returns a server that skips the rate limiter (allowPrivate)
// so the precondition branches of handleScore are reached directly. The
// preconditions all return before submitRunSize, so no store/sandbox is needed.
func newScoreTestServer() *server {
	return &server{allowPrivate: true}
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
