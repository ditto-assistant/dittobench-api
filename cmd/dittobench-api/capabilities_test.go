package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/efficiency"
	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

const testSourceRevision = "0123456789abcdef0123456789abcdef01234567"

func TestCapabilitiesReportBoundReleaseIdentity(t *testing.T) {
	s := &server{softwareVersion: "0.10.0", sourceRevision: testSourceRevision}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	s.handleCapabilities(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got capabilitiesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SoftwareVersion != "0.10.0" || got.SourceRevision != testSourceRevision {
		t.Fatalf("wrong identity: %+v", got)
	}
	if got.FullRunCapacity != maxConcurrentRuns {
		t.Fatalf("full-run capacity = %d, want %d", got.FullRunCapacity, maxConcurrentRuns)
	}
	if got.MemoryPhaseCapacity != maxConcurrentMemoryPhases {
		t.Fatalf("memory-phase capacity = %d, want %d", got.MemoryPhaseCapacity, maxConcurrentMemoryPhases)
	}
	if efficiency.ValidV7CalibrationReadiness(got.V7Calibration) || got.V7Calibration.ManifestSHA256 != "" {
		t.Fatalf("hosted v7 calibration escaped dark gate: %+v", got.V7Calibration)
	}
	if strings.Contains(rr.Body.String(), `"profile_revision":"openrouter-route-a471cd87ae7df5b9-v1"`) {
		t.Fatalf("pre-campaign v7 route leaked into capabilities: %s", rr.Body.String())
	}
	// v2-v4 are always advertised; v5 is negotiated only once reviewed token
	// baselines make efficiency.ProductionReady() true (the #54 release gate).
	want := []int{2, 3, 4}
	if efficiency.ProductionReady() {
		want = append(want, 5)
	}
	if efficiency.ProductionReadyForVersion(6) {
		want = append(want, 6)
	}
	// v7 is production-ready under the quality-only contract, but this server has
	// not flipped the DITTOBENCH_ENABLE_V7 activation switch, so it must not
	// advertise v7.
	if s.enableV7 && efficiency.ProductionReadyForVersion(7) {
		want = append(want, 7)
	}
	if len(got.SupportedBenchVersions) != len(want) {
		t.Fatalf("wrong supported versions: %v (want %v)", got.SupportedBenchVersions, want)
	}
	for i, v := range want {
		if got.SupportedBenchVersions[i] != v {
			t.Fatalf("wrong supported versions: %v (want %v)", got.SupportedBenchVersions, want)
		}
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("capabilities response must not be cached")
	}
}

// The embedded quality-only manifest makes v7 production-ready at the
// efficiency layer, but v7 must stay dark in a scorer that has not flipped the
// DITTOBENCH_ENABLE_V7 activation switch (and must advertise once it has).
func TestV7ActivationSwitchGatesCapability(t *testing.T) {
	if !efficiency.ProductionReadyForVersion(7) {
		t.Fatal("embedded quality-only v7 manifest must be production-ready")
	}

	dark := &server{softwareVersion: "0.10.0", sourceRevision: testSourceRevision}
	rr := httptest.NewRecorder()
	dark.handleCapabilities(rr, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	var got capabilitiesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, v := range got.SupportedBenchVersions {
		if v == protocol.BenchVersionV7 {
			t.Fatal("v7 advertised without the activation switch")
		}
	}
	if efficiency.ValidV7CalibrationReadiness(got.V7Calibration) || got.V7Calibration.ManifestSHA256 != "" {
		t.Fatalf("v7 calibration leaked without the activation switch: %+v", got.V7Calibration)
	}

	lit := &server{softwareVersion: "0.10.0", sourceRevision: testSourceRevision, enableV7: true}
	rr = httptest.NewRecorder()
	lit.handleCapabilities(rr, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range got.SupportedBenchVersions {
		if v == protocol.BenchVersionV7 {
			found = true
		}
	}
	if !found {
		t.Fatalf("v7 not advertised with the activation switch set: %v", got.SupportedBenchVersions)
	}
	if !efficiency.ValidV7CalibrationReadiness(got.V7Calibration) {
		t.Fatalf("activated v7 must expose a valid calibration readiness: %+v", got.V7Calibration)
	}
}

func TestV7CalibrationReadinessRequiresManifestAndExactRoutes(t *testing.T) {
	readiness := efficiency.CalibrationReadiness{
		ManifestSHA256: strings.Repeat("a", 64),
		SupportedRoutes: []efficiency.CalibrationRouteIdentity{{
			Provider: "openrouter", ProfileRevision: "openrouter-route-a471cd87ae7df5b9-v1",
			Model: llm.V7HarnessModel,
		}},
	}
	if !efficiency.ValidV7CalibrationReadiness(readiness) {
		t.Fatal("complete v7 calibration identity rejected")
	}
	readiness.SupportedRoutes[0].ProfileRevision = "openrouter-route-8efde5ce9f5a4e58-v1"
	if efficiency.ValidV7CalibrationReadiness(readiness) {
		t.Fatal("v7 calibration identity accepted superseded aggregate route")
	}
	readiness.SupportedRoutes[0].ProfileRevision = "openrouter-route-a471cd87ae7df5b9-v1"
	readiness.ManifestSHA256 = ""
	if efficiency.ValidV7CalibrationReadiness(readiness) {
		t.Fatal("v7 calibration identity accepted without reviewed manifest digest")
	}
	readiness.ManifestSHA256 = strings.Repeat("a", 64)
	readiness.SupportedRoutes[0].Provider = "groq"
	if efficiency.ValidV7CalibrationReadiness(readiness) {
		t.Fatal("v7 calibration identity accepted an unreviewed provider route")
	}
	readiness.SupportedRoutes[0].Provider = "openrouter"
	readiness.SupportedRoutes = append(readiness.SupportedRoutes, readiness.SupportedRoutes[0])
	if efficiency.ValidV7CalibrationReadiness(readiness) {
		t.Fatal("v7 calibration identity accepted an extra route")
	}
}

func TestCapabilitiesFailClosedOnUnboundIdentity(t *testing.T) {
	for _, revision := range []string{"", "not-a-sha", "0123456789ABCDEF0123456789ABCDEF01234567", "0000000000000000000000000000000000000000"} {
		s := &server{softwareVersion: "0.10.0", sourceRevision: revision}
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
		s.handleCapabilities(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("revision %q: expected 503, got %d", revision, rr.Code)
		}
	}
}
