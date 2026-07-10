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
// Env:
//   - RELAY_UPSTREAM_URL  upstream chat-completions URL
//     (default https://llm.chutes.ai/v1/chat/completions)
//   - RELAY_API_KEY       upstream bearer key (required)
//   - RELAY_MODEL         the model id to force (default: the upstream form of
//     the locked model must be set explicitly, since gateway names differ
//     from canonical ids; required)
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
)

const defaultUpstream = "https://llm.chutes.ai/v1/chat/completions"

// maxBody bounds a relayed request body; chat requests are prompts, not blobs.
const maxBody = 4 << 20

type relay struct {
	upstream string
	apiKey   string
	model    string
	client   *http.Client
}

func main() {
	r := &relay{
		upstream: envOr("RELAY_UPSTREAM_URL", defaultUpstream),
		apiKey:   strings.TrimSpace(os.Getenv("RELAY_API_KEY")),
		model:    strings.TrimSpace(os.Getenv("RELAY_MODEL")),
		client:   &http.Client{Timeout: 300 * time.Second},
	}
	if r.apiKey == "" || r.model == "" {
		log.Fatal("RELAY_API_KEY and RELAY_MODEL are required")
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
	// model and a non-streaming request (one JSON body back, nothing to parse
	// incrementally).
	body["model"] = r.model
	body["stream"] = false
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
