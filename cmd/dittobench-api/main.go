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
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ditto-assistant/dittobench-api/internal/catalog"
	"github.com/ditto-assistant/dittobench-api/internal/datagen"
	"github.com/ditto-assistant/dittobench-api/internal/gen"
	"github.com/ditto-assistant/dittobench-api/internal/llm"
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

type server struct {
	store   *store.Store
	sandbox sandbox.Sandbox
}

func main() {
	port := flag.Int("port", 8000, "HTTP listen port (ditto-subnet API convention)")
	flag.Parse()

	s := &server{
		store:   store.New(),
		sandbox: sandbox.NewLocalDocker(),
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
	HarnessURL string            `json:"harness_url,omitempty"`
	GitURL     string            `json:"git_url,omitempty"`
	GitRef     string            `json:"git_ref,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	N          int               `json:"n"`
	// RunSize selects the full SN118 pipeline (build → generate fresh anti-cheat
	// dataset → seed haystack → run tool+memory cases → judge → score). One of
	// "small" | "medium" | "full". When set, this path takes precedence.
	RunSize string `json:"run_size,omitempty"`
	// Seed pins the dataset seed (0 = fresh crypto-random per submission).
	Seed int64 `json:"seed,omitempty"`
}

// submitResponse is returned by the direct (synchronous) path.
type submitResponse struct {
	RunID     string  `json:"run_id"`
	Status    string  `json:"status"`
	Composite float64 `json:"composite"`
	ToolMean  float64 `json:"tool_mean"`
	MedianMs  int64   `json:"median_ms"`
	N         int     `json:"n"`
	Seed      int64   `json:"seed"`
}

// acceptedResponse is returned by the sandbox (asynchronous) path; poll
// GET /v1/runs/{id} for status + report.
type acceptedResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Poll   string `json:"poll"`
}

func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.HarnessURL == "" && req.GitURL == "" {
		writeError(w, http.StatusBadRequest, "either harness_url (direct) or git_url (sandbox/run_size) is required")
		return
	}
	if req.HarnessURL != "" && req.GitURL != "" {
		writeError(w, http.StatusBadRequest, "provide only one of harness_url or git_url")
		return
	}

	// run_size selects the full SN118 pipeline (git + fresh anti-cheat dataset +
	// memory seeding + LLM judge). Requires a git submission and an OpenRouter key.
	if req.RunSize != "" {
		s.submitRunSize(w, r, req)
		return
	}

	n := req.N
	if n <= 0 {
		n = defaultN
	}
	if req.GitURL != "" {
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

	// 2. Fresh random dataset — rotating seed prevents overfitting.
	seed := freshSeed()
	runID := uuid.NewString()
	s.store.Create(runID, "direct", store.StatusRunning, seed, n)

	report, err := s.evaluate(ctx, runID, req.HarnessURL, seed, n)
	if err != nil {
		s.store.Fail(runID, err.Error())
		writeError(w, http.StatusBadGateway, "harness run failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, submitResponse{
		RunID:     report.RunID,
		Status:    string(store.StatusDone),
		Composite: report.Composite,
		ToolMean:  report.ToolMean,
		MedianMs:  report.MedianMs,
		N:         report.N,
		Seed:      seed,
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

	seed := freshSeed()
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
	image, buildLog, err := s.sandbox.Build(ctx, sandbox.Source{GitURL: req.GitURL, GitRef: req.GitRef})
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
	if _, err := s.evaluate(ctx, runID, handle.BaseURL, seed, n); err != nil {
		s.store.Fail(runID, "evaluation failed: "+err.Error())
		return
	}
}

// submitRunSize validates a run_size submission, requires an OpenRouter key,
// and kicks off the full SN118 pipeline asynchronously (returns 202 + run_id).
func (s *server) submitRunSize(w http.ResponseWriter, r *http.Request, req submitRequest) {
	if req.GitURL == "" && req.HarnessURL == "" {
		writeError(w, http.StatusBadRequest, "run_size requires git_url (build the crate in Docker) or harness_url (point at an already-running harness, for local dev)")
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
	if req.GitURL != "" {
		if s.sandbox == nil {
			writeError(w, http.StatusNotImplemented, "sandbox mode not enabled on this server")
			return
		}
		if err := s.sandbox.Available(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "sandbox backend unavailable: "+err.Error())
			return
		}
	}
	// An OpenRouter key is REQUIRED for run_size: the validator uses it for the
	// generator (paraphrase) + judge, and forwards it to the crate's agent.
	llmClient, err := llm.New()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	seed := req.Seed
	if seed == 0 {
		seed = gen.FreshSeed()
	}
	runID := uuid.NewString()
	s.store.Create(runID, "run_size", store.StatusQueued, seed, prof.Tools+prof.Mem)
	s.store.SetRunSize(runID, req.RunSize)
	log.Printf("run %s: run_size=%s seed=%d tools=%d mem=%d distractors=%d paraphrase=%.2f",
		runID, req.RunSize, seed, prof.Tools, prof.Mem, prof.Distractors, prof.ParaphraseFrac)

	go s.runSizeJob(context.Background(), runID, req, prof, seed, llmClient)

	writeJSON(w, http.StatusAccepted, acceptedResponse{
		RunID:  runID,
		Status: string(store.StatusQueued),
		Poll:   "/v1/runs/" + runID,
	})
}

// runSizeJob is the full SN118 pipeline: building → generating → seeding →
// running (per-case judge, appending partials) → scoring → done.
func (s *server) runSizeJob(ctx context.Context, runID string, req submitRequest, prof gen.Profile, seed int64, llmClient *llm.Client) {
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
	if req.GitURL != "" {
		s.store.SetStage(runID, store.StatusBuilding, 0, total)
		img, buildLog, err := s.sandbox.Build(ctx, sandbox.Source{GitURL: req.GitURL, GitRef: req.GitRef})
		if err != nil {
			s.store.Fail(runID, "build failed: "+err.Error())
			log.Printf("run %s build failed: %v\n%s", runID, err, buildLog)
			return
		}
		image = img
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
			"OPENROUTER_API_KEY":  os.Getenv(llm.EnvAPIKey),
			"DITTOBENCH_PROVIDER": "openrouter",
			"OLLAMA_BASE_URL":     "http://host.docker.internal:11434",
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
	s.store.Finish(runID, report)
	log.Printf("run %s done: composite=%.3f tool_mean=%.3f memory_mean=%.3f", runID, report.Composite, report.ToolMean, report.MemoryMean)
}

// evaluate generates the dataset, runs the harness over it, scores it, and
// stores the finished report. Shared by both submit modes.
func (s *server) evaluate(ctx context.Context, runID, harnessURL string, seed int64, n int) (protocol.ScoreReport, error) {
	ds := datagen.Generate(seed, n)
	tools := catalog.Catalog()

	resps, err := runner.RunHarness(ctx, harnessURL, ds, tools)
	if err != nil {
		return protocol.ScoreReport{}, err
	}

	s.store.SetStatus(runID, store.StatusScoring)
	report := scorer.Score(runID, ds.ToolCases, resps)
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
