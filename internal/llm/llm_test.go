package llm

import "testing"

func TestModelDefaults(t *testing.T) {
	t.Setenv("GENERATOR_MODEL", "")
	t.Setenv("SCORER_MODEL", "")
	if got := GeneratorModel(); got != defaultGeneratorModel {
		t.Fatalf("generator default: got %q want %q", got, defaultGeneratorModel)
	}
	if got := ScorerModel(); got != defaultScorerModel {
		t.Fatalf("scorer default: got %q want %q", got, defaultScorerModel)
	}
}

func TestModelEnvOverride(t *testing.T) {
	t.Setenv("GENERATOR_MODEL", "acme/gen-1")
	t.Setenv("SCORER_MODEL", "acme/judge-1")
	if got := GeneratorModel(); got != "acme/gen-1" {
		t.Fatalf("generator override: got %q", got)
	}
	if got := ScorerModel(); got != "acme/judge-1" {
		t.Fatalf("scorer override: got %q", got)
	}
}

func TestNewRequiresKey(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	if _, err := New(); err == nil {
		t.Fatal("New() must error when the OpenRouter key is unset")
	}
	t.Setenv(EnvAPIKey, "sk-test")
	c, err := New()
	if err != nil || c == nil {
		t.Fatalf("New() with key: %v", err)
	}
}
