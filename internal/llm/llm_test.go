package llm

import "testing"

// TestHarnessModelFrozen: the locked model id is a code constant, not env-set,
// so HARNESS_MODEL cannot change it. Every validator scores against the same
// model.
func TestHarnessModelFrozen(t *testing.T) {
	t.Setenv("HARNESS_MODEL", "qwen/qwen3-next")
	if got := HarnessModel(); got != LockedHarnessModel {
		t.Fatalf("HARNESS_MODEL must not override the frozen model: got %q want %q", got, LockedHarnessModel)
	}
}
