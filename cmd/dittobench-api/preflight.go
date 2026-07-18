package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/refharness"
	"github.com/ditto-assistant/dittobench-api/internal/runner"
	"github.com/ditto-assistant/dittobench-api/internal/scorer"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
	"github.com/ditto-assistant/dittobench-datagen/toolexec"
)

// preflightPrompt instructs a harness that treats the probe as an ordinary case.
// Compliant harnesses key off the case_id prefix instead (PROTOCOL.md), so the
// probe does not depend on a model deciding to follow the prompt.
const preflightPrompt = "Reachability preflight: call the search_web tool once through the advertised tool endpoint with any query (for example \"preflight\"), then reply \"ok\". This turn only verifies the tool endpoint is reachable; it is not scored."

// preflightAttempts bounds how many probe turns are sent before the endpoint is
// declared unreachable. More than one attempt keeps a single transient harness
// hiccup from failing (and rescheduling) an otherwise healthy scored run.
const preflightAttempts = 3

// preflightRetryPause separates probe attempts.
const preflightRetryPause = time.Second

// probeToolReachability verifies that the advertised tool_endpoint is reachable
// from the harness's network namespace, not merely served by the validator. It
// registers a reserved preflight case on the mock tool server, sends the probe
// turn, and checks whether the server observed any call for it. Zero observed
// calls after every attempt means the endpoint is advertised but unreachable
// (Docker routing, network policy, a runtime fault) or the harness is not
// preflight-compliant; either way, observable cases could never be watched
// execute.
func probeToolReachability(ctx context.Context, harnessURL, toolEndpoint string, toolSrv *toolexec.Server, seed int64, tools []protocol.ToolDefinition, runID string) error {
	id := refharness.PreflightPrefix + runID
	toolSrv.Register(id, toolexec.BuildFixture(seed, protocol.ToolCase{ID: id}))
	var lastErr error
	for attempt := 0; attempt < preflightAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(preflightRetryPause):
			}
		}
		if _, err := runner.RunCase(ctx, harnessURL, id, preflightPrompt, tools, runner.CaseOptions{ToolEndpoint: toolEndpoint}); err != nil {
			lastErr = err
		}
		if len(toolSrv.Observed(id)) > 0 {
			return nil
		}
	}
	if lastErr != nil {
		return fmt.Errorf("no tool call observed across %d probe turns (last harness error: %w)", preflightAttempts, lastErr)
	}
	return fmt.Errorf("no tool call observed across %d probe turns", preflightAttempts)
}

// enforcePreflight runs the reachability probe and applies the scope policy to
// a failure: a scored run is failed (rescheduled by the platform) rather than
// completed as an indefensible zeroed report, while a practice run continues
// with observable cases capped exactly as before. Returns false when the run
// was failed and the pipeline must stop.
func (s *server) enforcePreflight(ctx context.Context, runID string, scope scorer.Scope, harnessURL, toolEndpoint string, toolSrv *toolexec.Server, seed int64, tools []protocol.ToolDefinition) bool {
	err := probeToolReachability(ctx, harnessURL, toolEndpoint, toolSrv, seed, tools, runID)
	if err == nil {
		return true
	}
	if scope == scorer.ScopeScored {
		s.store.Fail(runID, "scored run cannot observe tool execution: tool_endpoint advertised but unreachable from the harness ("+err.Error()+"). The reachability preflight (PROTOCOL.md) must observe one tool call before scoring; failing the run rather than emitting a zeroed report.")
		log.Printf("run %s: scored run aborted — reachability preflight failed: %v", runID, err)
		return false
	}
	log.Printf("run %s: reachability preflight failed (%v); observable tool cases will score capped (practice)", runID, err)
	return true
}
