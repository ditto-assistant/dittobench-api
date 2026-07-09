package main

import "testing"

// TestHarnessSandboxEnvLegacy: with the lock off, the OpenRouter key + provider
// are forwarded and caller-supplied env is honored (pre-lock BYOK behavior).
func TestHarnessSandboxEnvLegacy(t *testing.T) {
	t.Setenv("DITTOBENCH_MODEL_LOCK", "")
	env := harnessSandboxEnv("sk-test-key", map[string]string{"FOO": "bar"})
	if env["OPENROUTER_API_KEY"] != "sk-test-key" {
		t.Fatalf("legacy path must forward the OpenRouter key, got %q", env["OPENROUTER_API_KEY"])
	}
	if env["DITTOBENCH_PROVIDER"] != "openrouter" {
		t.Fatalf("legacy provider = %q, want openrouter", env["DITTOBENCH_PROVIDER"])
	}
	if env["FOO"] != "bar" {
		t.Fatalf("caller env should pass through when unlocked, got %q", env["FOO"])
	}
}

// TestHarnessSandboxEnvLockedForcesModel: with the lock on, the locked model +
// provider are forced and no OpenRouter key is forwarded.
func TestHarnessSandboxEnvLockedForcesModel(t *testing.T) {
	t.Setenv("DITTOBENCH_MODEL_LOCK", "1")
	t.Setenv("HARNESS_MODEL", "qwen/qwen2.5-72b-instruct")
	env := harnessSandboxEnv("sk-secret", nil)
	if _, ok := env["OPENROUTER_API_KEY"]; ok {
		t.Fatal("locked path must NOT forward the OpenRouter key")
	}
	if env["DITTOBENCH_MODEL"] != "qwen/qwen2.5-72b-instruct" {
		t.Fatalf("locked model = %q, want the Qwen2.5 lock", env["DITTOBENCH_MODEL"])
	}
	if env["DITTOBENCH_PROVIDER"] != "ollama" {
		t.Fatalf("locked provider = %q, want the gateway provider", env["DITTOBENCH_PROVIDER"])
	}
}

// TestHarnessSandboxEnvLockCannotBeOverridden pins the security-critical
// invariant: a malicious req.Env cannot escape the locked model by setting the
// OpenRouter key, swapping the model id, or redirecting the gateway.
func TestHarnessSandboxEnvLockCannotBeOverridden(t *testing.T) {
	t.Setenv("DITTOBENCH_MODEL_LOCK", "1")
	t.Setenv("HARNESS_MODEL", "qwen/qwen2.5-72b-instruct")
	hostile := map[string]string{
		"OPENROUTER_API_KEY":  "sk-attacker",
		"DITTOBENCH_MODEL":    "openai/gpt-4o",
		"DITTOBENCH_PROVIDER": "openrouter",
		"OLLAMA_BASE_URL":     "http://attacker.example/v1",
		"BENIGN":              "ok", // non-locked keys still pass through
	}
	env := harnessSandboxEnv("sk-server", hostile)
	if _, ok := env["OPENROUTER_API_KEY"]; ok {
		t.Fatal("req.Env must not be able to inject an OpenRouter key under lock")
	}
	if env["DITTOBENCH_MODEL"] != "qwen/qwen2.5-72b-instruct" {
		t.Fatalf("req.Env overrode the locked model: %q", env["DITTOBENCH_MODEL"])
	}
	if env["DITTOBENCH_PROVIDER"] != "ollama" {
		t.Fatalf("req.Env overrode the locked provider: %q", env["DITTOBENCH_PROVIDER"])
	}
	if env["OLLAMA_BASE_URL"] == "http://attacker.example/v1" {
		t.Fatal("req.Env redirected the gateway URL under lock")
	}
	if env["BENIGN"] != "ok" {
		t.Fatalf("non-locked caller env should still pass through, got %q", env["BENIGN"])
	}
}
