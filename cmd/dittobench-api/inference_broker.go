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
	"fmt"
	"io"
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
)

type brokerTicketIdentity struct {
	GrantID        string
	AgentID        string
	SlotID         string
	TicketDeadline time.Time
}

type brokerSession struct {
	mu               sync.Mutex
	id               string
	activationSecret string
	privateKey       ed25519.PrivateKey
	publicKey        ed25519.PublicKey
	grantID          string
	bearer           string
	proxyURL         string
	legacyGateway    string
	generation       int
	expiresAt        time.Time
	expectedSourceIP string
	provider         string
	model            string
	requestModel     string
	profileRevision  string
	preparedAt       time.Time
	ticketAgentID    string
	ticketSlotID     string
	ticketDeadline   time.Time
	boundRunID       string
	inFlight         int
	requests         uint64
	successes        uint64
	failures         uint64
	usageAvailable   uint64
	usageUnavailable uint64
	promptTokens     uint64
	promptBytes      uint64
	completionTokens uint64
	providerLatency  uint64
	callerCancels    uint64
	upstreamAttempts uint64
	cancels          map[string]context.CancelFunc
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
}

type toolRoute struct {
	expectedSourceIP string
	handler          http.Handler
	slots            chan struct{}
}

func newInferenceBroker(maxSessions int) *inferenceBroker {
	if maxSessions < 1 {
		maxSessions = 1
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
		maxSessions:  maxSessions * 2,
		controlToken: strings.TrimSpace(os.Getenv("DITTOBENCH_BROKER_CONTROL_TOKEN")),
		platformProxyURL: configuredPlatformProxyURL(
			os.Getenv("DITTOBENCH_PLATFORM_INFERENCE_PROXY_URL"),
		),
	}
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
	session.mu.Unlock()
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
	if sourceIP(r.RemoteAddr) != session.expectedSourceIP || !session.activeLocked(time.Now()) ||
		modelRequest.Model != session.requestModel {
		session.mu.Unlock()
		writeError(w, http.StatusUnauthorized, "inference session unavailable")
		return
	}
	grantID, bearer, proxyURL, generation := session.grantID, session.bearer, session.proxyURL, session.generation
	legacyGateway := session.legacyGateway
	privateKey := append(ed25519.PrivateKey(nil), session.privateKey...)
	session.requests++
	session.promptBytes += uint64(len(body))
	session.upstreamAttempts++
	session.mu.Unlock()

	nonce := uuid.NewString()
	requestCtx, cancel := context.WithCancel(r.Context())
	session.mu.Lock()
	session.cancels[nonce] = cancel
	session.mu.Unlock()
	defer func() {
		cancel()
		session.mu.Lock()
		delete(session.cancels, nonce)
		session.mu.Unlock()
	}()
	requested := time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	if legacyGateway != "" {
		var routeErr error
		proxyURL, routeErr = relayURL(legacyGateway, "/v1/chat/completions")
		if routeErr != nil {
			writeError(w, http.StatusBadGateway, "inference provider unavailable")
			return
		}
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, proxyURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "inference provider unavailable")
		return
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
	started := time.Now()
	resp, err := b.client.Do(req)
	latency := uint64(time.Since(started).Milliseconds())
	if err != nil {
		if requestCtx.Err() != nil {
			session.mu.Lock()
			session.callerCancels++
			session.mu.Unlock()
			writeError(w, http.StatusConflict, "inference session unavailable")
			return
		}
		session.mu.Lock()
		session.failures++
		session.providerLatency += latency
		session.mu.Unlock()
		writeError(w, http.StatusBadGateway, "inference provider unavailable")
		return
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(responseBody) > 16<<20 {
		session.mu.Lock()
		session.failures++
		session.providerLatency += latency
		session.mu.Unlock()
		writeError(w, http.StatusBadGateway, "inference provider unavailable")
		return
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		session.mu.Lock()
		session.usageUnavailable++
		session.providerLatency += latency
		session.mu.Unlock()
		writeError(w, resp.StatusCode, "inference request denied")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		session.mu.Lock()
		session.failures++
		session.providerLatency += latency
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
	session.providerLatency += latency
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
		CallerCancellations: session.callerCancels, UpstreamAttempts: session.upstreamAttempts,
		Provider: session.provider, ProfileRevision: session.profileRevision,
		Model: session.model, UsageAvailable: session.usageAvailable,
		UsageUnavailable: session.usageUnavailable, PromptTokens: session.promptTokens,
		PromptBytes: session.promptBytes, CompletionTokens: session.completionTokens,
		ProviderLatencyMs: session.providerLatency, TTFTStatus: "not_streamed",
	}, nil
}
