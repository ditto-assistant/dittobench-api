package main

// The inference broker is the trusted boundary between an untrusted harness
// and the platform-owned OpenRouter proxy. Platform bearer and DPoP private-key
// material live only in this process's memory: neither is put in a child
// container environment, command line, image, log, or Docker-readable mount.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/google/uuid"
)

const brokerBodyLimit = 4 << 20

type brokerSession struct {
	mu               sync.Mutex
	id               string
	activationSecret string
	privateKey       ed25519.PrivateKey
	publicKey        ed25519.PublicKey
	grantID          string
	bearer           string
	proxyURL         string
	generation       int
	expiresAt        time.Time
	expectedSourceIP string
	requests         uint64
	successes        uint64
	failures         uint64
	usageAvailable   uint64
	usageUnavailable uint64
	promptTokens     uint64
	promptBytes      uint64
	completionTokens uint64
	providerLatency  uint64
}

type inferenceBroker struct {
	mu       sync.RWMutex
	sessions map[string]*brokerSession
	tools    map[string]toolRoute
	client   *http.Client
}

type toolRoute struct {
	expectedSourceIP string
	handler          http.Handler
}

func newInferenceBroker() *inferenceBroker {
	return &inferenceBroker{
		sessions: make(map[string]*brokerSession),
		tools:    make(map[string]toolRoute),
		client:   &http.Client{Timeout: 100 * time.Second},
	}
}

func (b *inferenceBroker) registerTool(h http.Handler, expectedSourceIP string) (string, func(), error) {
	id, err := randomToken(18)
	if err != nil {
		return "", func() {}, err
	}
	b.mu.Lock()
	b.tools[id] = toolRoute{expectedSourceIP: expectedSourceIP, handler: h}
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

func (b *inferenceBroker) prepare(w http.ResponseWriter, _ *http.Request) {
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
	}
	b.mu.Lock()
	b.sessions[id] = session
	b.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"session_id":        id,
		"activation_secret": activation,
		"broker_public_key": base64.RawURLEncoding.EncodeToString(public),
	})
}

type brokerActivation struct {
	ActivationSecret string    `json:"activation_secret"`
	GrantID          string    `json:"grant_id"`
	Bearer           string    `json:"bearer"`
	ProxyURL         string    `json:"proxy_url"`
	Generation       int       `json:"generation"`
	ExpiresAt        time.Time `json:"expires_at"`
}

func (b *inferenceBroker) activate(w http.ResponseWriter, r *http.Request) {
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
	if activation.ActivationSecret != session.activationSecret || activation.Bearer == "" ||
		activation.GrantID == "" || activation.Generation < 1 ||
		activation.ExpiresAt.Before(time.Now()) || !strings.HasPrefix(activation.ProxyURL, "https://") {
		writeError(w, http.StatusUnauthorized, "invalid inference activation")
		return
	}
	session.activationSecret = ""
	session.grantID = activation.GrantID
	session.bearer = activation.Bearer
	session.proxyURL = activation.ProxyURL
	session.generation = activation.Generation
	session.expiresAt = activation.ExpiresAt
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"active": true})
}

func (b *inferenceBroker) bindSource(id, sourceIP string) bool {
	b.mu.RLock()
	session := b.sessions[id]
	b.mu.RUnlock()
	if session == nil || net.ParseIP(sourceIP) == nil {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	session.expectedSourceIP = sourceIP
	return session.bearer != "" && session.expiresAt.After(time.Now())
}

func (b *inferenceBroker) remove(id string) {
	b.mu.Lock()
	delete(b.sessions, id)
	b.mu.Unlock()
}

func (b *inferenceBroker) cancel(w http.ResponseWriter, r *http.Request) {
	b.remove(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (b *inferenceBroker) handle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rest := "/" + strings.TrimLeft(r.PathValue("rest"), "/")
	b.mu.RLock()
	session := b.sessions[id]
	b.mu.RUnlock()
	if session == nil {
		writeError(w, http.StatusNotFound, "inference session not found")
		return
	}
	if rest == "/health" && r.Method == http.MethodGet {
		b.health(w, session)
		return
	}
	if rest != "/v1/chat/completions" || r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "inference route not found")
		return
	}
	b.proxy(w, r, session)
}

func (b *inferenceBroker) health(w http.ResponseWriter, session *brokerSession) {
	session.mu.Lock()
	defer session.mu.Unlock()
	status := "ok"
	if session.bearer == "" || session.expectedSourceIP == "" || !session.expiresAt.After(time.Now()) {
		status = "unavailable"
	}
	writeJSON(w, http.StatusOK, relayHealthSnapshot{
		AccountingVersion:      2,
		Status:                 status,
		Requests:               session.requests,
		Successes:              session.successes,
		InfrastructureFailures: session.failures,
		Provider:               "openrouter",
		ProfileRevision:        llm.OpenRouterRelayProfileRevision,
		Model:                  llm.LockedHarnessModel,
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
	session.mu.Lock()
	if session.expectedSourceIP == "" || sourceIP(r.RemoteAddr) != session.expectedSourceIP ||
		session.bearer == "" || !session.expiresAt.After(time.Now()) {
		session.mu.Unlock()
		writeError(w, http.StatusUnauthorized, "inference session unavailable")
		return
	}
	grantID, bearer, proxyURL, generation := session.grantID, session.bearer, session.proxyURL, session.generation
	privateKey := append(ed25519.PrivateKey(nil), session.privateKey...)
	session.requests++
	session.promptBytes += uint64(len(body))
	session.mu.Unlock()

	nonce := uuid.NewString()
	requested := time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	digest := sha256.Sum256(body)
	message := fmt.Sprintf("ditto-inference:v1:%s:%d:%s:%s:%s", grantID, generation, nonce, requested, hex.EncodeToString(digest[:]))
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(message)))
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, proxyURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "inference provider unavailable")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-Ditto-Grant", grantID)
	req.Header.Set("X-Ditto-Generation", fmt.Sprint(generation))
	req.Header.Set("X-Ditto-Nonce", nonce)
	req.Header.Set("X-Ditto-Requested-At", requested)
	req.Header.Set("X-Ditto-Proof", proof)
	started := time.Now()
	resp, err := b.client.Do(req)
	latency := uint64(time.Since(started).Milliseconds())
	if err != nil {
		session.mu.Lock()
		session.failures++
		session.providerLatency += latency
		session.mu.Unlock()
		writeError(w, http.StatusBadGateway, "inference provider unavailable")
		return
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(responseBody) > 16<<20 || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		session.mu.Lock()
		session.failures++
		session.providerLatency += latency
		session.mu.Unlock()
		writeError(w, http.StatusBadGateway, "inference provider unavailable")
		return
	}
	var decoded struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	usageOK := json.Unmarshal(responseBody, &decoded) == nil && decoded.Usage.PromptTokens >= 0 && decoded.Usage.CompletionTokens >= 0
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
