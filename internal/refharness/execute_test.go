package refharness_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/catalog"
	"github.com/ditto-assistant/dittobench-api/internal/refharness"
	"github.com/ditto-assistant/dittobench-api/internal/scorer"
	"github.com/ditto-assistant/dittobench-api/internal/toolexec"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// End-to-end of Phase C observed execution across three packages: the reference
// harness routes a prompt, EXECUTES the routed tools through the validator's mock
// endpoint, the server OBSERVES the trajectory, and the scorer grades the
// observation (not the self-report). Also checks the served needle flows back so
// the harness's answer can incorporate it (result-usage substrate).
func TestObservedExecutionRoundTrip(t *testing.T) {
	c := protocol.ToolCase{
		ID:            "web_search-7-0",
		Category:      "web_search",
		ExpectedTools: []protocol.ToolSpec{{Name: "search_web"}},
		MaxToolCalls:  1,
	}
	fixture := toolexec.BuildFixture(7, c)

	srv := toolexec.NewServer()
	srv.Register(c.ID, fixture)
	mux := http.NewServeMux()
	mux.Handle("POST /tool", srv)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	endpoint := ts.URL + "/tool"

	// The harness routes the prompt to a tool, then executes it through the endpoint.
	prompt := "Search the web for the Veltrix index."
	calls := refharness.Route(prompt, catalog.Catalog())
	if len(calls) != 1 || calls[0].Name != "search_web" {
		t.Fatalf("expected route to search_web, got %v", calls)
	}
	result, err := refharness.Execute(context.Background(), endpoint, c.ID, "", calls)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if nv := fixture.NeedleValue(); nv == "" || !strings.Contains(result, nv) {
		t.Fatalf("served result %q should carry needle value %q", result, nv)
	}

	// The validator scores what it OBSERVED, and it saw the real call.
	observed := srv.Observed(c.ID)
	if len(observed) != 1 || observed[0].Name != "search_web" {
		t.Fatalf("expected 1 observed search_web, got %v", observed)
	}
	resp := protocol.RunResponse{FinalText: result, ToolCalls: calls}
	cs := scorer.ScoreToolCaseObserved(c, resp, true, observed)
	if cs.ToolScore == 0 {
		t.Fatalf("observed correct trajectory should score > 0, got %v", cs.ToolScore)
	}
}

// A harness that ignores the endpoint (endpoint unreachable / old harness) makes
// no observed calls, so an observable case is capped — even with a perfect
// self-reported trajectory and answer.
func TestUnobservedObservableCaseCapped(t *testing.T) {
	c := protocol.ToolCase{
		ID:            "web_search-7-1",
		Category:      "web_search",
		ExpectedTools: []protocol.ToolSpec{{Name: "search_web"}},
		MaxToolCalls:  1,
	}
	if !toolexec.Observable(c) {
		t.Fatal("web_search case should be observable")
	}
	// Perfect self-report, but nothing observed.
	self := protocol.RunResponse{FinalText: "ok", ToolCalls: []protocol.ObservedToolCall{{Name: "search_web"}}}
	cs := scorer.ScoreToolCaseObserved(c, self, true, nil)
	cs = scorer.ComposeTool(cs, 1.0)
	cs = scorer.CapUnobserved(cs)
	if cs.Score > scorer.UnobservedCeiling {
		t.Fatalf("unobserved observable case must be capped at %v, got %v", scorer.UnobservedCeiling, cs.Score)
	}
}
