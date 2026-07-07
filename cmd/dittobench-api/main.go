// Command dittobench-api is the off-chain DittoBench *practice* validator for
// Bittensor SN118. It mirrors the on-chain run+score loop minus TAO/chain:
// miners pull a fresh, randomized small dataset, run their harness against it,
// and get a DittoBench score — without overfitting risk (the seed rotates on
// every request).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ditto-assistant/dittobench-api/internal/catalog"
	"github.com/ditto-assistant/dittobench-api/internal/datagen"
	"github.com/ditto-assistant/dittobench-api/internal/gen"
	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-api/internal/netguard"
	"github.com/ditto-assistant/dittobench-api/internal/ratelimit"
	"github.com/ditto-assistant/dittobench-api/internal/runner"
	"github.com/ditto-assistant/dittobench-api/internal/sandbox"
	"github.com/ditto-assistant/dittobench-api/internal/scorer"
	"github.com/ditto-assistant/dittobench-api/internal/store"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

const defaultN = 30

// sandboxHealthTimeout bounds how long we wait for a freshly built container to
// answer /health before giving up on the submission.
const sandboxHealthTimeout = 90 * time.Second

// Abuse guards for the public submit endpoint.
const (
	maxSubmitBody     = 64 << 10        // request body cap (submissions are tiny)
	submitsPerWindow  = 15              // per-IP submissions...
	submitWindow      = 5 * time.Minute // ...per this window
	maxConcurrentRuns = 8               // global cap on in-flight run_size jobs
)

type server struct {
	store   *store.Store
	sandbox sandbox.Sandbox
	// allowPrivate relaxes the SSRF guard for local dev + the Docker sandbox
	// (loopback containers). False in production (hosted Cloud Run).
	allowPrivate bool
	limiter      *ratelimit.Limiter
	runSlots     chan struct{} // bounds concurrent run_size jobs
}

func main() {
	port := flag.Int("port", 8000, "HTTP listen port (ditto-subnet API convention)")
	flag.Parse()

	// Cloud Run (and most PaaS) inject the listen port via $PORT; honor it over
	// the flag default so the same binary runs locally and hosted.
	if p := os.Getenv("PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			*port = v
		}
	}

	// SSRF guard is ON by default; opt out for local dev / sandbox containers.
	allowPrivate := envBool("DITTOBENCH_ALLOW_PRIVATE_HARNESS")
	runner.Configure(allowPrivate)
	if allowPrivate {
		log.Printf("WARNING: DITTOBENCH_ALLOW_PRIVATE_HARNESS set — SSRF guard relaxed (local/dev only)")
	}

	s := &server{
		store:        store.New(),
		sandbox:      sandbox.NewLocalDocker(),
		allowPrivate: allowPrivate,
		limiter:      ratelimit.New(submitsPerWindow, submitWindow),
		runSlots:     make(chan struct{}, maxConcurrentRuns),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/dataset", s.handleDataset)
	mux.HandleFunc("GET /v1/catalog", s.handleCatalog)
	mux.HandleFunc("POST /v1/submit", s.handleSubmit)
	mux.HandleFunc("GET /v1/runs/{id}", s.handleGetRun)

	addr := ":" + strconv.Itoa(*port)
	log.Printf("dittobench-api (off-chain practice validator) listening on %s", addr)
	if err := http.ListenAndServe(addr, logging(mux)); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// ---- middleware & helpers ----

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Millisecond))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---- handlers ----

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, catalog.Catalog())
}

// handleDataset returns a fresh dataset for practice. Seed is random unless
// pinned via ?seed=, n defaults to 30 (clamped in datagen).
func (s *server) handleDataset(w http.ResponseWriter, r *http.Request) {
	n := parseIntDefault(r.URL.Query().Get("n"), defaultN)
	seed := freshSeed()
	if sv := r.URL.Query().Get("seed"); sv != "" {
		if parsed, err := strconv.ParseInt(sv, 10, 64); err == nil {
			seed = parsed
		}
	}
	ds := datagen.Generate(seed, n)
	writeJSON(w, http.StatusOK, ds)
}

// submitRequest accepts two mutually-exclusive modes:
//
//   - Direct:  {"harness_url": "..."} — the miner runs their own harness; the
//     API just scores it. Synchronous (fast), returns 200 with the report.
//   - Sandbox: {"git_url": "...", "git_ref": "...", "env": {...}} — the API
//     builds the submission in Docker and runs it, mirroring the on-chain
//     validator. Asynchronous (build is slow); returns 202 + run_id to poll.
//
// env is forwarded to the sandbox container (e.g. OPENROUTER_API_KEY and any
// model override the miner's harness reads). It is ignored in direct mode.
type submitRequest struct {
	HarnessURL string `json:"harness_url,omitempty"`
	GitURL     string `json:"git_url,omitempty"`
	GitRef     string `json:"git_ref,omitempty"`
	// TarballURL is a presigned https URL of a gzipped tar of the harness. The
	// SN118 platform stores miner uploads as tarballs, so the validator hands us
	// the platform's short-lived download URL instead of a git repo. Mutually
	// exclusive with harness_url / git_url; built in the Docker sandbox like
	// git_url. (mode B — platform-tarball ingest.)
	TarballURL string `json:"tarball_url,omitempty"`
	// TarballSHA256 optionally pins the tarball's SHA-256 (hex). When present the
	// sandbox re-verifies the fetched bytes — the platform already checks it at
	// upload, so this is defense in depth against a swapped/corrupted blob.
	TarballSHA256 string            `json:"tarball_sha256,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	N             int               `json:"n"`
	// RunSize selects the full SN118 pipeline (build → generate fresh anti-cheat
	// dataset → seed haystack → run tool+memory cases → judge → score). One of
	// "small" | "medium" | "full". When set, this path takes precedence.
	RunSize string `json:"run_size,omitempty"`
	// Seed pins the dataset seed (0 = fresh crypto-random per submission).
	Seed int64 `json:"seed,omitempty"`
	// OpenRouterKey is the miner's BYOK OpenRouter key, used for the generator
	// (paraphrase) + judge (scoring). The hosted practice API requires it per
	// request (it stores no keys); locally it falls back to the server env.
	// May also be supplied via the Authorization: Bearer header.
	OpenRouterKey string `json:"openrouter_key,omitempty"`
}

// resolveOpenRouterKey returns the OpenRouter key for a submission, preferring
// (1) the request body, (2) an Authorization: Bearer header, (3) the server's
// OPENROUTER_API_KEY env (local/internal fallback). The key is never logged.
func resolveOpenRouterKey(r *http.Request, req submitRequest) string {
	if k := strings.TrimSpace(req.OpenRouterKey); k != "" {
		return k
	}
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return strings.TrimSpace(os.Getenv(llm.EnvAPIKey))
}

// sourceFromReq builds the sandbox Source for a build submission. git_url and
// tarball_url are mutually exclusive (enforced in handleSubmit); GitRef applies
// only to the git path.
func sourceFromReq(req submitRequest) sandbox.Source {
	return sandbox.Source{
		GitURL:        req.GitURL,
		GitRef:        req.GitRef,
		TarballURL:    req.TarballURL,
		TarballSHA256: req.TarballSHA256,
	}
}

// submitResponse is returned by the direct (synchronous) path.
type submitResponse struct {
	RunID       string  `json:"run_id"`
	Status      string  `json:"status"`
	Composite   float64 `json:"composite"`
	ToolMean    float64 `json:"tool_mean"`
	LatencyMean float64 `json:"latency_mean"`
	MedianMs    int64   `json:"median_ms"`
	N           int     `json:"n"`
	Seed        int64   `json:"seed"`
}

// acceptedResponse is returned by the sandbox (asynchronous) path; poll
// GET /v1/runs/{id} for status + report.
type acceptedResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Poll   string `json:"poll"`
}

func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	// Abuse guard: per-IP rate limit on the expensive submit endpoint. Skipped
	// in local/dev mode (DITTOBENCH_ALLOW_PRIVATE_HARNESS), where a calibration
	// loop legitimately submits in bulk from a single IP.
	if !s.allowPrivate && !s.limiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded; slow down and retry shortly")
		return
	}

	var req submitRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxSubmitBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid or oversized JSON body")
		return
	}
	// Exactly one submission source: a running harness (direct), a git repo, or
	// a presigned platform tarball (mode B). git_url + tarball_url both build in
	// the Docker sandbox.
	sources := 0
	for _, set := range []bool{req.HarnessURL != "", req.GitURL != "", req.TarballURL != ""} {
		if set {
			sources++
		}
	}
	if sources == 0 {
		writeError(w, http.StatusBadRequest, "one of harness_url (direct), git_url, or tarball_url (sandbox/run_size) is required")
		return
	}
	if sources > 1 {
		writeError(w, http.StatusBadRequest, "provide only one of harness_url, git_url, or tarball_url")
		return
	}

	// SSRF guard: a caller-supplied harness_url / tarball_url must be a public
	// http(s) endpoint (relaxed for local dev via DITTOBENCH_ALLOW_PRIVATE_HARNESS).
	if req.HarnessURL != "" {
		if err := netguard.ValidateURL(req.HarnessURL, s.allowPrivate); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.TarballURL != "" {
		if err := netguard.ValidateURL(req.TarballURL, s.allowPrivate); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// run_size selects the full SN118 pipeline (build + fresh anti-cheat dataset +
	// memory seeding + LLM judge). Requires a buildable/runnable source + key.
	if req.RunSize != "" {
		s.submitRunSize(w, r, req)
		return
	}

	n := req.N
	if n <= 0 {
		n = defaultN
	}
	if req.GitURL != "" || req.TarballURL != "" {
		s.submitSandbox(w, r, req, n)
		return
	}
	s.submitDirect(w, r, req, n)
}

// submitDirect scores a harness the miner is already running, synchronously.
func (s *server) submitDirect(w http.ResponseWriter, r *http.Request, req submitRequest, n int) {
	ctx := r.Context()

	// 1. Health-check the harness before spending an evaluation on it.
	if err := runner.Health(ctx, req.HarnessURL); err != nil {
		writeError(w, http.StatusBadGateway, "harness health check failed: "+err.Error())
		return
	}

	// 2. Fresh random dataset — rotating seed prevents overfitting (honor a
	//    pinned "seed" for reproducibility).
	seed := pinnedOrFreshSeed(req.Seed)
	runID := uuid.NewString()
	s.store.Create(runID, "direct", store.StatusRunning, seed, n)

	// Direct harness_url path: no crate is built here, so there is no source tree
	// to fingerprint (nil ⇒ the platform gate falls back to its lexical + size
	// signals).
	report, err := s.evaluate(ctx, runID, req.HarnessURL, seed, n, nil)
	if err != nil {
		s.store.Fail(runID, err.Error())
		writeError(w, http.StatusBadGateway, "harness run failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, submitResponse{
		RunID:       report.RunID,
		Status:      string(store.StatusDone),
		Composite:   report.Composite,
		ToolMean:    report.ToolMean,
		LatencyMean: report.LatencyMean,
		MedianMs:    report.MedianMs,
		N:           report.N,
		Seed:        seed,
	})
}

// submitSandbox accepts a git submission, returns 202, and builds+runs+scores
// it in the background (mirroring the SN118 upload→poll lifecycle).
func (s *server) submitSandbox(w http.ResponseWriter, r *http.Request, req submitRequest, n int) {
	if s.sandbox == nil {
		writeError(w, http.StatusNotImplemented, "sandbox mode not enabled on this server")
		return
	}
	if err := s.sandbox.Available(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "sandbox backend unavailable: "+err.Error())
		return
	}

	seed := pinnedOrFreshSeed(req.Seed)
	runID := uuid.NewString()
	s.store.Create(runID, "sandbox", store.StatusQueued, seed, n)

	// Detach from the request context so the long build survives the response.
	go s.runSandboxJob(context.Background(), runID, req, seed, n)

	writeJSON(w, http.StatusAccepted, acceptedResponse{
		RunID:  runID,
		Status: string(store.StatusQueued),
		Poll:   "/v1/runs/" + runID,
	})
}

// runSandboxJob builds the submission, runs it, evaluates it, and tears it down,
// updating the job status at each step.
func (s *server) runSandboxJob(ctx context.Context, runID string, req submitRequest, seed int64, n int) {
	defer func() {
		if rec := recover(); rec != nil {
			s.store.Fail(runID, "internal panic during sandbox job")
			log.Printf("sandbox job %s panicked: %v", runID, rec)
		}
	}()

	s.store.SetStatus(runID, store.StatusBuilding)
	image, buildLog, fingerprint, err := s.sandbox.Build(ctx, sourceFromReq(req))
	if err != nil {
		s.store.Fail(runID, "build failed: "+err.Error())
		log.Printf("sandbox job %s build failed: %v\n%s", runID, err, buildLog)
		return
	}

	handle, err := s.sandbox.Run(ctx, image, req.Env)
	if err != nil {
		s.store.Fail(runID, "container start failed: "+err.Error())
		return
	}
	defer s.sandbox.Stop(context.Background(), handle)

	if err := runner.WaitHealthy(ctx, handle.BaseURL, sandboxHealthTimeout); err != nil {
		s.store.Fail(runID, "harness never became healthy: "+err.Error())
		return
	}

	s.store.SetStatus(runID, store.StatusRunning)
	if _, err := s.evaluate(ctx, runID, handle.BaseURL, seed, n, fingerprint); err != nil {
		s.store.Fail(runID, "evaluation failed: "+err.Error())
		return
	}
}

// submitRunSize validates a run_size submission, requires an OpenRouter key,
// and kicks off the full SN118 pipeline asynchronously (returns 202 + run_id).
func (s *server) submitRunSize(w http.ResponseWriter, r *http.Request, req submitRequest) {
	if req.GitURL == "" && req.HarnessURL == "" && req.TarballURL == "" {
		writeError(w, http.StatusBadRequest, "run_size requires git_url / tarball_url (build in Docker) or harness_url (point at an already-running harness, for local dev)")
		return
	}
	prof, ok := gen.ProfileFor(req.RunSize)
	if !ok {
		writeError(w, http.StatusBadRequest, "run_size must be one of: small, medium, full")
		return
	}
	// Sandbox is only needed when we build the crate ourselves. The local
	// harness_url path runs the full generate→seed→run→score pipeline against an
	// already-running harness, so it does not require Docker.
	if req.GitURL != "" || req.TarballURL != "" {
		if s.sandbox == nil {
			writeError(w, http.StatusNotImplemented, "git_url (Docker build) is not available on this server; submit a reachable harness_url instead")
			return
		}
		if err := s.sandbox.Available(r.Context()); err != nil {
			// The hosted practice API has no Docker daemon — the on-chain
			// validator owns crate builds. Miners practice by exposing a
			// reachable harness and submitting harness_url.
			writeError(w, http.StatusServiceUnavailable, "git_url (Docker build) is unavailable here ("+err.Error()+"); submit a reachable harness_url to practice")
			return
		}
	}
	// An OpenRouter key is REQUIRED for run_size: the validator uses it for the
	// generator (paraphrase) + judge, and (Docker path) forwards it to the
	// crate's agent. BYOK — from the request body, Bearer header, or env.
	apiKey := resolveOpenRouterKey(r, req)
	if apiKey == "" {
		writeError(w, http.StatusBadRequest, "an OpenRouter key is required for run_size submissions (send \"openrouter_key\" in the body or an Authorization: Bearer header)")
		return
	}
	llmClient := llm.NewWithKey(apiKey)

	// Bound concurrent in-flight runs so a burst can't exhaust the instance.
	select {
	case s.runSlots <- struct{}{}:
	default:
		writeError(w, http.StatusTooManyRequests, "validator at capacity; retry shortly")
		return
	}

	seed := pinnedOrFreshSeed(req.Seed)
	runID := uuid.NewString()
	s.store.Create(runID, "run_size", store.StatusQueued, seed, prof.Tools+prof.Mem)
	s.store.SetRunSize(runID, req.RunSize)
	log.Printf("run %s: run_size=%s seed=%d tools=%d mem=%d distractors=%d paraphrase=%.2f",
		runID, req.RunSize, seed, prof.Tools, prof.Mem, prof.Distractors, prof.ParaphraseFrac)

	go s.runSizeJob(context.Background(), runID, req, prof, seed, llmClient, apiKey)

	writeJSON(w, http.StatusAccepted, acceptedResponse{
		RunID:  runID,
		Status: string(store.StatusQueued),
		Poll:   "/v1/runs/" + runID,
	})
}

// runSizeJob is the full SN118 pipeline: building → generating → seeding →
// running (per-case judge, appending partials) → scoring → done.
func (s *server) runSizeJob(ctx context.Context, runID string, req submitRequest, prof gen.Profile, seed int64, llmClient *llm.Client, apiKey string) {
	defer func() { <-s.runSlots }() // release the concurrency slot
	defer func() {
		if rec := recover(); rec != nil {
			s.store.Fail(runID, "internal panic during run_size job")
			log.Printf("run_size job %s panicked: %v", runID, rec)
		}
	}()

	total := prof.Tools + prof.Mem

	// 1. building — build the crate in the Docker sandbox. Skipped on the local
	//    harness_url path (the miner is already running their harness).
	var image string
	var structuralFP *protocol.CodeFingerprint
	if req.GitURL != "" || req.TarballURL != "" {
		s.store.SetStage(runID, store.StatusBuilding, 0, total)
		img, buildLog, fp, err := s.sandbox.Build(ctx, sourceFromReq(req))
		if err != nil {
			s.store.Fail(runID, "build failed: "+err.Error())
			log.Printf("run %s build failed: %v\n%s", runID, err, buildLog)
			return
		}
		image = img
		structuralFP = fp
	}

	// 2. generating — fresh, anti-cheat dataset (tools + memory haystack).
	s.store.SetStage(runID, store.StatusGenerating, 0, total)
	rng := gen.NewRNG(seed)
	genModel := llm.GeneratorModel()
	toolCases := gen.GenerateTools(ctx, rng, prof.Tools, prof.ParaphraseFrac, llmClient, genModel)
	seedReq, memCases, err := gen.GenerateMemory(ctx, rng, prof.Mem, prof.Distractors, prof.ParaphraseFrac, llmClient, genModel, gen.SeedDir(), gen.OraclePath())
	if err != nil {
		s.store.Fail(runID, "dataset generation failed: "+err.Error())
		return
	}
	log.Printf("run %s generated: %d tool cases, %d memory cases, %d haystack pairs (%d subjects)",
		runID, len(toolCases), len(memCases), len(seedReq.Pairs), len(seedReq.Subjects))

	// 3. start the container, forwarding the OpenRouter key + provider config so
	//    the crate's agent + embedder can run. On the local harness_url path we
	//    skip the container and target the miner's already-running harness.
	harnessURL := req.HarnessURL
	if image != "" {
		env := map[string]string{
			"OPENROUTER_API_KEY":  apiKey,
			"DITTOBENCH_PROVIDER": "openrouter",
			"OLLAMA_BASE_URL":     "http://host.docker.internal:11434",
		}
		// Operator escape-hatch: when DITTOBENCH_HARNESS_MODEL is set on the
		// server, force the harness's chat model. Off by default so a miner's own
		// model choice is respected; set it only when the validator's OpenRouter
		// key can't reach the harness's default provider (e.g. this key returns
		// 404 "no endpoints found" for anthropic/* — see openai/gpt-4o-mini).
		if m := strings.TrimSpace(os.Getenv("DITTOBENCH_HARNESS_MODEL")); m != "" {
			env["DITTOBENCH_MODEL"] = m
		}
		for k, v := range req.Env {
			env[k] = v
		}
		handle, err := s.sandbox.Run(ctx, image, env)
		if err != nil {
			s.store.Fail(runID, "container start failed: "+err.Error())
			return
		}
		defer s.sandbox.Stop(context.Background(), handle)
		harnessURL = handle.BaseURL
	}

	if err := runner.WaitHealthy(ctx, harnessURL, sandboxHealthTimeout); err != nil {
		s.store.Fail(runID, "harness never became healthy: "+err.Error())
		return
	}

	// 4. seeding — push the fresh haystack to the crate's /seed.
	s.store.SetStage(runID, store.StatusSeeding, 0, total)
	if len(seedReq.Pairs) > 0 {
		if _, err := runner.Seed(ctx, harnessURL, seedReq); err != nil {
			s.store.Fail(runID, "seeding haystack failed: "+err.Error())
			return
		}
	}

	// 5. running — execute + score each case, appending partials for the UI.
	s.store.SetStage(runID, store.StatusRunning, 0, total)
	tools := catalog.Catalog()
	scorerModel := llm.ScorerModel()
	perCase := make([]protocol.CaseScore, 0, total)

	for _, c := range toolCases {
		resp, runErr := runner.RunCase(ctx, harnessURL, c.ID, c.Prompt, tools)
		cs := scorer.ScoreToolCase(c, resp, runErr == nil)
		quality := scorer.JudgeToolQuality(ctx, llmClient, scorerModel, c.Prompt, cs.Called, c.ExpectedBehavior, resp.FinalText)
		cs = scorer.ComposeTool(cs, quality)
		perCase = append(perCase, cs)
		s.store.AppendPartial(runID, cs)
	}

	for _, mc := range memCases {
		resp, _ := runner.RunCase(ctx, harnessURL, mc.ID, mc.Question, tools)
		correct := scorer.JudgeMemory(ctx, llmClient, scorerModel, mc.Question, mc.ExpectedAnswer, resp.FinalText, mc.QuestionType)
		cs := scorer.ScoreMemoryCase(mc, resp, correct)
		perCase = append(perCase, cs)
		s.store.AppendPartial(runID, cs)
	}

	// 6. scoring — aggregate + finish.
	s.store.SetStage(runID, store.StatusScoring, len(perCase), total)
	report := scorer.Aggregate(runID, perCase)
	report.Seed = seed
	report.StructuralFingerprint = structuralFP
	s.store.Finish(runID, report)
	log.Printf("run %s done: composite=%.3f tool_mean=%.3f memory_mean=%.3f latency_mean=%.3f", runID, report.Composite, report.ToolMean, report.MemoryMean, report.LatencyMean)
}

// evaluate generates the dataset, runs the harness over it, scores it, and
// stores the finished report. Shared by both submit modes. “fingerprint“ is the
// crate's structural sketch (nil on the local harness_url path); it is attached to
// the report as advisory anti-copy metadata and never affects the score.
func (s *server) evaluate(ctx context.Context, runID, harnessURL string, seed int64, n int, fingerprint *protocol.CodeFingerprint) (protocol.ScoreReport, error) {
	ds := datagen.Generate(seed, n)
	tools := catalog.Catalog()

	resps, err := runner.RunHarness(ctx, harnessURL, ds, tools)
	if err != nil {
		return protocol.ScoreReport{}, err
	}

	s.store.SetStatus(runID, store.StatusScoring)
	report := scorer.Score(runID, ds.ToolCases, resps)
	report.Seed = seed
	report.StructuralFingerprint = fingerprint
	s.store.Finish(runID, report)
	return report, nil
}

func (s *server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// ---- small utils ----

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}

// freshSeed returns a non-deterministic seed for a new evaluation. Uses both
// wall-clock and a random component so concurrent requests don't collide.
func freshSeed() int64 {
	return time.Now().UnixNano() ^ int64(rand.Uint64())
}

// pinnedOrFreshSeed returns the request's pinned seed when non-zero (so a run is
// reproducible via {"seed":N}), else a fresh non-negative crypto-random seed.
// Shared by the direct, sandbox, and run_size submit paths so seed semantics are
// identical across them.
func pinnedOrFreshSeed(pinned int64) int64 {
	if pinned != 0 {
		return pinned
	}
	return gen.FreshSeed()
}

// envBool reports whether an env var is set to a truthy value.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// clientIP returns the caller's IP for rate-limiting, honoring the first hop of
// X-Forwarded-For (set by Cloud Run / proxies) and falling back to RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
