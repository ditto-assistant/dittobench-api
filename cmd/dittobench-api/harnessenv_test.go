package main

import (
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
)

// TestHarnessSandboxEnvForcesModel: the frozen model + provider are forced and
// no OpenRouter key is forwarded. HARNESS_MODEL is set to a bogus id to prove
// the frozen model wins (it is not env-tunable).
func TestHarnessSandboxEnvForcesModel(t *testing.T) {
	t.Setenv("HARNESS_MODEL", "qwen/qwen2.5-72b-instruct")
	env := harnessSandboxEnv(nil)
	if _, ok := env["OPENROUTER_API_KEY"]; ok {
		t.Fatal("locked path must NOT forward the OpenRouter key")
	}
	if env["DITTOBENCH_MODEL"] != llm.LockedHarnessModel {
		t.Fatalf("locked model = %q, want the frozen %q", env["DITTOBENCH_MODEL"], llm.LockedHarnessModel)
	}
	if env["DITTOBENCH_PROVIDER"] != lockedProvider {
		t.Fatalf("locked provider = %q, want the frozen %q", env["DITTOBENCH_PROVIDER"], lockedProvider)
	}
}

// TestHarnessSandboxEnvRelayRouting: chat routes to the chat gateway via the
// frozen chutes provider while embeddings keep hitting the local Ollama, and no
// real key enters the sandbox.
func TestHarnessSandboxEnvRelayRouting(t *testing.T) {
	t.Setenv("HARNESS_GATEWAY_URL", "http://host.docker.internal:11435")
	t.Setenv("HARNESS_EMBED_URL", "http://host.docker.internal:11434")
	env := harnessSandboxEnv(nil)
	if env["DITTOBENCH_PROVIDER"] != lockedProvider || env["DITTOBENCH_MODEL"] != llm.LockedHarnessModel {
		t.Fatalf("locked provider/model wrong: %v", env)
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
	env := harnessSandboxEnv(hostile)
	// Keys with no locked value must be dropped entirely; the OpenAI + OpenRouter
	// selectors are never set on the locked path.
	for _, k := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		if _, ok := env[k]; ok {
			t.Fatalf("req.Env must not be able to set %s under lock", k)
		}
	}
	// The chutes selectors are set by the lock, so the attacker's values must be
	// overwritten with the locked placeholder / gateway, never survive.
	if env["CHUTES_API_KEY"] != "relay" {
		t.Fatalf("req.Env kept a real Chutes key under lock: %q", env["CHUTES_API_KEY"])
	}
	if env["CHUTES_BASE_URL"] == "http://attacker.example/v1" {
		t.Fatal("req.Env redirected the chat gateway under lock")
	}
	if env["DITTOBENCH_MODEL"] != llm.LockedHarnessModel {
		t.Fatalf("req.Env overrode the locked model: %q", env["DITTOBENCH_MODEL"])
	}
	if env["DITTOBENCH_PROVIDER"] != lockedProvider {
		t.Fatalf("req.Env overrode the locked provider: %q", env["DITTOBENCH_PROVIDER"])
	}
	if env["OLLAMA_BASE_URL"] == "http://attacker.example/v1" {
		t.Fatal("req.Env redirected the gateway URL under lock")
	}
	if env["BENIGN"] != "ok" {
		t.Fatalf("non-locked caller env should still pass through, got %q", env["BENIGN"])
	}
}
