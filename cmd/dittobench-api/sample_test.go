package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-datagen/gen"
)

func getSample(t *testing.T, s *server, query string) (*httptest.ResponseRecorder, gen.DatasetArtifact) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sample"+query, nil)
	s.handleSample(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", rr.Code, rr.Body.String())
	}
	var art gen.DatasetArtifact
	if err := json.Unmarshal(rr.Body.Bytes(), &art); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rr.Body.String())
	}
	return rr, art
}

// TestSampleUsesPublicNegativeSeed pins the load-bearing anti-gaming guarantee:
// the sampler's seed is always in the reserved negative namespace, disjoint from
// every (non-negative) per-submission seed, so a sample can never be a scored
// dataset.
func TestSampleUsesPublicNegativeSeed(t *testing.T) {
	s := &server{}
	for i := 0; i <= maxSampleIndex; i++ {
		_, art := getSample(t, s, "?sample="+itoa(i))
		if art.Seed >= 0 {
			t.Fatalf("sample %d seed %d is not negative (would risk colliding with a real seed)", i, art.Seed)
		}
		if art.Seed != publicSampleSeed(i) {
			t.Fatalf("sample %d seed mismatch: got %d want %d", i, art.Seed, publicSampleSeed(i))
		}
	}
}

// TestSampleIncludesAnswerKeys asserts the sample is the FULL artifact, answer
// keys included. The sampler is un-redacted on purpose: with the generator public
// the full dataset is derivable anyway, so the transparency window shows the same
// bytes a validator scores.
func TestSampleIncludesAnswerKeys(t *testing.T) {
	s := &server{}
	rr, art := getSample(t, s, "?run_size=full")
	raw := rr.Body.String()
	// The grading fields that redaction used to strip must now be present.
	for _, want := range []string{
		"expected_tools", "expected_answer", "tool_fixtures",
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("sample should carry answer-key field %q but did not: %s", want, raw)
		}
	}
	if len(art.ToolCases) == 0 || len(art.MemoryCases) == 0 {
		t.Fatalf("expected tool + memory cases in the sample, got %d/%d", len(art.ToolCases), len(art.MemoryCases))
	}
	if len(art.ToolFixtures) == 0 {
		t.Fatal("expected tool fixtures (the served needle facts) in the full sample")
	}
}

// TestSampleShapeMatchesProfile checks the sample is a real full-profile dataset
// (60 tool cases), so the community sees the real size, and it pins the
// frozen-v2 / hardened-v3 split at the API surface.
func TestSampleShapeMatchesProfile(t *testing.T) {
	s := &server{}

	// v2 is the FROZEN contract: 4 read-path isolation cases (the isoCases
	// quota) and nothing else user-scoped. The cross-user lifecycle probe is
	// v3-only, so if it ever shows up here, v2's bytes have drifted and every
	// run already scored under v2 has become unauditable.
	_, v2 := getSample(t, s, "?run_size=full&bench_version=2")
	if len(v2.ToolCases) != 60 {
		t.Fatalf("full profile should have 60 tool cases, got %d", len(v2.ToolCases))
	}
	if got := userScopedCases(v2); got != 4 {
		t.Fatalf("v2 should have 4 user-scoped cases (frozen), got %d", got)
	}

	// v3 adds the 5 cross-user LIFECYCLE cases (B3): a write and a delete under
	// user A, and the reads under user B that must be unaffected by them.
	_, v3 := getSample(t, s, "?run_size=full&bench_version=3")
	if got := userScopedCases(v3); got != 9 {
		t.Fatalf("v3 should have 9 user-scoped cases (4 isolation + 5 cross-user), got %d", got)
	}
	art := v3
	if len(art.MemoryWaves) < 1 {
		t.Fatalf("expected at least one memory wave, got %d", len(art.MemoryWaves))
	}
}

// TestSampleDeterministic: the same (run_size, sample) yields identical bytes.
// userScopedCases counts cases carrying an explicit memory graph.
func userScopedCases(art gen.DatasetArtifact) int {
	n := 0
	for _, c := range art.MemoryCases {
		if c.UserID != "" {
			n++
		}
	}
	return n
}

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
		{"unsupported bench version", "?bench_version=5", "unsupported bench_version"},
		{"non-integer bench version", "?bench_version=latest", "bench_version must be an integer"},
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

// TestSampleDefaultsToSmall: no run_size defaults to the small profile (6 tools).
func TestSampleDefaultsToSmall(t *testing.T) {
	s := &server{}
	_, art := getSample(t, s, "")
	if len(art.ToolCases) != 6 {
		t.Fatalf("small profile should have 6 tool cases, got %d", len(art.ToolCases))
	}
	if art.BenchVersion != 2 {
		t.Fatalf("omitted practice version must remain v2, got %d", art.BenchVersion)
	}
}

func TestSampleCanDeliberatelySelectV3(t *testing.T) {
	s := &server{}
	v2, art2 := getSample(t, s, "?run_size=small&sample=2&bench_version=2")
	v3, art3 := getSample(t, s, "?run_size=small&sample=2&bench_version=3")
	if art2.BenchVersion != 2 || art3.BenchVersion != 3 {
		t.Fatalf("wrong versions: v2=%d v3=%d", art2.BenchVersion, art3.BenchVersion)
	}
	if v2.Body.String() == v3.Body.String() {
		t.Fatal("v2 and v3 sample paths produced identical bytes")
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
