package main

import (
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

// TestHarnessSandboxEnvForcesModel: the frozen model + provider are forced and
// no OpenRouter key is forwarded. HARNESS_MODEL is set to a bogus id to prove
// the frozen model wins (it is not env-tunable).
func TestHarnessSandboxEnvForcesModel(t *testing.T) {
	t.Setenv("HARNESS_MODEL", "qwen/qwen2.5-72b-instruct")
	env := harnessSandboxEnv(nil, protocol.BenchVersionV6)
	if _, ok := env["OPENROUTER_API_KEY"]; ok {
		t.Fatal("locked path must NOT forward the OpenRouter key")
	}
	if env["DITTOBENCH_MODEL"] != llm.LockedHarnessModel {
		t.Fatalf("locked model = %q, want the frozen %q", env["DITTOBENCH_MODEL"], llm.LockedHarnessModel)
	}
	if env["DITTOBENCH_PROVIDER"] != legacyLockedProvider {
		t.Fatalf("locked provider = %q, want the frozen %q", env["DITTOBENCH_PROVIDER"], legacyLockedProvider)
	}
	if env["DITTOBENCH_DB"] != "/tmp/dittobench.db" {
		t.Fatalf("sandbox DB path = %q, want bounded writable tmpfs", env["DITTOBENCH_DB"])
	}
}

func TestSandboxRuntimeEnvLocksWritableDBWithoutChangingProvider(t *testing.T) {
	env := sandboxRuntimeEnv(map[string]string{
		"DITTOBENCH_DB":      "/app/read-only.db",
		"OPENROUTER_API_KEY": "practice-key",
	})
	if env["DITTOBENCH_DB"] != "/tmp/dittobench.db" {
		t.Fatalf("sandbox DB path = %q, want bounded writable tmpfs", env["DITTOBENCH_DB"])
	}
	if env["OPENROUTER_API_KEY"] != "practice-key" {
		t.Fatal("practice provider env was unexpectedly changed")
	}
}

// TestHarnessSandboxEnvRelayRouting: chat routes to the chat gateway via the
// frozen chutes provider while embeddings keep hitting the local Ollama, and no
// real key enters the sandbox.
func TestHarnessSandboxEnvRelayRouting(t *testing.T) {
	t.Setenv("HARNESS_GATEWAY_URL", "http://host.docker.internal:11435")
	t.Setenv("HARNESS_EMBED_URL", "http://host.docker.internal:11434")
	env := harnessSandboxEnv(nil, protocol.BenchVersionV6)
	if env["DITTOBENCH_PROVIDER"] != legacyLockedProvider || env["DITTOBENCH_MODEL"] != llm.LockedHarnessModel {
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

func TestTicketInferenceEnvContainsNoSessionCapability(t *testing.T) {
	t.Setenv("HARNESS_EMBED_URL", "")
	env := harnessSandboxEnv(nil, protocol.BenchVersionV7, "secret-session-route")
	if got := env["DITTOBENCH_INFERENCE_BASE_URL"]; got != "http://host.docker.internal:11436/v1/inference" {
		t.Fatalf("ticket gateway = %q", got)
	}
	if env["DITTOBENCH_PROVIDER"] != platformLockedProvider {
		t.Fatalf("v7 provider = %q, want %q", env["DITTOBENCH_PROVIDER"], platformLockedProvider)
	}
	if _, ok := env["CHUTES_API_KEY"]; ok {
		t.Fatal("v7 must not expose the retired compatibility key")
	}
	for key, value := range env {
		if key != "BENIGN" && strings.Contains(value, "secret-session-route") {
			t.Fatalf("session capability leaked through %s", key)
		}
	}
	if env["DITTOBENCH_MODEL"] != llm.V7HarnessModel {
		t.Fatalf("v7 model = %q, want %q", env["DITTOBENCH_MODEL"], llm.V7HarnessModel)
	}
	if got := env["OLLAMA_BASE_URL"]; got != "http://host.docker.internal:11434" {
		t.Fatalf("v7 embeddings must remain on the isolated embedding service, got %q", got)
	}
}

// TestHarnessSandboxEnvLockCannotBeOverridden pins the security-critical
// invariant: a malicious req.Env cannot escape the locked model by setting the
// OpenRouter key, swapping the model id, or redirecting the gateway.
func TestHarnessSandboxEnvLockCannotBeOverridden(t *testing.T) {
	hostile := map[string]string{
		"OPENROUTER_API_KEY":            "sk-attacker",
		"DITTOBENCH_MODEL":              "openai/gpt-4o",
		"DITTOBENCH_PROVIDER":           "openrouter",
		"OLLAMA_BASE_URL":               "http://attacker.example/v1",
		"CHUTES_API_KEY":                "cpk-attacker",
		"CHUTES_BASE_URL":               "http://attacker.example/v1",
		"OPENAI_API_KEY":                "sk-attacker",
		"OPENAI_BASE_URL":               "http://attacker.example/v1",
		"DITTOBENCH_INFERENCE_BASE_URL": "http://attacker.example/v1",
		"DITTOBENCH_DB":                 "/app/attacker.db",
		"BENIGN":                        "ok", // non-locked keys still pass through
	}
	env := harnessSandboxEnv(hostile, protocol.BenchVersionV6)
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
	if env["DITTOBENCH_PROVIDER"] != legacyLockedProvider {
		t.Fatalf("req.Env overrode the locked provider: %q", env["DITTOBENCH_PROVIDER"])
	}
	if env["OLLAMA_BASE_URL"] == "http://attacker.example/v1" {
		t.Fatal("req.Env redirected the gateway URL under lock")
	}
	if env["BENIGN"] != "ok" {
		t.Fatalf("non-locked caller env should still pass through, got %q", env["BENIGN"])
	}
	if env["DITTOBENCH_DB"] != "/tmp/dittobench.db" {
		t.Fatalf("req.Env overrode the locked DB path: %q", env["DITTOBENCH_DB"])
	}
}
