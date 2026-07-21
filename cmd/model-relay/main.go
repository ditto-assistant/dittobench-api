// model-relay is the model-pinning gateway for validators that back the model
// lock with a hosted provider instead of local GPUs. It terminates the
// sandbox's OpenAI-compatible chat requests locally, FORCES the model field to
// the locked id, injects the upstream API key, and forwards to the upstream.
// The sandbox never holds the key and cannot choose the model, so the lock's
// semantics are identical to a local Ollama/vLLM gateway. The egress side stays
// fail-closed: the sandbox reaches only this relay (host.docker.internal), and
// the relay is the only process that reaches the upstream.
//
// The same locked model (Qwen3-32B) is served by two interchangeable certified
// providers, and each validator picks one with RELAY_PROVIDER. This spreads
// scoring load: pinning the whole fleet to Chutes saturated that endpoint and
// drove up latency, so validators now split across Chutes (the hardware-
// attested TEE deployment, "chutes") and OpenRouter (the same weights via the
// certified Nebius deployment, "openrouter"). The two are certified as
// comparable, so a submission's k=3 quorum may mix providers by design; every
// score reports the same harness model id (llm.LockedHarnessModel), so the
// median is taken over comparable numbers.
//
// Each profile is CODE-FROZEN: RELAY_PROVIDER only chooses which certified
// profile runs, never what it pins (upstream, exact model id, serving-provider
// routing, thinking mode). All of those are consensus-critical constants a
// hybrid-reasoning model must share fleet-wide; change them in code (a
// network-wide change), then redeploy. There is deliberately no upstream-URL
// override: the pin is enforced in code, not left to a validator's env.
//
// Env (deployment only):
//   - RELAY_PROVIDER  "chutes" (default) or "openrouter"
//   - RELAY_API_KEY   upstream bearer key for the selected provider (required)
//   - PORT            listen port (default 11434, the gateway port the sandbox
//     already expects)
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
)

// providerProfile is one code-frozen upstream the relay may pin to. Every
// field is consensus-critical and therefore a constant: RELAY_PROVIDER only
// chooses WHICH profile runs, never what a profile pins.
type providerProfile struct {
	upstream string
	model    string
	// pinBody applies provider-specific consensus pins beyond the model id.
	pinBody func(body map[string]any)
}

// providers are the certified profiles for the locked Qwen3-32B. Chutes serves
// the hardware-attested TEE deployment; OpenRouter serves the same weights with
// routing locked to the certified Nebius deployment (the throughput evidence
// behind the certification measured that exact provider, and free routing would
// un-pin the scored backend).
var providers = map[string]providerProfile{
	"chutes": {
		upstream: "https://llm.chutes.ai/v1/chat/completions",
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

// maxResponseBody bounds the upstream completion before it is inspected and
// forwarded. The locked model is non-streaming and benchmark replies are small;
// a response above this limit is an upstream protocol failure, not a usable
// completion.
const maxResponseBody = 16 << 20

type relay struct {
	upstream               string
	apiKey                 string
	model                  string
	thinking               bool
	pinBody                func(body map[string]any)
	client                 *http.Client
	requests               atomic.Uint64
	successes              atomic.Uint64
	infrastructureFailures atomic.Uint64
}

type relayHealth struct {
	AccountingVersion      int    `json:"accounting_version"`
	Status                 string `json:"status"`
	Requests               uint64 `json:"requests"`
	Successes              uint64 `json:"successes"`
	InfrastructureFailures uint64 `json:"infrastructure_failures"`
}

func main() {
	providerName := envOr("RELAY_PROVIDER", "chutes")
	profile, ok := providers[providerName]
	if !ok {
		log.Fatalf("RELAY_PROVIDER %q is not a certified profile (chutes|openrouter)", providerName)
	}
	r := &relay{
		upstream: profile.upstream,
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
	mux.HandleFunc("GET /health", r.health)
	addr := ":" + envOr("PORT", "11434")
	log.Printf("model-relay on %s -> %s (model pinned to %s)", addr, r.upstream, r.model)
	// Bind IPv4 explicitly. The relay is the gateway a sandboxed harness reaches via
	// host.docker.internal (Docker Desktop's IPv4 host-gateway); a Go dual-stack
	// "[::]" listener is not reachable that way on Docker Desktop/WSL2, so the
	// harness's chat calls fail before reaching the relay. Docker networking is
	// IPv4, so tcp4 loses nothing.
	ln, err := net.Listen("tcp4", "0.0.0.0"+addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	log.Fatal(http.Serve(ln, mux))
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
	r.requests.Add(1)

	up, err := http.NewRequestWithContext(req.Context(), http.MethodPost, r.upstream, bytes.NewReader(out))
	if err != nil {
		http.Error(w, "build upstream request", http.StatusInternalServerError)
		return
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Authorization", "Bearer "+r.apiKey) // never the sandbox's header

	resp, err := r.client.Do(up)
	if err != nil {
		r.infrastructureFailures.Add(1)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		r.infrastructureFailures.Add(1)
		http.Error(w, "read upstream response", http.StatusBadGateway)
		return
	}
	if len(responseBody) > maxResponseBody {
		r.infrastructureFailures.Add(1)
		http.Error(w, "upstream response too large", http.StatusBadGateway)
		return
	}
	if isInfrastructureStatus(resp.StatusCode) {
		r.infrastructureFailures.Add(1)
	} else if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		var completion struct {
			Choices []json.RawMessage `json:"choices"`
		}
		if err := json.Unmarshal(responseBody, &completion); err != nil || len(completion.Choices) == 0 {
			r.infrastructureFailures.Add(1)
		} else {
			r.successes.Add(1)
		}
	}
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}

func isInfrastructureStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	default:
		return status >= http.StatusInternalServerError
	}
}

func (r *relay) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(relayHealth{
		AccountingVersion:      1,
		Status:                 "ok",
		Requests:               r.requests.Load(),
		Successes:              r.successes.Load(),
		InfrastructureFailures: r.infrastructureFailures.Load(),
	})
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}
