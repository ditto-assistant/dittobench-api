// model-relay is the model-pinning gateway for validators that back the model
// lock with a hosted provider (Chutes) instead of local GPUs. It terminates
// the sandbox's OpenAI-compatible chat requests locally, FORCES the model
// field to the locked id, injects the upstream API key, and forwards to the
// upstream. The sandbox never holds the key and cannot choose the model, so
// the lock's semantics are identical to a local Ollama/vLLM gateway.
//
// The egress side stays fail-closed: the sandbox reaches only this relay
// (host.docker.internal), and the relay is the only process that reaches the
// upstream. A CONNECT-only egress proxy cannot pin the model field (it cannot
// see request bodies); this relay exists because it can.
//
// The forced model and thinking mode are frozen, not env vars: both are
// consensus-critical (a hybrid-reasoning model must run one mode fleet-wide or
// the k=3 validators' scores of a submission are not comparable), so every
// validator's relay pins the same model (llm.LockedUpstreamModel) and the same
// thinking mode (lockedThinking, off, which keeps replies inside per-case
// budgets and cuts variance). Bump either in code (a network-wide change), then
// redeploy.
//
// The relay pins the model against a COMPROMISED SANDBOX, not against the
// validator running it: the sandbox reaches only this relay and cannot choose
// the model, provider, upstream, or see the key. A validator operator who
// points RELAY_UPSTREAM_URL at an arbitrary server DOES change which model
// answers (the forced `model` field is just a string an alternate server can
// ignore), so the model lock is honor-system against the operator, backstopped
// by k=3 quorum consensus: a single validator scoring on a different model is
// outvoted by the median. Every validator must therefore run a certified
// profile with its default upstream; overriding RELAY_UPSTREAM_URL is a
// consensus-affecting deviation, not a neutral deployment knob.
//
// Env:
//   - RELAY_PROVIDER      which CODE-FROZEN profile serves the locked model:
//     "chutes" (default) or "openrouter". The env selects among profiles;
//     everything inside a profile (default upstream, exact model id,
//     serving-provider routing, thinking lock) is a code constant.
//   - RELAY_UPSTREAM_URL  upstream chat-completions URL override for the
//     selected profile. Intended only for a transparent same-model mirror
//     (proxy in front of the SAME certified upstream). Pointing it elsewhere
//     un-pins the scored model — see the consensus note above.
//   - RELAY_API_KEY       upstream bearer key for the selected provider
//     (required)
//   - PORT                listen port (default 11434, the gateway port the
//     sandbox already expects)
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
)

const defaultUpstream = "https://llm.chutes.ai/v1/chat/completions"

// providerProfile is one code-frozen upstream the relay may pin to. Every
// field is consensus-critical and therefore a constant: RELAY_PROVIDER only
// chooses WHICH profile runs, never what a profile pins.
type providerProfile struct {
	upstream string
	model    string
	// pinBody applies provider-specific consensus pins beyond the model id.
	pinBody func(body map[string]any)
}

// providers are the certified upstream profiles for the locked Qwen3-32B.
// Chutes serves the hardware-attested TEE deployment; OpenRouter serves the
// same weights with routing locked to the certified Nebius deployment (the
// throughput/availability evidence behind the certification measured that
// exact provider, and free routing would un-pin the scored backend).
var providers = map[string]providerProfile{
	"chutes": {
		upstream: defaultUpstream,
		model:    llm.LockedUpstreamModel,
	},
	"openrouter": {
		upstream: "https://openrouter.ai/api/v1/chat/completions",
		model:    llm.LockedHarnessModel,
		pinBody: func(body map[string]any) {
			body["provider"] = map[string]any{
				"only":            []string{"nebius"},
				"allow_fallbacks": false,
			}
			appendNoThink(body)
		},
	},
}

// appendNoThink applies Qwen3's soft thinking switch on the openrouter
// profile. Nebius via OpenRouter ignores chat_template_kwargs.enable_thinking
// and OpenRouter's reasoning parameter (verified live 2026-07-17: reasoning
// tokens still arrive with both set), so the hard switch the Chutes profile
// relies on does not exist here; the documented Qwen3 soft switch does. The
// LATEST /think|/no_think directive in the conversation wins, so the relay
// appends /no_think after everything the sandbox wrote — a sandbox-supplied
// "/think" can never come later. On Chutes the hard template switch makes
// soft directives inert, so this pin is openrouter-only.
func appendNoThink(body map[string]any) {
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		body["messages"] = []any{map[string]any{"role": "system", "content": "/no_think"}}
		return
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last == nil {
		return
	}
	switch c := last["content"].(type) {
	case string:
		last["content"] = c + "\n/no_think"
	case []any:
		last["content"] = append(c, map[string]any{"type": "text", "text": "/no_think"})
	default:
		body["messages"] = append(msgs, map[string]any{"role": "user", "content": "/no_think"})
	}
}

// lockedThinking is the frozen fleet-wide thinking mode. Off: a hybrid-reasoning
// model (Qwen3) must not pick per request, and off keeps replies inside per-case
// budgets. Consensus-critical, so it is a code constant, not env-tunable.
const lockedThinking = false

// maxBody bounds a relayed request body; chat requests are prompts, not blobs.
const maxBody = 4 << 20

type relay struct {
	upstream string
	apiKey   string
	model    string
	thinking bool
	pinBody  func(body map[string]any)
	client   *http.Client
}

func main() {
	providerName := envOr("RELAY_PROVIDER", "chutes")
	profile, ok := providers[providerName]
	if !ok {
		log.Fatalf("RELAY_PROVIDER %q is not a certified profile (chutes|openrouter)", providerName)
	}
	r := &relay{
		upstream: envOr("RELAY_UPSTREAM_URL", profile.upstream),
		apiKey:   strings.TrimSpace(os.Getenv("RELAY_API_KEY")),
		model:    profile.model,
		thinking: lockedThinking,
		pinBody:  profile.pinBody,
		client:   &http.Client{Timeout: 300 * time.Second},
	}
	if r.apiKey == "" {
		log.Fatal("RELAY_API_KEY is required")
	}
	mux := http.NewServeMux()
	// Both the bare and /v1 chat-completions paths, so OLLAMA_BASE_URL-style
	// and OPENAI_BASE_URL-style clients work unchanged.
	mux.HandleFunc("POST /v1/chat/completions", r.handle)
	mux.HandleFunc("POST /chat/completions", r.handle)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	addr := ":" + envOr("PORT", "11434")
	log.Printf("model-relay on %s -> %s (model pinned to %s)", addr, r.upstream, r.model)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (r *relay) handle(w http.ResponseWriter, req *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "body must be a JSON object", http.StatusBadRequest)
		return
	}
	// The pin: whatever the sandbox asked for, the upstream sees the locked
	// model, a non-streaming request (one JSON body back), and one locked
	// thinking mode (a hybrid-reasoning model must not pick per request).
	body["model"] = r.model
	body["stream"] = false
	ctk, _ := body["chat_template_kwargs"].(map[string]any)
	if ctk == nil {
		ctk = map[string]any{}
	}
	ctk["enable_thinking"] = r.thinking
	body["chat_template_kwargs"] = ctk
	if r.pinBody != nil {
		r.pinBody(body)
	}
	out, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "marshal body", http.StatusInternalServerError)
		return
	}

	up, err := http.NewRequestWithContext(req.Context(), http.MethodPost, r.upstream, bytes.NewReader(out))
	if err != nil {
		http.Error(w, "build upstream request", http.StatusInternalServerError)
		return
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Authorization", "Bearer "+r.apiKey) // never the sandbox's header

	resp, err := r.client.Do(up)
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}
