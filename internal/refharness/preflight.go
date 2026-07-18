package refharness

import (
	"encoding/json"
	"strings"

	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// PreflightPrefix is the reserved case_id prefix for the validator's
// reachability preflight turn (PROTOCOL.md "Reachability preflight"). A case
// whose id carries this prefix is not scored; its only purpose is to prove the
// advertised tool_endpoint is reachable from the harness's network namespace.
// On seeing it a harness MUST issue one call to a served catalog tool through
// tool_endpoint (search_web with any args is sufficient) before responding.
const PreflightPrefix = "preflight:"

// IsPreflight reports whether the case is a reachability preflight turn.
func IsPreflight(caseID string) bool { return strings.HasPrefix(caseID, PreflightPrefix) }

// PreflightCalls is the reference implementation of the preflight requirement:
// one search_web call, executed through tool_endpoint. Deterministic, so the
// calibration harness's difficulty signal is unaffected.
func PreflightCalls() []protocol.ObservedToolCall {
	return []protocol.ObservedToolCall{{Name: "search_web", Args: json.RawMessage(`{"query":"preflight"}`)}}
}
