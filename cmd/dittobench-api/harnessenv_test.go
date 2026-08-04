package main

import (
	"strings"
	"testing"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
)

func TestV8HarnessEnvironmentUsesPlatformAdapter(t *testing.T) {
	t.Setenv("HARNESS_EMBED_URL", "")
	env := harnessSandboxEnv(nil, protocol.BenchVersionV8, "session-route")
	if env["DITTOBENCH_PROVIDER"] != platformLockedProvider {
		t.Fatalf("provider = %q, want %q", env["DITTOBENCH_PROVIDER"], platformLockedProvider)
	}
	if env["DITTOBENCH_MODEL"] != llm.HarnessModelForVersion(protocol.BenchVersionV8) {
		t.Fatalf("wrong locked model: %q", env["DITTOBENCH_MODEL"])
	}
	if got := env["DITTOBENCH_INFERENCE_BASE_URL"]; got != "http://host.docker.internal:11436/v1/inference" {
		t.Fatalf("ticket gateway = %q", got)
	}
	if got := env["OLLAMA_BASE_URL"]; got != "http://host.docker.internal:11436" {
		t.Fatalf("embedding gateway = %q", got)
	}
	for _, retired := range []string{
		"CHUTES_API_KEY", "CHUTES_BASE_URL", "OPENAI_API_KEY", "OPENAI_BASE_URL",
		"OPENAI_API_BASE", "OPENROUTER_API_KEY", "OPENROUTER_BASE_URL",
	} {
		if _, ok := env[retired]; ok {
			t.Fatalf("retired compatibility selector %s is still injected", retired)
		}
	}
	for key, value := range env {
		if strings.Contains(value, "session-route") {
			t.Fatalf("opaque session id leaked through %s", key)
		}
	}
}

func TestV8HarnessEnvironmentCannotBeOverridden(t *testing.T) {
	hostile := map[string]string{
		"DITTOBENCH_PROVIDER":           "openrouter",
		"DITTOBENCH_MODEL":              "attacker/model",
		"DITTOBENCH_INFERENCE_BASE_URL": "http://attacker.example/v1",
		"OLLAMA_BASE_URL":               "http://attacker.example",
		"CHUTES_API_KEY":                "attacker",
		"CHUTES_BASE_URL":               "http://attacker.example/v1",
		"OPENAI_API_KEY":                "attacker",
		"OPENAI_BASE_URL":               "http://attacker.example/v1",
		"OPENROUTER_API_KEY":            "attacker",
		"DITTOBENCH_DB":                 "/app/read-only.db",
		"BENIGN":                        "ok",
	}
	env := harnessSandboxEnv(hostile, protocol.BenchVersionV8, "session-route")
	if env["DITTOBENCH_PROVIDER"] != platformLockedProvider {
		t.Fatalf("provider lock escaped: %q", env["DITTOBENCH_PROVIDER"])
	}
	if env["DITTOBENCH_MODEL"] != llm.HarnessModelForVersion(protocol.BenchVersionV8) {
		t.Fatalf("model lock escaped: %q", env["DITTOBENCH_MODEL"])
	}
	if env["DITTOBENCH_DB"] != "/tmp/dittobench.db" {
		t.Fatalf("database path escaped: %q", env["DITTOBENCH_DB"])
	}
	if env["BENIGN"] != "ok" {
		t.Fatal("non-locked environment was not preserved")
	}
	for key, value := range env {
		if strings.Contains(value, "attacker") && key != "BENIGN" {
			t.Fatalf("hostile value survived in %s: %q", key, value)
		}
	}
}

func TestSandboxRuntimeEnvOnlyLocksWritableDatabase(t *testing.T) {
	env := sandboxRuntimeEnv(map[string]string{
		"DITTOBENCH_DB":      "/app/read-only.db",
		"OPENROUTER_API_KEY": "practice-key",
	})
	if env["DITTOBENCH_DB"] != "/tmp/dittobench.db" {
		t.Fatalf("sandbox DB path = %q", env["DITTOBENCH_DB"])
	}
	if env["OPENROUTER_API_KEY"] != "practice-key" {
		t.Fatal("practice provider environment was unexpectedly changed")
	}
}
