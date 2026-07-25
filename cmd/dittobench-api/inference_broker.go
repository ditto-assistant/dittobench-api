package main

// The inference broker is the trusted boundary between an untrusted harness
// and the platform-owned OpenRouter proxy. Platform bearer and DPoP private-key
// material live only in this process's memory: neither is put in a child
// container environment, command line, image, log, or Docker-readable mount.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/google/uuid"
)

const (
	brokerBodyLimit            = 4 << 20
	brokerMaximumSessionTTL    = 2 * time.Hour
	brokerPerSourceConcurrency = 4
	brokerReadHeaderTimeout    = 5 * time.Second
	brokerReadTimeout          = 15 * time.Second
	brokerWriteTimeout         = 2 * time.Minute
	brokerIdleTimeout          = 30 * time.Second
	brokerMaximumHeaderBytes   = 32 << 10
	platformInferenceAPIPath   = "/api/v1/inference/chat/completions"
	platformEmbeddingAPIPath   = "/api/v1/inference/embeddings"
	embeddingAPIPath           = "/api/embed"
	embeddingModel             = "embeddinggemma"
	hostedEmbeddingModel       = "perplexity/pplx-embed-v1-0.6b"
	embeddingDimensions        = 768
	embeddingMaximumInputs     = 256
	embeddingBodyLimit         = 1 << 20
	embeddingResponseLimit     = 16 << 20
	embeddingSessionRequests   = 100000
	embeddingSessionInputs     = 1000000
	embeddingSessionInputBytes = 1 << 30
)

// v7TransientMaxAttempts bounds how many times ONE logical v7 inference request
// may be delivered to the platform proxy. It is a code constant, not an env
// knob: a validator that quietly retried more than its peers would consume more
// of the shared route than the fleet agreed to, and the value below is chosen
// against a hard accounting cost rather than to taste.
//
// Why so small. Each broker-level attempt is a NEW nonce and therefore a NEW
// platform reservation (ditto-platform ditto/db/queries/inference.py:460-474:
// `reserved_tokens=token_reservation`, `grant.request_count += 1`). A failed
// attempt is then conservatively charged its FULL reservation to the grant
// (`if not usage_available: prompt_tokens = request.reserved_tokens`,
// inference.py:543-547). So attempt N costs the grant one request and a whole
// reservation whether or not the provider did any work. That ledger is the
// grant's CAPACITY budget -- it is not the miner's scored usage, which this
// broker books separately and only from a successful attempt -- but it is still
// finite, and a large cap here could exhaust a grant mid-run and convert a
// survivable blip into the very failure the retry exists to prevent.
//
// 3 attempts is therefore the smallest cap that survives a single fault the
// platform's own 3-attempt provider loop could not absorb, and it bounds
// worst-case grant consumption at 3x rather than unbounded.
const v7TransientMaxAttempts = 3

type brokerTicketIdentity struct {
	GrantID        string
	AgentID        string
	SlotID         string
	TicketDeadline time.Time
}

type brokerSession struct {
	mu                    sync.Mutex
	id                    string
	activationSecret      string
	privateKey            ed25519.PrivateKey
	publicKey             ed25519.PublicKey
	grantID               string
	bearer                string
	proxyURL              string
	legacyGateway         string
	generation            int
	expiresAt             time.Time
	expectedSourceIP      string
	provider              string
	model                 string
	requestModel          string
	profileRevision       string
	preparedAt            time.Time
	ticketAgentID         string
	ticketSlotID          string
	ticketDeadline        time.Time
	boundRunID            string
	benchVersion          int
	inFlight              int
	embeddingPhaseStarted bool
	embeddingPhaseActive  bool
	embeddingInFlight     bool
	embeddingCancel       context.CancelFunc
	embeddingDone         chan struct{}
	embeddingRetries      uint64
	embeddingRequests     uint64
	embeddingInputs       uint64
	embeddingInputBytes   uint64
	requests              uint64
	successes             uint64
	failures              uint64
	grantDenials          uint64
	usageAvailable        uint64
	usageUnavailable      uint64
	promptTokens          uint64
	promptBytes           uint64
	completionTokens      uint64
	providerLatency       uint64
	callerCancels         uint64
	upstreamAttempts      uint64
	cancels               map[string]context.CancelFunc
}

func newInferenceBrokerHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: brokerReadHeaderTimeout,
		ReadTimeout:       brokerReadTimeout,
		WriteTimeout:      brokerWriteTimeout,
		IdleTimeout:       brokerIdleTimeout,
		MaxHeaderBytes:    brokerMaximumHeaderBytes,
	}
}

type inferenceBroker struct {
	mu               sync.RWMutex
	sessions         map[string]*brokerSession
	tools            map[string]toolRoute
	client           *http.Client
	maxSessions      int
	controlToken     string
	platformProxyURL string
	embeddingURL     string
	embeddingSlots   chan struct{}
	retry            brokerRetryConfig
	sleep            func(context.Context, time.Duration) error
}

type toolRoute struct {
	expectedSourceIP string
	handler          http.Handler
	slots            chan struct{}
}

// platformGrantDenied marks a platform inference response that declined to
// reserve capacity for this ticket's grant rather than reporting an upstream
// provider fault.
//
// The distinction is exact, not heuristic. The platform inference proxy answers
// 429 in one place only: when begin_inference_request() refuses the reservation
// (ditto-platform ditto/api_server/endpoints/inference.py `inference grant
// unavailable` / `embedding grant unavailable`, backed by
// ditto/db/queries/inference.py:329-335, which sets `grant.status = "revoked"`
// when the owning ticket is no longer ISSUED, its deadline was rewritten, or
// its deadline has passed). A genuine provider rate limit can never surface
// here as a 429: the platform retries 408/429/5xx upstream itself and converts
// every remaining provider rejection into a 502.
//
// So a 429 on the ticket-scoped path means the validator's LEASE went away --
// platform-side eviction, budget exhaustion, or per-ticket concurrency -- and
// counting it as an `infrastructure_failure` alongside real provider faults is
// what made a mid-run ticket force-expiry look like an upstream provider blip.
// Both still fail the run closed; only the accounting and the operator-visible
// reason change.
type platformGrantDenied struct {
	status int
}

// Error deliberately preserves the historical wording so the marker-based
// classification in trustedEmbeddingInfrastructureFailure keeps matching.
func (e platformGrantDenied) Error() string {
	return fmt.Sprintf("embedding platform returned %d", e.status)
}

// platformEmbeddingTransient marks the narrow class of v7 embedding faults that
// are worth delivering again: no response at all, or a platform 5xx, which is
// what the platform returns once its own bounded provider loop has given up.
// Everything else -- a denied grant, a wrong model, a malformed or
// wrong-dimension vector, an unusable session -- stays terminal, because a
// repeat delivery cannot change any of those and an integrity fault must keep
// failing closed.
type platformEmbeddingTransient struct {
	err error
}

func (e platformEmbeddingTransient) Error() string { return e.err.Error() }
func (e platformEmbeddingTransient) Unwrap() error { return e.err }

// platformDeniesGrant reports whether a ticket-scoped platform response is a
// grant denial. Legacy relay sessions are excluded: they talk to the frozen
// model-relay, which forwards a real provider 429 verbatim, so on that path a
// 429 IS an upstream fault and must keep its existing accounting.
func platformDeniesGrant(legacyGateway string, status int) bool {
	return legacyGateway == "" && status == http.StatusTooManyRequests
}

type brokerRetryConfig struct {
	maxAttempts int
	base        time.Duration
	cap         time.Duration
	factor      float64
}

func (c brokerRetryConfig) backoff(attempt int) time.Duration {
	delay := float64(c.base) * math.Pow(c.factor, float64(attempt-1))
	if maximum := float64(c.cap); c.cap > 0 && delay > maximum {
		delay = maximum
	}
	if delay <= 0 {
		return 0
	}
	return time.Duration(delay/2 + mathrand.Float64()*(delay/2))
}

func brokerSleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newInferenceBroker(maxSessions int, embeddingCapacity ...int) *inferenceBroker {
	if maxSessions < 1 {
		maxSessions = 1
	}
	capacity := 1
	if len(embeddingCapacity) > 0 && embeddingCapacity[0] > 0 {
		capacity = embeddingCapacity[0]
	}
	return &inferenceBroker{
		sessions: make(map[string]*brokerSession),
		tools:    make(map[string]toolRoute),
		client: &http.Client{
			Timeout: 100 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxSessions:    maxSessions * 2,
		embeddingSlots: make(chan struct{}, capacity),
		controlToken:   strings.TrimSpace(os.Getenv("DITTOBENCH_BROKER_CONTROL_TOKEN")),
		platformProxyURL: configuredPlatformProxyURL(
			os.Getenv("DITTOBENCH_PLATFORM_INFERENCE_PROXY_URL"),
		),
		embeddingURL: configuredEmbeddingURL(
			envOr("DITTOBENCH_EMBEDDING_UPSTREAM_URL", "http://host.docker.internal:11434/api/embed"),
		),
		retry: brokerRetryConfig{
			maxAttempts: envIntDefault("RELAY_RETRY_MAX_ATTEMPTS", 6),
			base:        time.Duration(envIntDefault("RELAY_RETRY_BASE_MS", 200)) * time.Millisecond,
			cap:         time.Duration(envIntDefault("RELAY_RETRY_CAP_MS", 2000)) * time.Millisecond,
			factor:      envFloatDefault("RELAY_RETRY_FACTOR", 2),
		},
		sleep: brokerSleep,
	}
}

// beginEmbeddingPhase opens the locked embedding operation only after the
// scorer has admitted this exact run into its bounded memory phase. A session
// receives one phase for its lifetime; ending it is final, so a late harness
// request cannot reopen validator-owned embedding capacity.
func (b *inferenceBroker) beginEmbeddingPhase(id, runID string) bool {
	b.mu.RLock()
	session := b.sessions[id]
	b.mu.RUnlock()
	if session == nil {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.boundRunID != runID || !session.activeLocked(time.Now()) ||
		session.embeddingPhaseStarted {
		return false
	}
	session.embeddingPhaseStarted = true
	session.embeddingPhaseActive = true
	return true
}

func (b *inferenceBroker) endEmbeddingPhase(id, runID string) {
	b.mu.RLock()
	session := b.sessions[id]
	b.mu.RUnlock()
	if session == nil {
		return
	}
	session.mu.Lock()
	var cancel context.CancelFunc
	var done chan struct{}
	if session.boundRunID == runID {
		session.embeddingPhaseActive = false
		cancel = session.embeddingCancel
		done = session.embeddingDone
	}
	session.mu.Unlock()
	// A hostile harness may return from its scored request while leaving a
	// background embedding call open. Revoke that exact call and wait for its
	// cleanup to release any historical local-embedding slot before the scorer
	// releases memory-phase admission to a sibling.
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func configuredEmbeddingURL(raw string) string {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || value == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.Path != embeddingAPIPath {
		return ""
	}
	return parsed.String()
}

func configuredPlatformProxyURL(raw string) string {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || value == "" || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Path != platformInferenceAPIPath {
		return ""
	}
	return value
}

func (b *inferenceBroker) controlAuthorized(r *http.Request) bool {
	ip := net.ParseIP(sourceIP(r.RemoteAddr))
	if ip != nil && ip.IsLoopback() {
		return true
	}
	if b.controlToken == "" {
		return false
	}
	provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(provided), []byte(b.controlToken)) == 1
}

func (b *inferenceBroker) requireControl(w http.ResponseWriter, r *http.Request) bool {
	if b.controlAuthorized(r) {
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	writeError(w, http.StatusUnauthorized, "inference control plane unavailable")
	return false
}

// prepareLegacy creates a memory-only, run-bound session in front of a
// reviewed validator-owned compatibility relay. It gives concurrent v2-v6 sandboxes
// independent trusted accounting without putting the provider credential or a
// bearer in the harness. V7 is rejected here and requires a platform grant.
func (b *inferenceBroker) prepareLegacy(
	runID string,
	benchVersion int,
	gateway string,
	relay relayHealthSnapshot,
) (string, error) {
	b.pruneExpired(time.Now())
	if _, err := uuid.Parse(runID); err != nil || benchVersion < 2 || benchVersion > 6 {
		return "", fmt.Errorf("invalid legacy inference run")
	}
	if _, err := relayURL(gateway, "/v1/chat/completions"); err != nil {
		return "", err
	}
	if err := validLegacyRelayIdentity(relay); err != nil {
		return "", err
	}
	id, err := randomToken(18)
	if err != nil {
		return "", err
	}
	session := &brokerSession{
		id:              id,
		legacyGateway:   gateway,
		provider:        relay.Provider,
		profileRevision: relay.ProfileRevision,
		model:           relay.Model,
		requestModel:    llm.HarnessModelForVersion(benchVersion),
		expiresAt:       time.Now().Add(brokerMaximumSessionTTL),
		preparedAt:      time.Now(),
		boundRunID:      runID,
		cancels:         make(map[string]context.CancelFunc),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sessions) >= b.maxSessions {
		return "", fmt.Errorf("inference broker is at capacity")
	}
	b.sessions[id] = session
	return id, nil
}

func validLegacyRelayIdentity(relay relayHealthSnapshot) error {
	valid := (relay.Provider == "chutes" &&
		relay.ProfileRevision == llm.ChutesRelayProfileRevision &&
		relay.Model == llm.LockedUpstreamModel) ||
		(relay.Provider == "openrouter" &&
			relay.ProfileRevision == llm.OpenRouterRelayProfileRevision &&
			relay.Model == llm.LockedHarnessModel)
	if !valid {
		return fmt.Errorf("legacy relay identity is not a reviewed profile")
	}
	return nil
}

func (b *inferenceBroker) registerTool(h http.Handler, expectedSourceIP string) (string, func(), error) {
	id, err := randomToken(18)
	if err != nil {
		return "", func() {}, err
	}
	b.mu.Lock()
	b.tools[id] = toolRoute{
		expectedSourceIP: expectedSourceIP,
		handler:          h,
		slots:            make(chan struct{}, brokerPerSourceConcurrency),
	}
	b.mu.Unlock()
	stop := func() {
		b.mu.Lock()
		delete(b.tools, id)
		b.mu.Unlock()
	}
	return id, stop, nil
}

func (b *inferenceBroker) handleTool(w http.ResponseWriter, r *http.Request) {
	b.mu.RLock()
	route, ok := b.tools[r.PathValue("id")]
	b.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "tool route not found")
		return
	}
	if route.expectedSourceIP == "" || sourceIP(r.RemoteAddr) != route.expectedSourceIP {
		writeError(w, http.StatusUnauthorized, "tool route unavailable")
		return
	}
	select {
	case route.slots <- struct{}{}:
		defer func() { <-route.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "tool source is at capacity")
		return
	}
	forwarded := r.Clone(r.Context())
	forwarded.URL.Path = "/tool"
	route.handler.ServeHTTP(w, forwarded)
}

func randomToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (b *inferenceBroker) prepare(w http.ResponseWriter, r *http.Request) {
	if !b.requireControl(w, r) {
		return
	}
	b.pruneExpired(time.Now())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "inference broker unavailable")
		return
	}
	id, err := randomToken(18)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "inference broker unavailable")
		return
	}
	activation, err := randomToken(24)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "inference broker unavailable")
		return
	}
	session := &brokerSession{
		id: id, activationSecret: activation, privateKey: private, publicKey: public,
		preparedAt: time.Now(), cancels: make(map[string]context.CancelFunc),
	}
	b.mu.Lock()
	if len(b.sessions) >= b.maxSessions {
		b.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "inference broker is at capacity")
		return
	}
	b.sessions[id] = session
	b.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"session_id":        id,
		"activation_secret": activation,
		"broker_public_key": base64.RawURLEncoding.EncodeToString(public),
	})
}

type brokerActivation struct {
	ActivationSecret string    `json:"activation_secret"`
	GrantID          string    `json:"grant_id"`
	AgentID          string    `json:"agent_id,omitempty"`
	SlotID           string    `json:"slot_id,omitempty"`
	TicketDeadline   time.Time `json:"ticket_deadline,omitempty"`
	Bearer           string    `json:"bearer"`
	ProxyURL         string    `json:"proxy_url"`
	Generation       int       `json:"generation"`
	ExpiresAt        time.Time `json:"expires_at"`
	Provider         string    `json:"provider"`
	ProfileRevision  string    `json:"profile_revision"`
	Model            string    `json:"model"`
}

func (b *inferenceBroker) activate(w http.ResponseWriter, r *http.Request) {
	if !b.requireControl(w, r) {
		return
	}
	id := r.PathValue("id")
	b.mu.RLock()
	session := b.sessions[id]
	b.mu.RUnlock()
	if session == nil {
		writeError(w, http.StatusNotFound, "inference session not found")
		return
	}
	var activation brokerActivation
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&activation); err != nil {
		writeError(w, http.StatusBadRequest, "invalid inference activation")
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	now := time.Now()
	_, grantErr := uuid.Parse(activation.GrantID)
	ticketIdentity := brokerTicketIdentity{
		GrantID: activation.GrantID, AgentID: activation.AgentID,
		SlotID: activation.SlotID, TicketDeadline: activation.TicketDeadline,
	}
	hasRouteIdentity := activation.Provider != "" || activation.ProfileRevision != "" || activation.Model != ""
	secretMatches := subtle.ConstantTimeCompare(
		[]byte(activation.ActivationSecret), []byte(session.activationSecret),
	) == 1
	if !secretMatches || activation.Bearer == "" ||
		len(activation.Bearer) > 4096 || grantErr != nil || activation.Generation < 1 ||
		!activation.ExpiresAt.After(now) || activation.ExpiresAt.After(now.Add(brokerMaximumSessionTTL)) ||
		b.platformProxyURL == "" || activation.ProxyURL != b.platformProxyURL ||
		(hasRouteIdentity && (!validBrokerTicketIdentity(ticketIdentity, now) ||
			activation.ExpiresAt.After(activation.TicketDeadline) || activation.Provider == "" ||
			activation.ProfileRevision == "" || activation.Model == "")) {
		writeError(w, http.StatusUnauthorized, "invalid inference activation")
		return
	}
	session.activationSecret = ""
	session.grantID = activation.GrantID
	session.bearer = activation.Bearer
	session.proxyURL = activation.ProxyURL
	session.generation = activation.Generation
	session.expiresAt = activation.ExpiresAt
	session.provider = activation.Provider
	session.profileRevision = activation.ProfileRevision
	session.model = activation.Model
	session.requestModel = activation.Model
	session.ticketAgentID = activation.AgentID
	session.ticketSlotID = activation.SlotID
	session.ticketDeadline = activation.TicketDeadline
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"active": true})
}

func validBrokerTicketIdentity(identity brokerTicketIdentity, now time.Time) bool {
	_, grantErr := uuid.Parse(identity.GrantID)
	_, agentErr := uuid.Parse(identity.AgentID)
	validSlot := len(identity.SlotID) == len("slot-0") && strings.HasPrefix(identity.SlotID, "slot-") &&
		identity.SlotID[len(identity.SlotID)-1] >= '0' && identity.SlotID[len(identity.SlotID)-1] <= '7'
	return grantErr == nil && agentErr == nil && validSlot && identity.TicketDeadline.After(now)
}

func (b *inferenceBroker) claimRun(id, runID string, identity brokerTicketIdentity, benchVersion int) bool {
	b.mu.RLock()
	session := b.sessions[id]
	b.mu.RUnlock()
	if session == nil {
		return false
	}
	if _, err := uuid.Parse(runID); err != nil {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	now := time.Now()
	if session.boundRunID != "" || session.bearer == "" || !session.expiresAt.After(now) {
		return false
	}
	if benchVersion < 7 {
		// Bounded transition compatibility: old platform/subnet clients do not
		// carry route identity. Historical OpenRouter scoring keeps its original
		// aggregate provider/profile key so the reviewed v5/v6 baseline remains
		// valid even after this broker learns the v7 route fields.
		session.provider = "openrouter"
		session.profileRevision = llm.OpenRouterRelayProfileRevision
		session.model = llm.LockedHarnessModel
	}
	if benchVersion >= 7 {
		expected := brokerTicketIdentity{
			GrantID: session.grantID, AgentID: session.ticketAgentID,
			SlotID: session.ticketSlotID, TicketDeadline: session.ticketDeadline,
		}
		if !validV7RouteProfile(session.profileRevision) || !validBrokerTicketIdentity(identity, now) ||
			identity.GrantID != expected.GrantID || identity.AgentID != expected.AgentID ||
			identity.SlotID != expected.SlotID || !identity.TicketDeadline.Equal(expected.TicketDeadline) {
			return false
		}
	}
	if session.model != llm.HarnessModelForVersion(benchVersion) || session.provider == "" || session.profileRevision == "" {
		return false
	}
	session.requestModel = session.model
	session.boundRunID = runID
	session.benchVersion = benchVersion
	return true
}

func (b *inferenceBroker) bindSource(id, runID, sourceIP string) bool {
	b.mu.RLock()
	session := b.sessions[id]
	b.mu.RUnlock()
	if session == nil || net.ParseIP(sourceIP) == nil {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.boundRunID != runID || session.expectedSourceIP != "" ||
		(session.bearer == "" && session.legacyGateway == "") || !session.expiresAt.After(time.Now()) {
		return false
	}
	session.expectedSourceIP = sourceIP
	return true
}

func (session *brokerSession) activeLocked(now time.Time) bool {
	return session.boundRunID != "" && session.expectedSourceIP != "" &&
		session.expiresAt.After(now) && (session.bearer != "" || session.legacyGateway != "")
}

func destroyBrokerSession(session *brokerSession) {
	if session == nil {
		return
	}
	session.mu.Lock()
	embeddingCancel := session.embeddingCancel
	embeddingDone := session.embeddingDone
	for _, cancel := range session.cancels {
		cancel()
	}
	clear(session.cancels)
	for i := range session.privateKey {
		session.privateKey[i] = 0
	}
	session.activationSecret = ""
	session.bearer = ""
	session.legacyGateway = ""
	session.requestModel = ""
	session.embeddingPhaseActive = false
	session.mu.Unlock()
	if embeddingCancel != nil {
		embeddingCancel()
	}
	if embeddingDone != nil {
		<-embeddingDone
	}
}

func (b *inferenceBroker) remove(id string) {
	b.mu.Lock()
	session := b.sessions[id]
	delete(b.sessions, id)
	b.mu.Unlock()
	destroyBrokerSession(session)
}

func (b *inferenceBroker) removeRun(id, runID string) bool {
	b.mu.Lock()
	session := b.sessions[id]
	if session == nil {
		b.mu.Unlock()
		return false
	}
	session.mu.Lock()
	owned := session.boundRunID != "" && session.boundRunID == runID
	session.mu.Unlock()
	if !owned {
		b.mu.Unlock()
		return false
	}
	delete(b.sessions, id)
	b.mu.Unlock()
	destroyBrokerSession(session)
	return true
}

func (b *inferenceBroker) pruneExpired(now time.Time) {
	b.mu.RLock()
	ids := make([]string, 0)
	for id, session := range b.sessions {
		session.mu.Lock()
		expired := (!session.expiresAt.IsZero() && !session.expiresAt.After(now)) ||
			(session.expiresAt.IsZero() && now.Sub(session.preparedAt) > 2*time.Minute)
		session.mu.Unlock()
		if expired {
			ids = append(ids, id)
		}
	}
	b.mu.RUnlock()
	for _, id := range ids {
		b.remove(id)
	}
}

func (b *inferenceBroker) cancel(w http.ResponseWriter, r *http.Request) {
	if !b.requireControl(w, r) {
		return
	}
	b.remove(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (b *inferenceBroker) handle(w http.ResponseWriter, r *http.Request) {
	b.pruneExpired(time.Now())
	session := b.sessionForSource(sourceIP(r.RemoteAddr))
	if session == nil {
		writeError(w, http.StatusUnauthorized, "inference session unavailable")
		return
	}
	rest := "/" + strings.TrimLeft(r.PathValue("rest"), "/")
	if rest == "/health" && r.Method == http.MethodGet {
		b.health(w, session)
		return
	}
	// The frozen starter-kit Chutes adapter appends /chat/completions to its
	// configured base URL, while newer OpenAI-compatible clients append
	// /v1/chat/completions. Accept both at this trusted, source-bound boundary;
	// both routes are forwarded to the one locked upstream relay endpoint.
	if (rest != "/chat/completions" && rest != "/v1/chat/completions") || r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "inference route not found")
		return
	}
	session.mu.Lock()
	if session.inFlight >= brokerPerSourceConcurrency {
		session.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "inference source is at capacity")
		return
	}
	session.inFlight++
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.inFlight--
		session.mu.Unlock()
	}()
	b.proxy(w, r, session)
}

type embeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingResponse struct {
	Embeddings      [][]float64 `json:"embeddings"`
	PromptEvalCount int         `json:"prompt_eval_count,omitempty"`
}

// handleEmbedding exposes only the deterministic embedding operation to the
// source-bound harness. Ollama's model-management, generation, discovery, and
// administrative APIs remain unreachable from the sandbox.
func (b *inferenceBroker) handleEmbedding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != embeddingAPIPath {
		writeError(w, http.StatusNotFound, "embedding route not found")
		return
	}
	b.pruneExpired(time.Now())
	session := b.sessionForSource(sourceIP(r.RemoteAddr))
	if session == nil {
		writeError(w, http.StatusUnauthorized, "embedding session unavailable")
		return
	}
	session.mu.Lock()
	benchVersion := session.benchVersion
	session.mu.Unlock()
	if benchVersion < 7 && b.embeddingURL == "" {
		writeError(w, http.StatusServiceUnavailable, "embedding service unavailable")
		return
	}
	requestContext, cancelRequest := context.WithTimeout(r.Context(), 65*time.Second)
	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			cancelRequest()
			_ = r.Body.Close()
		})
	}
	session.mu.Lock()
	if !session.embeddingPhaseActive {
		session.mu.Unlock()
		cancel()
		writeError(w, http.StatusConflict, "embedding phase unavailable")
		return
	}
	if session.embeddingInFlight {
		session.mu.Unlock()
		cancel()
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "embedding source is at capacity")
		return
	}
	done := make(chan struct{})
	session.embeddingInFlight = true
	session.embeddingCancel = cancel
	session.embeddingDone = done
	session.mu.Unlock()
	slotAcquired := false
	defer func() {
		if slotAcquired {
			<-b.embeddingSlots
		}
		session.mu.Lock()
		if session.embeddingDone == done {
			session.embeddingInFlight = false
			session.embeddingCancel = nil
			session.embeddingDone = nil
		}
		session.mu.Unlock()
		cancel()
		close(done)
	}()

	body, err := io.ReadAll(io.LimitReader(r.Body, embeddingBodyLimit+1))
	if err != nil || len(body) > embeddingBodyLimit {
		writeError(w, http.StatusRequestEntityTooLarge, "embedding request too large")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload embeddingRequest
	if decoder.Decode(&payload) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		payload.Model != embeddingModel || len(payload.Input) == 0 ||
		len(payload.Input) > embeddingMaximumInputs {
		writeError(w, http.StatusBadRequest, "invalid embedding request")
		return
	}
	for _, input := range payload.Input {
		if input == "" || len(input) > embeddingBodyLimit {
			writeError(w, http.StatusBadRequest, "invalid embedding request")
			return
		}
	}
	inputBytes := 0
	for _, input := range payload.Input {
		inputBytes += len(input)
	}
	session.mu.Lock()
	if !session.embeddingPhaseActive {
		session.mu.Unlock()
		writeError(w, http.StatusConflict, "embedding phase unavailable")
		return
	}
	if session.embeddingRequests+1 > embeddingSessionRequests ||
		session.embeddingInputs+uint64(len(payload.Input)) > embeddingSessionInputs ||
		session.embeddingInputBytes+uint64(inputBytes) > embeddingSessionInputBytes {
		session.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "embedding session budget exhausted")
		return
	}
	session.embeddingRequests++
	session.embeddingInputs += uint64(len(payload.Input))
	session.embeddingInputBytes += uint64(inputBytes)
	session.mu.Unlock()

	// v2-v6 retain the frozen global Ollama lane. Hosted v7 requests are already
	// isolated and serialized per ticket above, so unrelated evaluations must
	// not queue behind an obsolete host-global embedding bottleneck.
	if benchVersion < 7 {
		select {
		case b.embeddingSlots <- struct{}{}:
			slotAcquired = true
		default:
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "embedding service is at capacity")
			return
		}
	}

	var decoded embeddingResponse
	if benchVersion >= 7 {
		decoded, err = b.forwardPlatformEmbeddingWithRetry(requestContext, session, payload.Input)
	} else {
		decoded, err = b.forwardLocalEmbedding(requestContext, payload.Input)
	}
	if err != nil {
		if benchVersion >= 7 {
			// #97 made a v7 embedding fault fail the run closed, and it still
			// does. What changes is only WHICH counter it lands in: a platform
			// grant denial is recorded as a lost lease rather than as an
			// upstream provider failure. Embeddings are roughly two thirds of a
			// v7 run's inference requests, so an evicted ticket is most likely
			// to be discovered here first -- which is exactly how a platform
			// eviction came to be reported as "1 upstream failure".
			var denied platformGrantDenied
			session.mu.Lock()
			if errors.As(err, &denied) {
				session.grantDenials++
				log.Printf(
					"run %s: platform declined the embedding grant (429); ticket deadline held locally is %s (in %s) -- this is a lease denial, not a provider fault (denial #%d)",
					session.boundRunID, session.ticketDeadline.UTC().Format(time.RFC3339),
					time.Until(session.ticketDeadline).Truncate(time.Second), session.grantDenials,
				)
			} else {
				session.failures++
			}
			session.mu.Unlock()
		}
		writeError(w, http.StatusBadGateway, "embedding service unavailable")
		return
	}
	for _, vector := range decoded.Embeddings {
		if len(vector) != embeddingDimensions {
			writeError(w, http.StatusBadGateway, "invalid embedding response")
			return
		}
		for _, value := range vector {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				writeError(w, http.StatusBadGateway, "invalid embedding response")
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, decoded)
}

// forwardPlatformEmbeddingWithRetry gives the v7 embedding lane the same tiny
// second line of defence the chat lane has. Embeddings are roughly two thirds
// of a v7 run's ~1,067 inference requests and had NO retry at all, so they were
// the most likely place for a single transient fault to discard a whole run.
//
// The retryable class is exactly the transient one: a transport failure, or a
// platform 5xx (which is what the platform returns after its own 3-attempt
// provider loop gives up). A grant denial is auth class and returns
// immediately -- retrying a revoked lease cannot succeed and would burn a fresh
// reservation each time. Every extra delivery is recorded in the session's
// retry ledger so a run that survived a fault stays distinguishable from one
// that never faulted.
func (b *inferenceBroker) forwardPlatformEmbeddingWithRetry(
	ctx context.Context, session *brokerSession, inputs []string,
) (embeddingResponse, error) {
	var lastErr error
	for attempt := 1; attempt <= v7TransientMaxAttempts; attempt++ {
		if attempt > 1 {
			if b.sleep(ctx, b.retry.backoff(attempt-1)) != nil {
				break
			}
			session.mu.Lock()
			session.embeddingRetries++
			retries := session.embeddingRetries
			runID := session.boundRunID
			session.mu.Unlock()
			log.Printf(
				"run %s: retrying v7 embedding attempt %d/%d after a transient platform fault (%v); run retry ledger=%d",
				runID, attempt, v7TransientMaxAttempts, lastErr, retries,
			)
		}
		decoded, err := b.forwardPlatformEmbedding(ctx, session, inputs)
		if err == nil {
			return decoded, nil
		}
		lastErr = err
		var transient platformEmbeddingTransient
		if !errors.As(err, &transient) || ctx.Err() != nil {
			return embeddingResponse{}, err
		}
	}
	return embeddingResponse{}, lastErr
}

func (b *inferenceBroker) forwardLocalEmbedding(ctx context.Context, inputs []string) (embeddingResponse, error) {
	lockedBody, err := json.Marshal(embeddingRequest{Model: embeddingModel, Input: inputs})
	if err != nil {
		return embeddingResponse{}, err
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, b.embeddingURL, bytes.NewReader(lockedBody))
	if err != nil {
		return embeddingResponse{}, err
	}
	upstream.Header.Set("Content-Type", "application/json")
	response, err := b.client.Do(upstream)
	if err != nil {
		return embeddingResponse{}, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, embeddingResponseLimit+1))
	if err != nil || len(responseBody) > embeddingResponseLimit || response.StatusCode < 200 || response.StatusCode >= 300 {
		return embeddingResponse{}, fmt.Errorf("embedding upstream returned %d", response.StatusCode)
	}
	var decoded embeddingResponse
	if json.Unmarshal(responseBody, &decoded) != nil || len(decoded.Embeddings) != len(inputs) || decoded.PromptEvalCount < 0 {
		return embeddingResponse{}, fmt.Errorf("invalid embedding response")
	}
	return decoded, nil
}

type platformEmbeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     int      `json:"dimensions"`
	EncodingFormat string   `json:"encoding_format"`
}

type platformEmbeddingResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func platformEmbeddingURL(chatProxyURL string) (string, error) {
	parsed, err := url.Parse(chatProxyURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Path != platformInferenceAPIPath {
		return "", fmt.Errorf("invalid platform inference route")
	}
	parsed.Path = platformEmbeddingAPIPath
	return parsed.String(), nil
}

func (b *inferenceBroker) forwardPlatformEmbedding(ctx context.Context, session *brokerSession, inputs []string) (embeddingResponse, error) {
	body, err := json.Marshal(platformEmbeddingRequest{
		Model: hostedEmbeddingModel, Input: inputs,
		Dimensions: embeddingDimensions, EncodingFormat: "float",
	})
	if err != nil {
		return embeddingResponse{}, err
	}
	session.mu.Lock()
	if session.benchVersion != 7 || !session.activeLocked(time.Now()) {
		session.mu.Unlock()
		return embeddingResponse{}, fmt.Errorf("embedding session unavailable")
	}
	grantID, bearer, proxyURL, generation := session.grantID, session.bearer, session.proxyURL, session.generation
	privateKey := append(ed25519.PrivateKey(nil), session.privateKey...)
	session.mu.Unlock()
	endpoint, err := platformEmbeddingURL(proxyURL)
	if err != nil {
		return embeddingResponse{}, err
	}
	nonce := uuid.NewString()
	requested := time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	digest := sha256.Sum256(body)
	message := fmt.Sprintf("ditto-inference:v1:%s:%d:%s:%s:%s", grantID, generation, nonce, requested, hex.EncodeToString(digest[:]))
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(message)))
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return embeddingResponse{}, err
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Authorization", "Bearer "+bearer)
	upstream.Header.Set("X-Ditto-Grant", grantID)
	upstream.Header.Set("X-Ditto-Generation", fmt.Sprint(generation))
	upstream.Header.Set("X-Ditto-Nonce", nonce)
	upstream.Header.Set("X-Ditto-Requested-At", requested)
	upstream.Header.Set("X-Ditto-Proof", proof)
	response, err := b.client.Do(upstream)
	if err != nil {
		// No response at all: transport/connection fault, the transient class.
		return embeddingResponse{}, platformEmbeddingTransient{err: err}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, embeddingResponseLimit+1))
	if err != nil || len(responseBody) > embeddingResponseLimit || response.StatusCode < 200 || response.StatusCode >= 300 {
		if err != nil || response.StatusCode >= 500 {
			// The platform returns 5xx after its own bounded provider loop has
			// already given up, and a truncated read produced no outcome
			// either. Both are transient and worth one more delivery.
			return embeddingResponse{}, platformEmbeddingTransient{
				err: fmt.Errorf("embedding platform returned %d", response.StatusCode),
			}
		}
		if response.StatusCode == http.StatusTooManyRequests {
			// Auth class, not a provider fault: the platform's embedding proxy
			// answers 429 only from begin_inference_request declining the
			// reservation. Typed so the caller neither retries it nor books it
			// as an upstream failure; the message is unchanged so the existing
			// marker-based classification still matches.
			return embeddingResponse{}, platformGrantDenied{status: response.StatusCode}
		}
		return embeddingResponse{}, fmt.Errorf("embedding platform returned %d", response.StatusCode)
	}
	var platformResponse platformEmbeddingResponse
	if json.Unmarshal(responseBody, &platformResponse) != nil || platformResponse.Model != hostedEmbeddingModel ||
		len(platformResponse.Data) != len(inputs) || platformResponse.Usage.PromptTokens < 0 ||
		platformResponse.Usage.TotalTokens != platformResponse.Usage.PromptTokens {
		return embeddingResponse{}, fmt.Errorf("invalid platform embedding response")
	}
	decoded := embeddingResponse{
		Embeddings: make([][]float64, len(inputs)), PromptEvalCount: platformResponse.Usage.PromptTokens,
	}
	for index, item := range platformResponse.Data {
		if item.Index != index || len(item.Embedding) != embeddingDimensions {
			return embeddingResponse{}, fmt.Errorf("invalid platform embedding response")
		}
		for _, value := range item.Embedding {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return embeddingResponse{}, fmt.Errorf("invalid platform embedding response")
			}
		}
		decoded.Embeddings[index] = item.Embedding
	}
	return decoded, nil
}

func (b *inferenceBroker) sessionForSource(ip string) *brokerSession {
	if net.ParseIP(ip) == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, session := range b.sessions {
		session.mu.Lock()
		matches := session.expectedSourceIP == ip && session.activeLocked(time.Now())
		session.mu.Unlock()
		if matches {
			return session
		}
	}
	return nil
}

func (b *inferenceBroker) health(w http.ResponseWriter, session *brokerSession) {
	session.mu.Lock()
	defer session.mu.Unlock()
	status := "ok"
	if !session.activeLocked(time.Now()) {
		status = "unavailable"
	}
	writeJSON(w, http.StatusOK, relayHealthSnapshot{
		AccountingVersion:      2,
		Status:                 status,
		Requests:               session.requests,
		Successes:              session.successes,
		InfrastructureFailures: session.failures,
		GrantDenials:           session.grantDenials,
		EmbeddingRetries:       session.embeddingRetries,
		CallerCancellations:    session.callerCancels,
		UpstreamAttempts:       session.upstreamAttempts,
		Provider:               session.provider,
		ProfileRevision:        session.profileRevision,
		Model:                  session.model,
		UsageAvailable:         session.usageAvailable,
		UsageUnavailable:       session.usageUnavailable,
		PromptTokens:           session.promptTokens,
		PromptBytes:            session.promptBytes,
		CompletionTokens:       session.completionTokens,
		ProviderLatencyMs:      session.providerLatency,
		TTFTStatus:             "not_streamed",
	})
}

func sourceIP(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return ""
	}
	return host
}

func (b *inferenceBroker) proxy(w http.ResponseWriter, r *http.Request, session *brokerSession) {
	body, err := io.ReadAll(io.LimitReader(r.Body, brokerBodyLimit+1))
	if err != nil || len(body) > brokerBodyLimit {
		writeError(w, http.StatusRequestEntityTooLarge, "inference request too large")
		return
	}
	var modelRequest struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &modelRequest) != nil {
		writeError(w, http.StatusBadRequest, "invalid inference request")
		return
	}
	session.mu.Lock()
	if sourceIP(r.RemoteAddr) != session.expectedSourceIP || !session.activeLocked(time.Now()) {
		session.mu.Unlock()
		writeError(w, http.StatusUnauthorized, "inference session unavailable")
		return
	}
	// The model is a property of the ticket, not of the request. The harness
	// that produced this body is miner-authored, so its model field is at best
	// advisory: substitute the ticket's model rather than rejecting, so a
	// harness carrying a stale default (every pre-v7 fork of the starter kit
	// defaults to qwen/qwen3-32b) is scored on the locked model instead of
	// failing closed with an error it cannot act on. The platform proxy re-locks
	// the same value independently, so this is convenience, not the boundary.
	if modelRequest.Model != session.requestModel {
		rewritten, rewriteErr := rewriteRequestModel(body, session.requestModel)
		if rewriteErr != nil {
			session.mu.Unlock()
			writeError(w, http.StatusBadRequest, "invalid inference request")
			return
		}
		log.Printf("run %s: harness requested model %q; serving the ticket model %q",
			session.boundRunID, modelRequest.Model, session.requestModel)
		body = rewritten
	}
	grantID, bearer, proxyURL, generation := session.grantID, session.bearer, session.proxyURL, session.generation
	legacyGateway := session.legacyGateway
	privateKey := append(ed25519.PrivateKey(nil), session.privateKey...)
	session.requests++
	session.promptBytes += uint64(len(body))
	session.mu.Unlock()

	requestCtx, cancel := context.WithCancel(r.Context())
	cancelID := uuid.NewString()
	session.mu.Lock()
	session.cancels[cancelID] = cancel
	session.mu.Unlock()
	defer func() {
		cancel()
		session.mu.Lock()
		delete(session.cancels, cancelID)
		session.mu.Unlock()
	}()
	if legacyGateway != "" {
		var routeErr error
		proxyURL, routeErr = relayURL(legacyGateway, "/v1/chat/completions")
		if routeErr != nil {
			writeError(w, http.StatusBadGateway, "inference provider unavailable")
			return
		}
	}
	var responseBody []byte
	var responseStatus int
	var totalLatency uint64
	// The platform owns the FIRST line of provider retry for ticket-scoped v7
	// inference (_PROVIDER_MAX_ATTEMPTS=3 over 408/429/5xx, all under one
	// reservation), so #97 collapsed this loop to a single attempt rather than
	// multiply provider work. That is still the right default, but one attempt
	// means a fault the platform could not absorb discards ~18 minutes of work,
	// so v7 keeps a deliberately tiny second line of defence. See
	// v7TransientMaxAttempts for why the cap is 3 and not larger.
	maxAttempts := v7TransientMaxAttempts
	if legacyGateway != "" {
		maxAttempts = b.retry.maxAttempts
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 && b.sleep(requestCtx, b.retry.backoff(attempt-1)) != nil {
			break
		}
		nonce := uuid.NewString()
		requested := time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
		req, buildErr := http.NewRequestWithContext(requestCtx, http.MethodPost, proxyURL, bytes.NewReader(body))
		if buildErr != nil {
			break
		}
		req.Header.Set("Content-Type", "application/json")
		if legacyGateway == "" {
			digest := sha256.Sum256(body)
			message := fmt.Sprintf("ditto-inference:v1:%s:%d:%s:%s:%s", grantID, generation, nonce, requested, hex.EncodeToString(digest[:]))
			proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(message)))
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("X-Ditto-Grant", grantID)
			req.Header.Set("X-Ditto-Generation", fmt.Sprint(generation))
			req.Header.Set("X-Ditto-Nonce", nonce)
			req.Header.Set("X-Ditto-Requested-At", requested)
			req.Header.Set("X-Ditto-Proof", proof)
		}
		session.mu.Lock()
		session.upstreamAttempts++
		session.mu.Unlock()
		started := time.Now()
		resp, requestErr := b.client.Do(req)
		totalLatency += uint64(time.Since(started).Milliseconds())
		if requestErr != nil {
			if requestCtx.Err() != nil {
				break
			}
			continue
		}
		candidateBody, readErr := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
		_ = resp.Body.Close()
		responseStatus = resp.StatusCode
		if readErr != nil || len(candidateBody) > 16<<20 {
			continue
		}
		responseBody = candidateBody
		// Auth class: the platform declined the grant. The lease is gone (or its
		// budget is), so every further attempt would fail identically while
		// still consuming a fresh reservation and one more request from a grant
		// that no longer exists. Stop immediately.
		if platformDeniesGrant(legacyGateway, responseStatus) {
			break
		}
		if responseStatus == http.StatusRequestTimeout || responseStatus == http.StatusTooManyRequests || responseStatus >= 500 {
			continue
		}
		break
	}
	if requestCtx.Err() != nil {
		session.mu.Lock()
		session.callerCancels++
		session.providerLatency += totalLatency
		session.mu.Unlock()
		writeError(w, http.StatusConflict, "inference session unavailable")
		return
	}
	if responseStatus >= 400 && responseStatus < 500 && responseStatus != http.StatusTooManyRequests {
		session.mu.Lock()
		session.usageUnavailable++
		session.providerLatency += totalLatency
		session.mu.Unlock()
		writeError(w, responseStatus, "inference request denied")
		return
	}
	// A platform grant denial is a lost lease, not a provider fault. It is
	// counted separately so the run's failure names the real cause; the harness
	// still sees the byte-identical 502 it saw before, because this run is going
	// to be discarded either way and its remaining requests must not observe a
	// changed gateway contract mid-benchmark.
	if platformDeniesGrant(legacyGateway, responseStatus) {
		session.mu.Lock()
		session.grantDenials++
		session.providerLatency += totalLatency
		denials := session.grantDenials
		runID, deadline := session.boundRunID, session.ticketDeadline
		session.mu.Unlock()
		log.Printf(
			"run %s: platform declined the inference grant (429); ticket deadline held locally is %s (in %s) -- this is a lease denial, not a provider fault (denial #%d)",
			runID, deadline.UTC().Format(time.RFC3339), time.Until(deadline).Truncate(time.Second), denials,
		)
		writeError(w, http.StatusBadGateway, "inference provider unavailable")
		return
	}
	if responseStatus < 200 || responseStatus >= 300 || len(responseBody) == 0 {
		session.mu.Lock()
		session.failures++
		session.providerLatency += totalLatency
		session.mu.Unlock()
		writeError(w, http.StatusBadGateway, "inference provider unavailable")
		return
	}
	var decoded struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	usageOK := json.Unmarshal(responseBody, &decoded) == nil && decoded.Usage != nil && decoded.Usage.PromptTokens >= 0 && decoded.Usage.CompletionTokens >= 0
	session.mu.Lock()
	session.successes++
	session.providerLatency += totalLatency
	if usageOK {
		session.usageAvailable++
		session.promptTokens += uint64(decoded.Usage.PromptTokens)
		session.completionTokens += uint64(decoded.Usage.CompletionTokens)
	} else {
		session.usageUnavailable++
	}
	session.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBody)
}

func (b *inferenceBroker) trustedProbe(ctx context.Context, id string) error {
	b.mu.RLock()
	session := b.sessions[id]
	b.mu.RUnlock()
	if session == nil {
		return fmt.Errorf("inference session unavailable")
	}
	body, _ := json.Marshal(map[string]any{
		"model":      session.requestModel,
		"messages":   []map[string]string{{"role": "user", "content": "Reply OK."}},
		"max_tokens": 1, "temperature": 0, "stream": false,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	session.mu.Lock()
	req.RemoteAddr = net.JoinHostPort(session.expectedSourceIP, "1")
	session.mu.Unlock()
	recorder := httptest.NewRecorder()
	b.proxy(recorder, req, session)
	if recorder.Code < 200 || recorder.Code >= 300 {
		return fmt.Errorf("ticket inference probe returned %d", recorder.Code)
	}
	var decoded struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(recorder.Body.Bytes(), &decoded) != nil || len(decoded.Choices) == 0 {
		return fmt.Errorf("ticket inference probe returned no completion")
	}
	session.mu.Lock()
	benchVersion := session.benchVersion
	session.mu.Unlock()
	if benchVersion == 7 {
		embedding, err := b.forwardPlatformEmbedding(ctx, session, []string{"validator embedding preflight"})
		if err != nil {
			return fmt.Errorf("ticket embedding probe failed: %w", err)
		}
		if len(embedding.Embeddings) != 1 || len(embedding.Embeddings[0]) != embeddingDimensions {
			return fmt.Errorf("ticket embedding probe returned no vector")
		}
	}
	return nil
}

func (b *inferenceBroker) snapshot(id string) (relayHealthSnapshot, error) {
	b.mu.RLock()
	session := b.sessions[id]
	b.mu.RUnlock()
	if session == nil {
		return relayHealthSnapshot{}, fmt.Errorf("inference session unavailable")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return relayHealthSnapshot{
		AccountingVersion: 2, Status: "ok", Requests: session.requests,
		Successes: session.successes, InfrastructureFailures: session.failures,
		GrantDenials: session.grantDenials, EmbeddingRetries: session.embeddingRetries,
		CallerCancellations: session.callerCancels, UpstreamAttempts: session.upstreamAttempts,
		Provider: session.provider, ProfileRevision: session.profileRevision,
		Model: session.model, UsageAvailable: session.usageAvailable,
		UsageUnavailable: session.usageUnavailable, PromptTokens: session.promptTokens,
		PromptBytes: session.promptBytes, CompletionTokens: session.completionTokens,
		ProviderLatencyMs: session.providerLatency, TTFTStatus: "not_streamed",
	}, nil
}

// rewriteRequestModel replaces the caller's `model` with the ticket's, leaving
// every other field of the request untouched. Decoding into a generic map and
// re-encoding is deliberate: it normalises exactly one field and cannot smuggle
// an unmodelled field past the schema the platform proxy validates downstream.
func rewriteRequestModel(body []byte, model string) ([]byte, error) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil || decoded == nil {
		return nil, fmt.Errorf("inference request is not a JSON object")
	}
	decoded["model"] = model
	return json.Marshal(decoded)
}
