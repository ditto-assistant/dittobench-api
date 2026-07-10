package main

import "testing"

// TestHarnessSandboxEnvLegacy: with the lock off, the OpenRouter key + provider
// are forwarded and caller-supplied env is honored (pre-lock BYOK behavior).
func TestHarnessSandboxEnvLegacy(t *testing.T) {
	t.Setenv("DITTOBENCH_MODEL_LOCK", "")
	t.Setenv("DITTOBENCH_PROVIDER", "")
	t.Setenv("CHUTES_API_KEY", "")
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

// TestHarnessSandboxEnvLegacyChutes: with the lock off, the operator can point
// the crate at Chutes; the provider selection and Chutes settings pass through.
func TestHarnessSandboxEnvLegacyChutes(t *testing.T) {
	t.Setenv("DITTOBENCH_MODEL_LOCK", "")
	t.Setenv("DITTOBENCH_PROVIDER", "chutes")
	t.Setenv("CHUTES_API_KEY", "cpk-operator")
	t.Setenv("CHUTES_BASE_URL", "https://llm.chutes.ai/v1")
	env := harnessSandboxEnv("", nil)
	if env["DITTOBENCH_PROVIDER"] != "chutes" {
		t.Fatalf("provider = %q, want chutes", env["DITTOBENCH_PROVIDER"])
	}
	if env["CHUTES_API_KEY"] != "cpk-operator" || env["CHUTES_BASE_URL"] != "https://llm.chutes.ai/v1" {
		t.Fatalf("Chutes settings must pass through on the legacy path: %v", env)
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

// TestHarnessSandboxEnvLockedChutesRelay: with the lock on and the relay as
// the gateway, chat routes to the relay via the chutes provider while
// embeddings keep hitting the local Ollama, and no real key enters the sandbox.
func TestHarnessSandboxEnvLockedChutesRelay(t *testing.T) {
	t.Setenv("DITTOBENCH_MODEL_LOCK", "1")
	t.Setenv("HARNESS_MODEL", "Qwen/Qwen3-32B-TEE")
	t.Setenv("HARNESS_PROVIDER", "chutes")
	t.Setenv("HARNESS_GATEWAY_URL", "http://host.docker.internal:11435")
	t.Setenv("HARNESS_EMBED_URL", "http://host.docker.internal:11434")
	env := harnessSandboxEnv("sk-secret", nil)
	if env["DITTOBENCH_PROVIDER"] != "chutes" || env["DITTOBENCH_MODEL"] != "Qwen/Qwen3-32B-TEE" {
		t.Fatalf("locked chutes provider/model wrong: %v", env)
	}
	if env["CHUTES_BASE_URL"] != "http://host.docker.internal:11435" {
		t.Fatalf("chat must route to the relay: %q", env["CHUTES_BASE_URL"])
	}
	if env["OLLAMA_BASE_URL"] != "http://host.docker.internal:11434" {
		t.Fatalf("embeddings must keep hitting local Ollama: %q", env["OLLAMA_BASE_URL"])
	}
	if env["CHUTES_API_KEY"] != "relay" {
		t.Fatalf("sandbox must hold only the placeholder key, got %q", env["CHUTES_API_KEY"])
	}
	if _, ok := env["OPENROUTER_API_KEY"]; ok {
		t.Fatal("locked path must not forward the OpenRouter key")
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
		"CHUTES_API_KEY":      "cpk-attacker",
		"CHUTES_BASE_URL":     "http://attacker.example/v1",
		"OPENAI_API_KEY":      "sk-attacker",
		"OPENAI_BASE_URL":     "http://attacker.example/v1",
		"BENIGN":              "ok", // non-locked keys still pass through
	}
	env := harnessSandboxEnv("sk-server", hostile)
	for _, k := range []string{"OPENROUTER_API_KEY", "CHUTES_API_KEY", "CHUTES_BASE_URL", "OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		if _, ok := env[k]; ok {
			t.Fatalf("req.Env must not be able to set %s under lock", k)
		}
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
