// Command refharness is a DETERMINISTIC reference harness for DittoBench.
//
// It implements the miner-harness wire contract (GET /health, POST /seed,
// POST /run) with a fixed, no-LLM, no-randomness policy: for each /run it routes
// to the catalog tool whose name+description keywords best overlap the prompt,
// emitting one call (or none, for prompts that match nothing). Because the
// policy is a pure function of the prompt, the ONLY thing that varies run to run
// is the dataset — which is exactly what the calibration harness needs to
// measure dataset *difficulty* without model or judge noise. It also serves as a
// zero-dependency harness for a single end-to-end smoke run (no OpenRouter key).
//
// Routing traps (categories whose surface wording points at the wrong tool) are
// missed by design, so the score reflects genuine per-dataset difficulty.
//
// Usage: refharness [-port 9000]
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// minOverlap is the keyword-overlap floor to call a tool at all; below it the
// harness calls nothing (so no-tool / abstention prompts can score correctly).
const minOverlap = 1

// fixedLatencyMs is reported for every case so median latency is deterministic.
const fixedLatencyMs = 1

// stopwords are dropped from both prompts and tool signals so routing keys on
// content words, not filler.
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "to": true, "of": true, "for": true,
	"and": true, "or": true, "in": true, "on": true, "at": true, "is": true,
	"it": true, "with": true, "your": true, "you": true, "me": true, "my": true,
	"can": true, "please": true, "i": true, "that": true, "this": true,
	"what": true, "whats": true, "how": true, "do": true, "does": true,
}

func main() {
	port := flag.Int("port", 9000, "HTTP listen port")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /seed", handleSeed)
	mux.HandleFunc("POST /run", handleRun)

	addr := ":" + strconv.Itoa(*port)
	log.Printf("refharness (deterministic reference) listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// handleSeed is a no-op store: it accepts the haystack and echoes the counts.
// The reference policy holds no memory, so memory cases score by the judge as
// not-recalled — fine for the tool-only direct path and a stable floor for full.
func handleSeed(w http.ResponseWriter, r *http.Request) {
	var req protocol.SeedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad seed body"})
		return
	}
	writeJSON(w, http.StatusOK, protocol.SeedResponse{
		Pairs:    len(req.Pairs),
		Subjects: len(req.Subjects),
		Links:    len(req.Links),
	})
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	var req protocol.RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad run body"})
		return
	}
	resp := protocol.RunResponse{
		FinalText: "ok",
		ToolCalls: route(req.UserInput, req.Tools),
		LatencyMs: fixedLatencyMs,
	}
	writeJSON(w, http.StatusOK, resp)
}

// route picks the single best-overlapping tool for the prompt, or nothing. It is
// a pure function of (prompt, tools): deterministic and order-stable.
func route(prompt string, tools []protocol.ToolDefinition) []protocol.ObservedToolCall {
	promptTokens := tokenSet(prompt)

	best := -1
	bestScore := 0
	for i, t := range tools {
		score := overlap(promptTokens, signal(t))
		if score > bestScore {
			bestScore = score
			best = i
		}
	}

	if best < 0 || bestScore < minOverlap {
		return []protocol.ObservedToolCall{} // call nothing
	}
	return []protocol.ObservedToolCall{{Name: tools[best].Name, Args: json.RawMessage(`{}`)}}
}

// signal is the keyword set a tool routes on: its name (split on '_') plus the
// content words of its description.
func signal(t protocol.ToolDefinition) map[string]bool {
	s := tokenSet(strings.ReplaceAll(t.Name, "_", " "))
	for tok := range tokenSet(t.Description) {
		s[tok] = true
	}
	return s
}

func overlap(a, b map[string]bool) int {
	// Iterate the smaller set for stability; count is order-independent.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	n := 0
	for tok := range small {
		if large[tok] {
			n++
		}
	}
	return n
}

// tokenSet lowercases, splits on non-alphanumerics, and drops stopwords + tokens
// shorter than 3 chars.
func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(f) < 3 || stopwords[f] {
			continue
		}
		out[f] = true
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
