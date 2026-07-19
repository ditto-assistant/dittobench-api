// Command dittobench-api is the off-chain DittoBench *practice* validator for
// Bittensor SN118. It mirrors the on-chain run+score loop minus TAO/chain:
// miners pull a fresh, randomized small dataset, run their harness against it,
// and get a DittoBench score — without overfitting risk (the seed rotates on
// every request).
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-api/internal/netguard"
	"github.com/ditto-assistant/dittobench-api/internal/ratelimit"
	"github.com/ditto-assistant/dittobench-api/internal/runner"
	"github.com/ditto-assistant/dittobench-api/internal/sandbox"
	"github.com/ditto-assistant/dittobench-api/internal/scorer"
	"github.com/ditto-assistant/dittobench-api/internal/store"
	"github.com/ditto-assistant/dittobench-datagen/catalog"
	"github.com/ditto-assistant/dittobench-datagen/datagen"
	"github.com/ditto-assistant/dittobench-datagen/gen"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
	"github.com/ditto-assistant/dittobench-datagen/toolexec"
)

const defaultN = 30

// sandboxHealthTimeout bounds how long we wait for a freshly built container to
// answer /health before giving up on the submission.
const sandboxHealthTimeout = 3 * time.Minute

// Abuse guards for the public submit endpoint.
const (
	maxSubmitBody    = 64 << 10        // request body cap (submissions are tiny)
	submitsPerWindow = 15              // per-IP submissions...
	submitWindow     = 5 * time.Minute // ...per this window
)

// maxConcurrentRuns is the global cap on in-flight run_size jobs. caseConcurrency
// is how many cases within one run execute in parallel against the harness. The
// two multiply into the peak load on the locked-model provider (roughly
// maxConcurrentRuns * caseConcurrency concurrent model round-trips), so size them
// together to the provider's rate limit. caseConcurrency=1 reproduces the
// original strictly-sequential per-case execution. Both are env-overridable so a
// rescore wave can be tuned to the available provider headroom without a rebuild.
var (
	// Defaults to 1: one full miner sandbox has a 3 GiB cgroup cap and 512 MiB
	// writable tmpfs within it, and the validator host also runs Ollama, the
	// relay, Docker, Pylon, and the worker, so concurrent in-process full runs
	// overcommit the documented 16 GiB host (#31). Raise it only on a host with
	// headroom to spare.
	maxConcurrentRuns = envIntDefault("DITTOBENCH_MAX_CONCURRENT_RUNS", 1)
	caseConcurrency   = envIntDefault("DITTOBENCH_CASE_CONCURRENCY", 4)
)

// envIntDefault reads a positive int from key, returning def when unset or invalid.
func envIntDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// runBounded runs fn for each index in [0,n) with at most `concurrency` calls in
// flight, and returns once all have finished. fn must be safe for concurrent use;
// callers write per-index results into a preallocated slice so output order is
// independent of completion order. Acquisition honors ctx cancellation so a
// timed-out or cancelled run stops scheduling new cases promptly.
func runBounded(ctx context.Context, n, concurrency int, fn func(i int)) {
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			// A panic in one worker must not kill the whole process (the
			// pipeline's own recover only covers runSizeJob's goroutine, not
			// these). The case keeps its zero-value score, which grades as a
			// miss, matching how a failed /run call is treated.
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("bounded worker %d panicked: %v", i, rec)
				}
			}()
			fn(i)
		}(i)
	}
	wg.Wait()
}

type server struct {
	store   *store.Store
	sandbox sandbox.Sandbox
	// allowPrivate relaxes caller-supplied URL checks for local development.
	// Validator-owned loopback sandboxes use a separate trusted client.
	allowPrivate        bool
	allowScreenedImages bool
	softwareVersion     string
	sourceRevision      string
	limiter             *ratelimit.Limiter
	runSlots            chan struct{} // bounds concurrent run_size jobs
	cancelMu            sync.Mutex
	runCancels          map[string]context.CancelFunc
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
	allowScreenedImages := envBool("DITTOBENCH_ALLOW_SCREENED_IMAGES")
	softwareVersion := strings.TrimSpace(os.Getenv("DITTOBENCH_SOFTWARE_VERSION"))
	sourceRevision := strings.TrimSpace(os.Getenv("DITTOBENCH_SOURCE_SHA"))
	runner.Configure(allowPrivate)
	if allowPrivate {
		log.Printf("WARNING: DITTOBENCH_ALLOW_PRIVATE_HARNESS set — SSRF guard relaxed (local/dev only)")
	}
	if allowScreenedImages {
		log.Printf("DITTOBENCH_ALLOW_SCREENED_IMAGES set — trusted validator image path enabled")
	}

	s := &server{
		store:               store.New(),
		sandbox:             sandbox.NewLocalDocker(),
		allowPrivate:        allowPrivate,
		allowScreenedImages: allowScreenedImages,
		softwareVersion:     softwareVersion,
		sourceRevision:      sourceRevision,
		limiter:             ratelimit.New(submitsPerWindow, submitWindow),
		runSlots:            make(chan struct{}, maxConcurrentRuns),
		runCancels:          make(map[string]context.CancelFunc),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /v1/dataset", s.handleDataset)
	mux.HandleFunc("GET /v1/sample", s.handleSample)
	mux.HandleFunc("GET /v1/catalog", s.handleCatalog)
	mux.HandleFunc("POST /v1/submit", s.handleSubmit)
	mux.HandleFunc("POST /v1/score", s.handleScore)
	mux.HandleFunc("POST /v2/score", s.handleVersionedScore)
	mux.HandleFunc("GET /v1/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /v1/runs/{id}/transcript", s.handleGetTranscript)
	mux.HandleFunc("DELETE /v1/runs/{id}", s.handleCancelRun)

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

type capabilitiesResponse struct {
	SoftwareVersion        string `json:"software_version"`
	SourceRevision         string `json:"source_revision"`
	SupportedBenchVersions []int  `json:"supported_bench_versions"`
}

// handleCapabilities reports public release metadata to a co-located validator.
// Identity is supplied by the immutable release descriptor at deploy time and
// the validator accepts a v3 claim only when both fields match that descriptor.
// A bearer token would not authenticate the scorer: the scorer itself would hold
// the token and could use it while lying. Keeping this read-only response
// secretless avoids an operator cutover without weakening the identity binding.
func (s *server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.softwareVersion == "" || !canonicalSourceRevision(s.sourceRevision) {
		writeError(w, http.StatusServiceUnavailable, "scorer release identity unavailable")
		return
	}
	writeJSON(w, http.StatusOK, capabilitiesResponse{
		SoftwareVersion:        s.softwareVersion,
		SourceRevision:         s.sourceRevision,
		SupportedBenchVersions: []int{protocol.BenchVersionV2, protocol.BenchVersionV3},
	})
}

func canonicalSourceRevision(value string) bool {
	if len(value) != 40 || value != strings.ToLower(value) {
		return false
	}
	if value == strings.Repeat("0", 40) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
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
	// The oracle (expected tools/answers, category labels, grading params) is
	// validator-internal grading data. On the public hosted deployment we serve
	// only what the harness itself sees at scoring time (the prompt/question), so
	// this endpoint cannot be scraped into a prompt->answer training set. Trusted
	// local mode (DITTOBENCH_ALLOW_PRIVATE_HARNESS) returns the full labeled
	// dataset for calibration and self-hosted practice.
	if !s.allowPrivate {
		writeJSON(w, http.StatusOK, redactDataset(ds))
		return
	}
	writeJSON(w, http.StatusOK, ds)
}

// publicDataset is the answer-free view of a Dataset served on the public
// practice endpoint: only the harness-visible prompt/question, never the
// expected tools/answers, category label, or grading params.
type publicDataset struct {
	Seed        int64              `json:"seed"`
	GeneratedAt string             `json:"generated_at"`
	ToolCases   []publicToolCase   `json:"tool_cases"`
	MemoryCases []publicMemoryCase `json:"memory_cases,omitempty"`
}

type publicToolCase struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
}

type publicMemoryCase struct {
	ID       string `json:"id"`
	Question string `json:"question"`
}

// redactDataset strips validator-internal grading data, leaving only what the
// harness sees. See the "validator-internal" comments on ToolCase/MemoryCase in
// the dittobench-datagen protocol module.
func redactDataset(ds protocol.Dataset) publicDataset {
	pub := publicDataset{Seed: ds.Seed, GeneratedAt: ds.GeneratedAt}
	for _, c := range ds.ToolCases {
		pub.ToolCases = append(pub.ToolCases, publicToolCase{ID: c.ID, Prompt: c.Prompt})
	}
	for _, c := range ds.MemoryCases {
		pub.MemoryCases = append(pub.MemoryCases, publicMemoryCase{ID: c.ID, Question: c.Question})
	}
	return pub
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
	// BenchVersion selects the deterministic dataset/scoring contract. The
	// version-bound canonical endpoint requires this field; the legacy endpoint
	// and practice path preserve their historical omitted-means-v2 behavior.
	BenchVersion int    `json:"bench_version,omitempty"`
	HarnessURL   string `json:"harness_url,omitempty"`
	GitURL       string `json:"git_url,omitempty"`
	GitRef       string `json:"git_ref,omitempty"`
	// TarballURL is a presigned https URL of a gzipped tar of the harness. The
	// SN118 platform stores miner uploads as tarballs, so the validator hands us
	// the platform's short-lived download URL instead of a git repo. Mutually
	// exclusive with harness_url / git_url; built in the Docker sandbox like
	// git_url. (mode B — platform-tarball ingest.)
	TarballURL string `json:"tarball_url,omitempty"`
	// TarballSHA256 optionally pins the tarball's SHA-256 (hex). When present the
	// sandbox re-verifies the fetched bytes — the platform already checks it at
	// upload, so this is defense in depth against a swapped/corrupted blob.
	TarballSHA256 string `json:"tarball_sha256,omitempty"`
	// ScreenedImageURL is a short-lived URL for a `docker save` archive
	// produced by the trusted screener from this exact tarball. It is
	// accepted only together with TarballURL: the source is still materialized
	// for structural anti-copy fingerprinting, but the Rust crate is not rebuilt.
	ScreenedImageURL string `json:"screened_image_url,omitempty"`
	// ScreenedImageSHA256 pins the archive bytes. ScreenedImageID pins
	// the Docker image loaded from those bytes, preventing an archive/tag swap.
	ScreenedImageSHA256 string            `json:"screened_image_sha256,omitempty"`
	ScreenedImageID     string            `json:"screened_image_id,omitempty"`
	ScreenedImageRef    string            `json:"screened_image_ref,omitempty"`
	ScreenedImageSize   int64             `json:"screened_image_size_bytes,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	N                   int               `json:"n"`
	// RunSize selects the full SN118 pipeline (build → generate fresh anti-cheat
	// dataset → seed haystack → run tool+memory cases → judge → score). One of
	// "small" | "medium" | "full". When set, this path takes precedence.
	RunSize string `json:"run_size,omitempty"`
	// Seed pins the dataset seed (0 = fresh crypto-random per submission).
	Seed int64 `json:"seed,omitempty"`
	// ExpectedDatasetSHA256 pins the exact dataset the caller intends to score.
	// When set, the run regenerates the dataset from Seed (generation is
	// deterministic) and FAILS if the regenerated dataset_sha256 does not match —
	// tamper-evidence for the canonical validator path (POST /v1/score): the
	// platform issues (seed, dataset_sha256) with the ticket, and this guarantees
	// the validator scored precisely that dataset. Empty on the practice path.
	ExpectedDatasetSHA256 string `json:"dataset_sha256,omitempty"`
}

// sourceFromReq builds the sandbox Source for a build submission. git_url and
// tarball_url are mutually exclusive (enforced in handleSubmit); GitRef applies
// only to the git path.
func sourceFromReq(req submitRequest) sandbox.Source {
	return sandbox.Source{
		GitURL:              req.GitURL,
		GitRef:              req.GitRef,
		TarballURL:          req.TarballURL,
		TarballSHA256:       req.TarballSHA256,
		ScreenedImageURL:    req.ScreenedImageURL,
		ScreenedImageSHA256: req.ScreenedImageSHA256,
		ScreenedImageID:     req.ScreenedImageID,
		ScreenedImageRef:    req.ScreenedImageRef,
		ScreenedImageSize:   req.ScreenedImageSize,
	}
}

func validateScreenedImage(req submitRequest) string {
	fieldsSet := req.ScreenedImageURL != "" || req.ScreenedImageSHA256 != "" ||
		req.ScreenedImageID != "" || req.ScreenedImageRef != "" || req.ScreenedImageSize != 0
	if !fieldsSet {
		return ""
	}
	if req.TarballURL == "" {
		return "screened image requires tarball_url for source fingerprinting"
	}
	if req.ScreenedImageURL == "" || req.ScreenedImageSHA256 == "" ||
		req.ScreenedImageID == "" || req.ScreenedImageRef == "" || req.ScreenedImageSize <= 0 {
		return "screened image url, sha256, id, ref, and positive size are required together"
	}
	if req.ScreenedImageSize > 8<<30 {
		return "screened_image_size_bytes exceeds the 8 GiB limit"
	}
	if len(req.ScreenedImageSHA256) != 64 {
		return "screened_image_sha256 must be 64 lowercase hex characters"
	}
	if _, err := hex.DecodeString(req.ScreenedImageSHA256); err != nil ||
		req.ScreenedImageSHA256 != strings.ToLower(req.ScreenedImageSHA256) {
		return "screened_image_sha256 must be 64 lowercase hex characters"
	}
	if !strings.HasPrefix(req.ScreenedImageID, "sha256:") || len(req.ScreenedImageID) != 71 {
		return "screened_image_id must be a sha256 Docker image id"
	}
	imageHex := strings.TrimPrefix(req.ScreenedImageID, "sha256:")
	if _, err := hex.DecodeString(imageHex); err != nil || imageHex != strings.ToLower(imageHex) {
		return "screened_image_id must be a sha256 Docker image id"
	}
	const refPrefix = "ditto-screen/"
	const refSuffix = ":latest"
	refID := strings.TrimSuffix(strings.TrimPrefix(req.ScreenedImageRef, refPrefix), refSuffix)
	parsedRefID, refErr := uuid.Parse(refID)
	if !strings.HasPrefix(req.ScreenedImageRef, refPrefix) || !strings.HasSuffix(req.ScreenedImageRef, refSuffix) ||
		refErr != nil || parsedRefID.String() != refID {
		return "screened_image_ref must be the screener-owned ditto-screen reference"
	}
	return ""
}

func validateScreenedImageAccess(req submitRequest, allowScreenedImages bool) string {
	if req.ScreenedImageURL != "" && !allowScreenedImages {
		return "screened images are only accepted by the validator sandbox"
	}
	return ""
}

func validateBenchmarkImageContract(req submitRequest) string {
	if req.BenchVersion == 3 && req.ScreenedImageURL == "" {
		return "benchmark version 3 requires a screener-built image; source builds are disabled"
	}
	return ""
}

// submitResponse is returned by the direct (synchronous) path.
type submitResponse struct {
	RunID        string  `json:"run_id"`
	Status       string  `json:"status"`
	Composite    float64 `json:"composite"`
	ToolMean     float64 `json:"tool_mean"`
	MedianMs     int64   `json:"median_ms"`
	N            int     `json:"n"`
	Seed         int64   `json:"seed"`
	BenchVersion int     `json:"bench_version"`
}

// acceptedResponse is returned by the sandbox (asynchronous) path; poll
// GET /v1/runs/{id} for status + report.
type acceptedResponse struct {
	RunID        string `json:"run_id"`
	Status       string `json:"status"`
	Poll         string `json:"poll"`
	BenchVersion int    `json:"bench_version"`
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
	version, msg := requestedBenchVersion(req.BenchVersion, false)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	req.BenchVersion = version
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
	if msg := validateScreenedImage(req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// Screened-image metadata is an integrity pin, not caller authentication.
	// Only validator-owned/private deployments may bypass the source build; the
	// public unauthenticated practice API must always build submitted source.
	if msg := validateScreenedImageAccess(req, s.allowScreenedImages); msg != "" {
		writeError(w, http.StatusForbidden, msg)
		return
	}
	if msg := validateBenchmarkImageContract(req); msg != "" {
		writeError(w, http.StatusForbidden, msg)
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
	if req.ScreenedImageURL != "" {
		if err := netguard.ValidateURL(req.ScreenedImageURL, s.allowPrivate); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// run_size selects the full SN118 pipeline (build + fresh anti-cheat dataset +
	// memory seeding + deterministic grading). Requires a buildable/runnable source.
	if req.RunSize != "" {
		s.submitRunSize(w, r, req)
		return
	}
	if req.BenchVersion != 2 {
		writeError(w, http.StatusBadRequest, "bench_version 3 requires run_size so the complete versioned benchmark is administered")
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

// handleScore is the CANONICAL validator scoring path. Unlike /v1/submit (miner
// practice: fresh seed, generate-and-score), a validator scores the EXACT dataset
// the platform issued with its ticket, so this endpoint requires a pinned seed, a
// run_size, and the platform's dataset_sha256, and it fails the run if the
// regenerated dataset does not hash to that value (tamper-evidence). It reuses
// the full run_size pipeline (build/run → seed → judge → score → signed report).
func (s *server) handleScore(w http.ResponseWriter, r *http.Request) {
	s.handleScoreRequest(w, r, false)
}

// handleVersionedScore is the capability-negotiated canonical scoring path.
// Unlike the legacy /v1/score route, it never guesses the benchmark contract:
// bench_version must be present and supported.
func (s *server) handleVersionedScore(w http.ResponseWriter, r *http.Request) {
	s.handleScoreRequest(w, r, true)
}

func (s *server) handleScoreRequest(w http.ResponseWriter, r *http.Request, requireExplicitVersion bool) {
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
	version, msg := requestedBenchVersion(req.BenchVersion, requireExplicitVersion)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	req.BenchVersion = version
	// Validator-specific preconditions (beyond what submitRunSize enforces).
	if req.Seed == 0 {
		writeError(w, http.StatusBadRequest, "seed is required (the platform-issued dataset seed) for canonical scoring")
		return
	}
	if strings.TrimSpace(req.ExpectedDatasetSHA256) == "" {
		writeError(w, http.StatusBadRequest, "dataset_sha256 is required (the platform-issued dataset hash) for canonical scoring")
		return
	}
	if req.RunSize == "" {
		writeError(w, http.StatusBadRequest, "run_size is required (small|medium|full) for canonical scoring")
		return
	}
	// Exactly one harness source; SSRF-guard any caller-supplied URL (same rules
	// as /v1/submit).
	sources := 0
	for _, set := range []bool{req.HarnessURL != "", req.GitURL != "", req.TarballURL != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		writeError(w, http.StatusBadRequest, "provide exactly one of harness_url, git_url, or tarball_url")
		return
	}
	if msg := validateScreenedImage(req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if msg := validateScreenedImageAccess(req, s.allowScreenedImages); msg != "" {
		writeError(w, http.StatusForbidden, msg)
		return
	}
	if msg := validateBenchmarkImageContract(req); msg != "" {
		writeError(w, http.StatusForbidden, msg)
		return
	}
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
	if req.ScreenedImageURL != "" {
		if err := netguard.ValidateURL(req.ScreenedImageURL, s.allowPrivate); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// Delegate to the shared run_size pipeline; the dataset_sha256 pin is enforced
	// inside runSizeJob after regeneration.
	s.submitRunSize(w, r, req)
}

func requestedBenchVersion(requested int, requireExplicit bool) (int, string) {
	if requested == 0 {
		if requireExplicit {
			return 0, "bench_version is required (supported: 2, 3)"
		}
		return 2, ""
	}
	if !protocol.SupportedBenchVersion(requested) {
		return 0, "unsupported bench_version (supported: 2, 3)"
	}
	return requested, ""
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
	s.store.SetBenchVersion(runID, req.BenchVersion)

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
		RunID:        report.RunID,
		Status:       string(store.StatusDone),
		Composite:    report.Composite,
		ToolMean:     report.ToolMean,
		MedianMs:     report.MedianMs,
		N:            report.N,
		Seed:         seed,
		BenchVersion: req.BenchVersion,
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
	if !s.acquireRunSlot(w) {
		return
	}

	seed := pinnedOrFreshSeed(req.Seed)
	runID := uuid.NewString()
	s.store.Create(runID, "sandbox", store.StatusQueued, seed, n)
	s.store.SetBenchVersion(runID, req.BenchVersion)

	// Detach from the request context so the long build survives the response.
	go s.runSandboxJob(context.Background(), runID, req, seed, n)

	writeJSON(w, http.StatusAccepted, acceptedResponse{
		RunID:        runID,
		Status:       string(store.StatusQueued),
		Poll:         "/v1/runs/" + runID,
		BenchVersion: req.BenchVersion,
	})
}

// runSandboxJob builds the submission, runs it, evaluates it, and tears it down,
// updating the job status at each step.
func (s *server) runSandboxJob(ctx context.Context, runID string, req submitRequest, seed int64, n int) {
	defer func() { <-s.runSlots }()
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
	defer s.sandbox.Release(context.Background(), image)

	handle, err := s.sandbox.Run(ctx, image, sandboxRuntimeEnv(req.Env))
	if err != nil {
		s.store.Fail(runID, "container start failed: "+err.Error())
		return
	}
	defer s.finishSandboxRun(runID, handle)
	ctx = runner.TrustSandbox(ctx)

	if err := s.waitSandboxHealthy(ctx, handle, sandboxHealthTimeout); err != nil {
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
	// Generation is deterministic, scoring is judge-free, and the harness talks
	// only to the locked-model gateway, so no LLM key exists anywhere in a run.

	if !s.acquireRunSlot(w) {
		return
	}

	seed := pinnedOrFreshSeed(req.Seed)
	runID := uuid.NewString()
	s.store.Create(runID, "run_size", store.StatusQueued, seed, prof.Tools+prof.Mem)
	s.store.SetRunSize(runID, req.RunSize)
	s.store.SetBenchVersion(runID, req.BenchVersion)
	log.Printf("run %s: run_size=%s bench_version=%d seed=%d tools=%d mem=%d",
		runID, req.RunSize, req.BenchVersion, seed, prof.Tools, prof.Mem)

	runCtx, cancel := context.WithCancel(context.Background())
	s.registerRunCancel(runID, cancel)
	go s.runSizeJob(runCtx, runID, req, prof, seed)

	writeJSON(w, http.StatusAccepted, acceptedResponse{
		RunID:        runID,
		Status:       string(store.StatusQueued),
		Poll:         "/v1/runs/" + runID,
		BenchVersion: req.BenchVersion,
	})
}

func (s *server) acquireRunSlot(w http.ResponseWriter) bool {
	select {
	case s.runSlots <- struct{}{}:
		return true
	default:
		writeError(w, http.StatusTooManyRequests, "validator at capacity; retry shortly")
		return false
	}
}

// runScope classifies a run_size request as SCORED or PRACTICE. The canonical
// on-chain path (POST /v1/score) pins the exact dataset the platform issued with
// its ticket (dataset_sha256), so its report feeds the KOTH ledger and scoring is
// trustless-strict: observed execution is mandatory and the parser free points
// close. A run_size PRACTICE submission (POST /v1/submit, no dataset_sha256) keeps
// the lenient self-hostable scoring. The scope is a pure property of the request,
// so any third party re-deriving (dataset, transcript, scope) reproduces the
// score — no validator secret enters the score path.
func runScope(req submitRequest) scorer.Scope {
	if strings.TrimSpace(req.ExpectedDatasetSHA256) != "" {
		return scorer.ScopeScored
	}
	return scorer.ScopePractice
}

// runSizeJob is the full SN118 pipeline: building → generating → seeding →
// running (appending partials) → scoring → done. Every stage is deterministic;
// the only LLM in the loop is the locked model the harness itself talks to.
func (s *server) runSizeJob(ctx context.Context, runID string, req submitRequest, prof gen.Profile, seed int64) {
	defer func() { <-s.runSlots }() // release the concurrency slot
	defer s.unregisterRunCancel(runID)
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
		defer s.sandbox.Release(context.Background(), image)
	}

	// 2. generating — fresh, anti-cheat dataset (tools + memory haystack).
	s.store.SetStage(runID, store.StatusGenerating, 0, total)
	rng, rngErr := gen.NewRNGForVersion(seed, req.BenchVersion)
	if rngErr != nil {
		s.store.Fail(runID, "unsupported bench_version during generation")
		return
	}
	toolCases, toolPara := gen.GenerateTools(rng, seed, prof.Tools)
	// v2 memory engine (bench_version 2): a fresh procedural persona universe
	// per seed replaces the static LongMemEval fixture. Generation is fully
	// non-LLM and a pure function of the master `seed`; selection shares the run
	// rng. The suite lays cases out across seeding tiers (A prepared, B raw-pairs)
	// and staged Tier-C waves.
	// Always pass the version explicitly. The un-suffixed wrappers default to
	// the FROZEN v2 contract, so an implicit call here would score the run
	// against the pre-hardening dataset while the report claimed v3.
	memSuite, memErr := gen.GenerateMemorySuiteForVersion(rng, seed, prof.Mem, prof.Waves, prof.RawPairsFrac, req.BenchVersion)
	if memErr != nil {
		s.store.Fail(runID, "memory dataset generation failed for requested bench_version")
		return
	}
	// Multi-graph isolation: seed a second persona under a different
	// user_id and add cross-user isolation cases. The secondary graph is template-
	// rendered. Cases carry the user_id they must be answered under; they merge
	// into the primary staged-case stream.
	iso, isoErr := gen.GenerateIsolationForVersion(seed, prof.Mem, prof.Waves, prof.IsoCases, req.BenchVersion)
	if isoErr != nil {
		s.store.Fail(runID, "isolation dataset generation failed for requested bench_version")
		return
	}
	memSuite.Cases = append(memSuite.Cases, iso.Cases...)
	para := toolPara
	para.Add(memSuite.Stats)
	totalPairs, totalSubjects := 0, 0
	for _, w := range memSuite.Waves {
		totalPairs += len(w.Pairs)
		totalSubjects += len(w.Subjects)
	}
	log.Printf("run %s generated: %d tool cases, %d memory cases, %d haystack pairs (%d subjects) across %d wave(s), %d Tier-B cases; paraphrase attempted=%d applied=%d retried=%d fallback=%d",
		runID, len(toolCases), len(memSuite.Cases), totalPairs, totalSubjects, memSuite.SeedingWaves, memSuite.TierBCases,
		para.Attempted, para.Applied, para.Retried, para.Fallback)
	total = prof.Tools + len(memSuite.Cases) // actual case count for progress

	// Dataset hashing + optional artifact persistence:
	// hash the fully-rendered dataset so a dispute can be pinned to exact bytes;
	// when DITTOBENCH_ARTIFACT_DIR is set, persist the artifact keyed by run_id
	// (the local-file form of the "upload rendered dataset" persistence; a
	// platform bucket is the drop-in replacement for os.WriteFile here).
	// Observed execution: derive each tool case's live mock-tool environment (the Fixtures
	// back the mock endpoint stood up below). The hashed artifact — assembled by
	// gen.BuildArtifact so the run path and the generate service produce identical
	// bytes for a seed — recomputes the fixture digests from the same (seed, case).
	toolFixtures := make([]toolexec.Fixture, len(toolCases))
	for i, c := range toolCases {
		toolFixtures[i] = toolexec.BuildFixture(seed, c)
	}
	// The hashed artifact covers the secondary isolation graph too (when present),
	// so a dispute re-scores the exact multi-graph seeding.
	memWaves := gen.MergeMemoryWaves(memSuite.Waves, iso.SecondaryWave)
	artifact, artifactErr := gen.BuildArtifactForVersion(seed, req.BenchVersion, toolCases, memSuite.Cases, memWaves)
	if artifactErr != nil {
		s.store.Fail(runID, "dataset generation failed for requested bench_version")
		return
	}
	if artifact.BenchVersion != req.BenchVersion {
		s.store.Fail(runID, fmt.Sprintf("bench_version mismatch: requested %d but generated artifact reports %d", req.BenchVersion, artifact.BenchVersion))
		return
	}
	datasetHash, artifactBytes, hashErr := artifact.SHA256Hex()
	if hashErr != nil {
		log.Printf("run %s: dataset hashing failed: %v", runID, hashErr)
	}
	// Canonical validator path (/v1/score): the platform pins the dataset it
	// issued with the ticket. Regeneration is deterministic, so a mismatch means
	// the generator/bench_version drifted from what the platform shipped — fail
	// loudly rather than score a different dataset than the one under dispute.
	if want := strings.TrimSpace(req.ExpectedDatasetSHA256); want != "" && hashErr == nil && !strings.EqualFold(want, datasetHash) {
		s.store.Fail(runID, "dataset_sha256 mismatch: platform issued "+want+" but this validator regenerated "+datasetHash+" (generator/bench_version drift)")
		log.Printf("run %s: dataset_sha256 mismatch (want %s got %s)", runID, want, datasetHash)
		return
	}
	if dir := strings.TrimSpace(os.Getenv("DITTOBENCH_ARTIFACT_DIR")); dir != "" && artifactBytes != nil {
		if err := os.WriteFile(filepath.Join(dir, runID+".json"), artifactBytes, 0o644); err != nil {
			log.Printf("run %s: artifact persist failed: %v", runID, err)
		}
	}

	// 3. start the container. On the local harness_url path we skip the container
	//    and target the miner's already-running harness.
	harnessURL := req.HarnessURL
	var handle *sandbox.Handle
	var runErr error
	if image != "" {
		env := harnessSandboxEnv(req.Env)
		handle, runErr = s.sandbox.Run(ctx, image, env)
		if runErr != nil {
			s.store.Fail(runID, "container start failed: "+runErr.Error())
			return
		}
		defer s.finishSandboxRun(runID, handle)
		harnessURL = handle.BaseURL
		ctx = runner.TrustSandbox(ctx)
	}

	var healthErr error
	if handle != nil {
		healthErr = s.waitSandboxHealthy(ctx, handle, sandboxHealthTimeout)
	} else {
		healthErr = runner.WaitHealthy(ctx, harnessURL, sandboxHealthTimeout)
	}
	if healthErr != nil {
		s.store.Fail(runID, "harness never became healthy: "+healthErr.Error())
		return
	}

	tools := catalog.Catalog()
	perCase := make([]protocol.CaseScore, 0, total)

	// Observed tool execution: stand up the validator's mock tool
	// endpoint. It serves deterministic, seed-derived results for external-world
	// tools AND records the real trajectory, so tool cases are scored on what the
	// validator observed rather than the harness's self-report.
	// Registered for every case (tool + memory) so a harness may route any
	// non-memory call through it during any case.
	toolSrv := toolexec.NewServer()
	for i, c := range toolCases {
		toolSrv.Register(c.ID, toolFixtures[i])
	}
	for _, sc := range memSuite.Cases {
		toolSrv.Register(sc.Case.ID, toolexec.BuildFixture(seed, protocol.ToolCase{ID: sc.Case.ID}))
	}
	toolEndpoint, stopToolSrv, err := s.startToolServer(toolSrv, image != "")
	if err != nil {
		s.store.Fail(runID, "tool endpoint start failed: "+err.Error())
		return
	}
	defer stopToolSrv()

	scope := runScope(req)

	if toolEndpoint == "" {
		if scope == scorer.ScopeScored {
			// Observed execution is mandatory on the scored path: without the mock
			// endpoint reachable, observable tool cases can never be watched execute
			// and would all score 0, which is not a defensible score. The on-chain
			// validator always builds+runs the harness in Docker (endpoint served via
			// host.docker.internal), so this only trips on a misconfigured scored run
			// (e.g. a remote harness_url that cannot reach our loopback). Fail loudly
			// rather than emit a zeroed report.
			s.store.Fail(runID, "scored run cannot observe tool execution: tool_endpoint unreachable from the harness (build+run in the Docker sandbox, or use a locally reachable harness). Observed execution is mandatory on the scored path.")
			log.Printf("run %s: scored run aborted — tool_endpoint not advertised (harness cannot be observed)", runID)
			return
		}
		log.Printf("run %s: tool_endpoint not advertised (remote harness unreachable); observable tool cases scored capped (practice)", runID)
	}

	// Reachability preflight: an advertised endpoint can still be unreachable
	// from the harness's network namespace (Docker routing, network policy, a
	// runtime fault). Validator state alone cannot distinguish that from a
	// harness that legitimately never calls tools — both produce zero observed
	// calls — so an active probe the harness participates in settles it before
	// any case is scored. On the scored path a failed probe fails the run
	// (rescheduled) instead of completing a zeroed report.
	if toolEndpoint != "" {
		if !s.enforcePreflight(ctx, runID, scope, harnessURL, toolEndpoint, toolSrv, seed, tools) {
			return
		}
	}

	// 4. tool cases — independent of the memory haystack and of each other, so
	//    run before seeding and with bounded per-case concurrency. Results are
	//    written per index so the report order is identical to sequential
	//    execution regardless of completion order.
	observedTool, cappedTool := 0, 0
	s.store.SetStage(runID, store.StatusRunning, 0, total)
	toolResults := make([]protocol.CaseScore, len(toolCases))
	toolWasObserved := make([]bool, len(toolCases))
	toolWasCapped := make([]bool, len(toolCases))
	toolTranscripts := make([]transcriptCase, len(toolCases))
	runBounded(ctx, len(toolCases), caseConcurrency, func(i int) {
		c := toolCases[i]
		resp, runErr := runner.RunCase(ctx, harnessURL, c.ID, c.Prompt, tools, runner.CaseOptions{ToolEndpoint: toolEndpoint})
		observed := toolSrv.Observed(c.ID)
		toolTranscripts[i] = transcriptCase{CaseID: c.ID, Kind: protocol.KindTool, Response: resp, Observed: observed}
		cs := scorer.ScoreToolCaseObservedScope(c, resp, runErr == nil, observed, scope)
		if datagen.IsResultUsage(c.Category) {
			// Result-usage: trajectory + whether the answer carried the served
			// needle value (a fabricated value only the executed tool could
			// reveal). An answer carrying the served DECOY (a plausible number
			// fished from the wrong tool's result) zeros the usage half too.
			cs = scorer.ComposeResultUsageWithDecoy(cs, resp.FinalText,
				toolFixtures[i].NeedleValue(), toolFixtures[i].DecoyValue())
		} else {
			cs = scorer.FinishTool(cs)
		}
		switch {
		case len(observed) > 0:
			toolWasObserved[i] = true
		case toolexec.Observable(c):
			// Unobserved observable case: capped at 0.5 in practice, 0 when scored.
			cs = scorer.CapUnobservedScope(cs, scope)
			toolWasCapped[i] = true
		}
		toolResults[i] = cs
		s.store.AppendPartial(runID, cs) // store append is mutex-guarded
	})
	// A cancelled context aborts runBounded early, leaving unexecuted slots as
	// zero-value CaseScores. Never fold those into a report: the cancel handler
	// has already failed the run, so just abandon it.
	if ctx.Err() != nil {
		log.Printf("run %s: cancelled during tool cases; abandoning without a report", runID)
		return
	}
	for i, cs := range toolResults {
		perCase = append(perCase, cs)
		if toolWasObserved[i] {
			observedTool++
		} else if toolWasCapped[i] {
			cappedTool++
		}
	}
	transcripts := append(make([]transcriptCase, 0, total), toolTranscripts...)

	// 5. memory cases — staged Tier-C ingestion: seed a wave,
	//    then run the cases it unlocks (all their evidence is now seeded), then
	//    the next wave. A single-wave run degrades to seed-then-run-all.
	casesByWave := make([][]gen.StagedCase, memSuite.SeedingWaves)
	for _, sc := range memSuite.Cases {
		w := sc.RunAfterWave
		if w < 0 {
			w = 0
		}
		if w >= memSuite.SeedingWaves {
			w = memSuite.SeedingWaves - 1
		}
		casesByWave[w] = append(casesByWave[w], sc)
	}
	// Seed the secondary isolation graph up front (a distinct user_id), so cross-
	// user isolation cases can run in any wave.
	if len(iso.SecondaryWave.Pairs) > 0 {
		s.store.SetStage(runID, store.StatusSeeding, len(perCase), total)
		if _, err := runner.Seed(ctx, harnessURL, iso.SecondaryWave); err != nil {
			s.store.Fail(runID, "seeding secondary isolation graph failed: "+err.Error())
			return
		}
	}
	for w, wave := range memSuite.Waves {
		if len(wave.Pairs) > 0 {
			s.store.SetStage(runID, store.StatusSeeding, len(perCase), total)
			if _, err := runner.Seed(ctx, harnessURL, wave); err != nil {
				s.store.Fail(runID, fmt.Sprintf("seeding haystack wave %d failed: %s", w, err.Error()))
				return
			}
		}
		s.store.SetStage(runID, store.StatusRunning, len(perCase), total)
		// Cases within one wave are independent: their evidence is fully seeded
		// (this wave and all prior waves), lifecycle WRITE cases live only in wave
		// 0 and their READ cases in a later wave, and same-wave writes target
		// distinct keys — so they run with bounded concurrency. The wave boundary
		// stays a barrier: seed wave w, run its cases, then seed wave w+1.
		waveCases := casesByWave[w]
		waveResults := make([]protocol.CaseScore, len(waveCases))
		waveTranscripts := make([]transcriptCase, len(waveCases))
		runBounded(ctx, len(waveCases), caseConcurrency, func(i int) {
			sc := waveCases[i]
			mc := sc.Case
			// Scope the query to the case's memory graph: isolation cases carry an
			// explicit user_id; all others default to the primary wave's user.
			uid := sc.UserID
			if uid == "" {
				uid = wave.UserID
			}
			resp, _ := runner.RunCase(ctx, harnessURL, mc.ID, mc.Question, tools, runner.CaseOptions{ToolEndpoint: toolEndpoint, UserID: uid})
			observedCalls := toolSrv.Observed(mc.ID)
			resp = withObservedTrajectory(resp, observedCalls)
			waveTranscripts[i] = transcriptCase{CaseID: mc.ID, Kind: protocol.KindMemory, UserID: uid, Response: resp, Observed: observedCalls}
			cs := scorer.GradeMemory(mc, resp)
			if len(observedCalls) > 0 {
				// The graded response's ToolCalls (and thus cs.Called) are the
				// validator-observed trajectory, not the harness self-report. Mark it
				// so a consumer/auditor reading the report — e.g. reviewing a BaitTool
				// zero — knows cs.Called is authoritative, matching the tool-case path.
				cs.Observed = true
				cs.Notes = append(cs.Notes, "called reflects the validator-observed trajectory")
			}
			waveResults[i] = cs
			s.store.AppendPartial(runID, cs) // store append is mutex-guarded
		})
		// Same zero-value guard as the tool loop: a cancellation mid-wave must
		// not fold half-empty results into a report.
		if ctx.Err() != nil {
			log.Printf("run %s: cancelled during memory wave %d; abandoning without a report", runID, w)
			return
		}
		perCase = append(perCase, waveResults...)
		transcripts = append(transcripts, waveTranscripts...)
	}

	// 6. scoring — aggregate + finish.
	s.store.SetStage(runID, store.StatusScoring, len(perCase), total)
	report := scorer.Aggregate(runID, perCase)
	report.Seed = seed
	report.StructuralFingerprint = structuralFP
	injections := 0
	for _, cs := range perCase {
		if cs.Injection {
			injections++
		}
	}
	report.Details = &protocol.RunDetails{
		BenchVersion:      req.BenchVersion,
		DatasetSHA256:     datasetHash,
		Paraphrase:        &para,
		InjectionAttempts: injections,
		ToolMean:          report.ToolMean,
		MemoryMean:        report.MemoryMean,
		SeedingWaves:      memSuite.SeedingWaves,
		RawPairsCases:     memSuite.TierBCases,
		ObservedToolCases: observedTool,
		CappedToolCases:   cappedTool,
		IsolationCases:    len(iso.Cases),
		LifecycleCases:    memSuite.LifecycleCases,
		ToolEfficiency:    scorer.ToolEfficiencyFactor(perCase),
		// Generation and grading are both deterministic and non-LLM; the only
		// model in a run is the locked one the harness talks to.
		Models: &protocol.ModelInfo{
			Harness: llm.HarnessModel(),
		},
		PerCategory: report.PerCategory,
	}
	if memSuite.LexicalGap.Questions > 0 {
		lg := memSuite.LexicalGap
		report.Details.LexicalGap = &lg
	}
	report.Details.MetamorphicConsistency = scorer.MetamorphicConsistency(perCase)
	if tr, pairs := scorer.TransformRobustness(perCase); tr != nil {
		report.Details.TransformRobustness = tr
		report.Details.AuditCaseCount = pairs
	}
	// The pair COUNTS are what a brittleness verdict is computed from; the
	// robustness ratio above stays for continuity and human reading. Counts pool
	// across runs and validators, and they preserve the direction of a split,
	// which is the only part of the signal that separates a brittle harness from
	// a noisy honest one.
	if ap := scorer.AuditPairs(perCase); ap.Total() > 0 {
		report.Details.AuditPairs = &ap
	}
	if brier, cn := scorer.CalibrationBrier(perCase); brier != nil {
		report.Details.CalibrationBrier = brier
		report.Details.CalibrationN = cn
	}
	if injections > 0 {
		log.Printf("run %s: %d injection-compliance case(s) flagged", runID, injections)
	}
	if err := validateBenchVersionResult(req.BenchVersion, artifact.BenchVersion, report.Details); err != nil {
		s.store.Fail(runID, err.Error())
		return
	}

	// Offline reproducibility: content-address the transcript artifact (the
	// graded per-case inputs) and attach it to the run. The grader is public and
	// deterministic, so (dataset regenerated from seed) + (this transcript)
	// re-grades to the same numbers; the digest travels with the run status so
	// the platform can bind it into the signed score payload and publish the
	// bytes. A hashing failure is logged, never fatal: the score itself does not
	// depend on the artifact.
	tArtifact := transcriptArtifact{
		RunID:         runID,
		Seed:          seed,
		BenchVersion:  artifact.BenchVersion,
		DatasetSHA256: datasetHash,
		Cases:         transcripts,
	}
	if tSHA, tBody, tErr := tArtifact.canonicalBytes(); tErr != nil {
		log.Printf("run %s: transcript hashing failed: %v", runID, tErr)
	} else {
		s.store.SetTranscript(runID, tSHA, tBody)
		if dir := strings.TrimSpace(os.Getenv("DITTOBENCH_ARTIFACT_DIR")); dir != "" {
			if err := os.WriteFile(filepath.Join(dir, runID+".transcript.json"), tBody, 0o644); err != nil {
				log.Printf("run %s: transcript persist failed: %v", runID, err)
			}
		}
		log.Printf("run %s: transcript_sha256=%s (%d cases)", runID, tSHA, len(transcripts))
	}
	s.store.Finish(runID, report)
	log.Printf("run %s done: bench_version=%d composite=%.3f tool_mean=%.3f memory_mean=%.3f observed=%d capped=%d",
		runID, req.BenchVersion, report.Composite, report.ToolMean, report.MemoryMean, observedTool, cappedTool)
}

func validateBenchVersionResult(requested, artifactVersion int, details *protocol.RunDetails) error {
	if requested != artifactVersion {
		return fmt.Errorf("bench_version contradiction: request=%d artifact=%d", requested, artifactVersion)
	}
	if details == nil || details.BenchVersion != requested {
		reported := 0
		if details != nil {
			reported = details.BenchVersion
		}
		return fmt.Errorf("bench_version contradiction: request=%d report=%d", requested, reported)
	}
	return nil
}

func (s *server) finishSandboxRun(runID string, handle *sandbox.Handle) {
	job, ok := s.store.Get(runID)
	if ok && job.Status == store.StatusFailed {
		diagnostics := s.sandbox.Diagnostics(context.Background(), handle)
		failure := &store.Failure{
			Kind:      "sandbox_failure",
			Code:      "sandbox_runtime",
			Retryable: false,
			Diagnostics: map[string]any{
				"running":       diagnostics.Running,
				"oom_killed":    diagnostics.OOMKilled,
				"exit_code":     diagnostics.ExitCode,
				"memory_events": diagnostics.MemoryEvents,
			},
		}
		if diagnostics.MemoryPeakBytes != nil {
			failure.Diagnostics["memory_peak_bytes"] = *diagnostics.MemoryPeakBytes
		}
		if diagnostics.TmpfsUsedBytes != nil {
			failure.Diagnostics["tmpfs_used_bytes"] = *diagnostics.TmpfsUsedBytes
		}
		if diagnostics.TmpfsCapacityBytes != nil {
			failure.Diagnostics["tmpfs_capacity_bytes"] = *diagnostics.TmpfsCapacityBytes
		}
		message := job.Error
		if code := diagnostics.InfrastructureCode(); code != "" {
			failure.Kind = "validator_infrastructure"
			failure.Code = code
			failure.Retryable = true
			message = "validator sandbox resource envelope exhausted"
			log.Printf(
				"run %s validator infrastructure failure code=%s oom=%t exit=%d",
				runID, code, diagnostics.OOMKilled, diagnostics.ExitCode,
			)
		}
		s.store.FailWith(runID, message, failure)
	}
	s.sandbox.Stop(context.Background(), handle)
}

// withObservedTrajectory substitutes the validator-OBSERVED tool trajectory for
// the harness's self-reported ToolCalls before a memory case is graded. This is
// what makes injection-bait compliance unscrubable: a bait action-tool call
// recorded by the mock endpoint reaches the grader even if the harness deleted
// every trace of it from its response text. When nothing was observed (a
// harness that stubs tools locally, or a pure recall answer that called no
// served tool), the self-report is left untouched, so honest harnesses are
// unaffected.
func withObservedTrajectory(resp protocol.RunResponse, observed []protocol.ObservedToolCall) protocol.RunResponse {
	if len(observed) > 0 {
		resp.ToolCalls = observed
	}
	return resp
}

// startToolServer stands up the observed-execution mock tool endpoint on an
// ephemeral host port, serving h at POST /tool. It returns the URL the harness
// should use (reachable from where the harness runs) and a stop func. The URL is
// empty when the harness cannot reach our loopback port — a remote hosted
// harness_url with the SSRF guard on — in which case observable tool cases are
// scored capped (the harness simply won't call it). Listens on all interfaces so
// a Docker-sandboxed container reaches it via host.docker.internal.
func (s *server) startToolServer(h http.Handler, docker bool) (endpoint string, stop func(), err error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return "", func() {}, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.Handle("POST /tool", h)
	srv := &http.Server{Handler: mux}
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("tool endpoint serve error: %v", serveErr)
		}
	}()
	stop = func() { _ = srv.Close() }

	switch {
	case docker:
		// Docker sandbox: the container reaches the host at host.docker.internal
		// (mapped to host-gateway when the container is started, see sandbox.Run).
		endpoint = fmt.Sprintf("http://host.docker.internal:%d/tool", port)
	case s.allowPrivate:
		// Local dev: the harness runs on the same host as the validator.
		endpoint = fmt.Sprintf("http://127.0.0.1:%d/tool", port)
	default:
		// Hosted practice with a remote harness_url: it cannot reach our loopback
		// port. Leave the endpoint unadvertised; observable cases score capped.
		endpoint = ""
	}
	return endpoint, stop, nil
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

func (s *server) registerRunCancel(runID string, cancel context.CancelFunc) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.runCancels == nil {
		s.runCancels = make(map[string]context.CancelFunc)
	}
	s.runCancels[runID] = cancel
}

func (s *server) unregisterRunCancel(runID string) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	delete(s.runCancels, runID)
}

func (s *server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if job.Status == store.StatusDone || job.Status == store.StatusFailed {
		writeJSON(w, http.StatusOK, job)
		return
	}

	s.cancelMu.Lock()
	cancel := s.runCancels[id]
	s.cancelMu.Unlock()
	if cancel == nil {
		writeError(w, http.StatusConflict, "run is not cancellable")
		return
	}
	cancel()
	s.store.Fail(id, "run cancelled by client")
	job, _ = s.store.Get(id)
	writeJSON(w, http.StatusAccepted, job)
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

// handleGetTranscript serves a completed run's canonical transcript artifact —
// the graded per-case inputs whose SHA-256 is published as the run's
// transcript_sha256. Anyone holding these bytes plus the seed-regenerated
// dataset can re-run the public grader and reproduce the score offline.
func (s *server) handleGetTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if len(job.Transcript) == 0 {
		writeError(w, http.StatusNotFound, "run has no transcript (not finished, failed, or pre-transcript)")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Transcript-SHA256", job.TranscriptSHA256)
	w.WriteHeader(http.StatusOK)
	w.Write(job.Transcript)
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

// envOr returns the env var value or def when unset/blank.
func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func (s *server) waitSandboxHealthy(
	ctx context.Context, handle *sandbox.Handle, timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if last = runner.Health(ctx, handle.BaseURL); last == nil {
			return nil
		}
		diagnostics := s.sandbox.Diagnostics(ctx, handle)
		if diagnostics.StateKnown && !diagnostics.Running {
			return fmt.Errorf(
				"harness exited before health: exit_code=%d oom_killed=%t",
				diagnostics.ExitCode,
				diagnostics.OOMKilled,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if last == nil {
		last = fmt.Errorf("timeout")
	}
	return fmt.Errorf("harness not healthy after %s: %w", timeout, last)
}

// lockedProvider is the frozen crate provider. The whole fleet reaches the
// locked model through this one OpenAI-compatible path, so serving differences
// do not make the k=3 validators' scores less comparable. Not env-tunable.
const lockedProvider = "chutes"

// lockedEnvKeys are the sandbox env vars the model lock owns. A caller-supplied
// req.Env may not set any of these — otherwise a miner could route around the
// locked model (point at OpenRouter, Chutes, or any OpenAI-compatible host, swap
// the model id, or redirect the gateway URL). Every provider selector any
// supported crate honors must be listed here.
var lockedEnvKeys = map[string]bool{
	"OPENROUTER_API_KEY":  true,
	"DITTOBENCH_PROVIDER": true,
	"DITTOBENCH_MODEL":    true,
	"OLLAMA_BASE_URL":     true,
	"CHUTES_API_KEY":      true,
	"CHUTES_BASE_URL":     true,
	"OPENAI_API_KEY":      true,
	"OPENAI_BASE_URL":     true,
	"DITTOBENCH_DB":       true,
}

// sandboxRuntimeEnv applies filesystem invariants shared by practice and
// canonical scoring without changing the practice endpoint's provider env.
func sandboxRuntimeEnv(reqEnv map[string]string) map[string]string {
	env := make(map[string]string, len(reqEnv)+1)
	for key, value := range reqEnv {
		if key != "DITTOBENCH_DB" {
			env[key] = value
		}
	}
	env["DITTOBENCH_DB"] = "/tmp/dittobench.db"
	return env
}

// harnessSandboxEnv builds the env for the miner sandbox container.
//
// The harness is scored against ONE locked open-weight model (llm.HarnessModel)
// served by the host gateway. No provider key is forwarded — the sandbox cannot
// reach any LLM but the locked gateway (the egress allowlist admits only the
// gateway upstream), so model choice is not an attack surface and the median of
// the k=3 validators' scores is comparable. The locked provider/model/gateway
// are applied AFTER the caller-supplied env and the caller's attempts to set any
// lockedEnvKey are dropped, so req.Env can never override the lock.
//
// The provider is frozen (lockedProvider): the crate always reaches the locked
// model through the OpenAI-compatible "chutes" path, so the fleet serves one
// backend and scores stay comparable. Only the gateway URLs are env-configurable
// (HARNESS_GATEWAY_URL for chat, HARNESS_EMBED_URL for embeddings), pointing at
// whatever serves the locked model on the host: cmd/model-relay fronting Chutes
// for a GPU-less validator, or a local OpenAI-compatible server. The model id is
// frozen too (llm.HarnessModel).
func harnessSandboxEnv(reqEnv map[string]string) map[string]string {
	gateway := envOr("HARNESS_GATEWAY_URL", "http://host.docker.internal:11434")
	env := map[string]string{}
	for k, v := range sandboxRuntimeEnv(reqEnv) {
		if lockedEnvKeys[k] {
			continue // the lock owns these; callers cannot set them
		}
		env[k] = v
	}
	// The lock, applied last so it wins over caller env. Chat routes to the
	// gateway through the crate's chutes provider (CHUTES_BASE_URL); embeddings
	// hit the local Ollama (OLLAMA_BASE_URL), which is the same host by default.
	// The real upstream key lives only in the relay, so the sandbox-side key is a
	// placeholder.
	env["DITTOBENCH_PROVIDER"] = lockedProvider
	env["DITTOBENCH_MODEL"] = llm.HarnessModel()
	env["OLLAMA_BASE_URL"] = envOr("HARNESS_EMBED_URL", gateway)
	env["CHUTES_BASE_URL"] = gateway
	env["CHUTES_API_KEY"] = "relay"
	// The production sandbox has a read-only root and exposes exactly one
	// bounded writable filesystem at /tmp. Force the standard harness database
	// there so an image cannot pass screening as root and then fail to boot as
	// the validator's unprivileged UID.
	env["DITTOBENCH_DB"] = "/tmp/dittobench.db"
	return env
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
