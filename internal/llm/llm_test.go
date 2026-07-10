package llm

import "testing"

func TestHarnessModelDefault(t *testing.T) {
	t.Setenv("HARNESS_MODEL", "")
	if got := HarnessModel(); got != defaultHarnessModel {
		t.Fatalf("harness model default: got %q want %q", got, defaultHarnessModel)
	}
	t.Setenv("HARNESS_MODEL", "qwen/qwen3-next")
	if got := HarnessModel(); got != "qwen/qwen3-next" {
		t.Fatalf("HARNESS_MODEL override: got %q", got)
	}
}
