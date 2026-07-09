package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getSample(t *testing.T, s *server, query string) (*httptest.ResponseRecorder, datasetSample) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sample"+query, nil)
	s.handleSample(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", rr.Code, rr.Body.String())
	}
	var ds datasetSample
	if err := json.Unmarshal(rr.Body.Bytes(), &ds); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rr.Body.String())
	}
	return rr, ds
}

// TestSampleUsesPublicNegativeSeed pins the load-bearing anti-gaming guarantee:
// the sampler's seed is always in the reserved negative namespace, disjoint from
// every (non-negative) per-submission seed, so a sample can never be a scored
// dataset.
func TestSampleUsesPublicNegativeSeed(t *testing.T) {
	s := &server{}
	for i := 0; i <= maxSampleIndex; i++ {
		_, ds := getSample(t, s, "?sample="+itoa(i))
		if ds.Seed >= 0 {
			t.Fatalf("sample %d seed %d is not negative (would risk colliding with a real seed)", i, ds.Seed)
		}
		if ds.Sample != i {
			t.Fatalf("sample index mismatch: got %d want %d", ds.Sample, i)
		}
	}
}

// TestSampleRedactsAnswerKey asserts the sample never carries grading data.
func TestSampleRedactsAnswerKey(t *testing.T) {
	s := &server{}
	rr, ds := getSample(t, s, "?run_size=full")
	raw := rr.Body.String()
	// Answer-key JSON fields that must never appear. ("memory_waves" is NOT
	// listed: the sample exposes only the wave *count* as shape, never the seeded
	// pairs — the actual haystack facts live under a "pairs" array we drop.)
	for _, leaked := range []string{
		"expected_tools", "expected_answer", "forbidden_answer",
		"expected_behavior", "tool_fixtures", "\"pairs\"", "needle",
	} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("sample leaked answer-key field %q: %s", leaked, raw)
		}
	}
	// ...but the harness-visible shape IS present.
	if len(ds.ToolCases) == 0 || len(ds.MemoryCases) == 0 {
		t.Fatalf("expected tool + memory cases in the sample, got %d/%d", len(ds.ToolCases), len(ds.MemoryCases))
	}
	if ds.ToolCases[0].Prompt == "" {
		t.Fatalf("expected a visible prompt on the first tool case")
	}
}

// TestSampleShapeCountsMatchProfile checks the shape summary reflects the full
// profile (60 tool + 50 memory + 4 isolation), so the community sees real size.
func TestSampleShapeCountsMatchProfile(t *testing.T) {
	s := &server{}
	_, ds := getSample(t, s, "?run_size=full")
	if ds.Shape.ToolCaseCount != len(ds.ToolCases) {
		t.Fatalf("tool count mismatch: shape %d vs cases %d", ds.Shape.ToolCaseCount, len(ds.ToolCases))
	}
	if ds.Shape.ToolCaseCount != 60 {
		t.Fatalf("full profile should have 60 tool cases, got %d", ds.Shape.ToolCaseCount)
	}
	if ds.Shape.IsolationCases != 4 {
		t.Fatalf("full profile should have 4 isolation cases, got %d", ds.Shape.IsolationCases)
	}
	if ds.Shape.MemoryWaves < 1 {
		t.Fatalf("expected at least one memory wave, got %d", ds.Shape.MemoryWaves)
	}
	if len(ds.Shape.ToolCategories) == 0 {
		t.Fatalf("expected a tool-category histogram")
	}
}

// TestSampleDeterministic: the same (run_size, sample) yields identical bytes.
func TestSampleDeterministic(t *testing.T) {
	s := &server{}
	rr1, _ := getSample(t, s, "?run_size=medium&sample=3")
	rr2, _ := getSample(t, s, "?run_size=medium&sample=3")
	if rr1.Body.String() != rr2.Body.String() {
		t.Fatal("sampler is not deterministic for the same (run_size, sample)")
	}
	// A different sample index gives a different dataset.
	rr3, _ := getSample(t, s, "?run_size=medium&sample=4")
	if rr1.Body.String() == rr3.Body.String() {
		t.Fatal("different sample indices produced identical datasets")
	}
}

func TestSampleRejectsBadInput(t *testing.T) {
	s := &server{}
	cases := []struct{ name, query, wantSubstr string }{
		{"bad run_size", "?run_size=huge", "run_size must be one of"},
		{"index too high", "?sample=99", "sample must be between"},
		{"negative index", "?sample=-1", "sample must be between"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/sample"+c.query, nil)
			s.handleSample(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (body %s)", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), c.wantSubstr) {
				t.Fatalf("expected error to mention %q, got %s", c.wantSubstr, rr.Body.String())
			}
		})
	}
}

// TestSampleDefaultsToSmall: no run_size defaults to the small profile.
func TestSampleDefaultsToSmall(t *testing.T) {
	s := &server{}
	_, ds := getSample(t, s, "")
	if ds.RunSize != "small" {
		t.Fatalf("expected default run_size small, got %q", ds.RunSize)
	}
	if ds.Shape.ToolCaseCount != 6 {
		t.Fatalf("small profile should have 6 tool cases, got %d", ds.Shape.ToolCaseCount)
	}
}

// itoa is a tiny local int->string without importing strconv into the test twice.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
