package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ditto-assistant/dittobench-api/internal/llm"
	"github.com/ditto-assistant/dittobench-datagen/protocol"
	"github.com/google/uuid"
)

const testBrokerAgentID = "00000000-0000-0000-0000-000000000002"

func TestLegacyBrokerSessionsRunConcurrentlyWithIsolatedAccounting(t *testing.T) {
	var inFlight atomic.Int64
	var peak atomic.Int64
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("legacy broker must not send a provider bearer to model-relay")
		}
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4},"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer upstream.Close()

	broker := newInferenceBroker(2)
	ids := make([]string, 2)
	runIDs := []string{uuid.NewString(), uuid.NewString()}
	for i, ip := range []string{"192.0.2.40", "192.0.2.41"} {
		id, err := broker.prepareLegacy(runIDs[i], protocol.BenchVersionV6, upstream.URL, relayHealthSnapshot{
			Provider: "openrouter", ProfileRevision: llm.OpenRouterRelayProfileRevision,
			Model: llm.LockedHarnessModel,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		if !broker.bindSource(id, runIDs[i], ip) {
			t.Fatalf("failed to bind legacy session %d", i)
		}
	}

	var wg sync.WaitGroup
	recorders := make([]*httptest.ResponseRecorder, 2)
	for i, ip := range []string{"192.0.2.40", "192.0.2.41"} {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			// Match the frozen starter-kit adapter exactly: CHUTES_BASE_URL is
			// the broker base and the client appends /chat/completions.
			request := httptest.NewRequest(http.MethodPost, "/v1/inference/chat/completions", bytes.NewBufferString(`{"model":"qwen/qwen3-32b"}`))
			request.RemoteAddr = ip + ":4321"
			request.SetPathValue("rest", "chat/completions")
			recorders[i] = httptest.NewRecorder()
			broker.handle(recorders[i], request)
		}(i, ip)
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("legacy requests did not overlap")
		}
	}
	close(release)
	wg.Wait()

	if peak.Load() != 2 {
		t.Fatalf("peak legacy relay concurrency = %d, want 2", peak.Load())
	}
	for i, id := range ids {
		if recorders[i].Code != http.StatusOK {
			t.Fatalf("legacy response %d = %d: %s", i, recorders[i].Code, recorders[i].Body.String())
		}
		snapshot, err := broker.snapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Provider != "openrouter" || snapshot.ProfileRevision != llm.OpenRouterRelayProfileRevision ||
			snapshot.Model != llm.LockedHarnessModel || snapshot.Requests != 1 || snapshot.Successes != 1 ||
			snapshot.UsageAvailable != 1 {
			t.Fatalf("legacy session %d accounting = %+v", i, snapshot)
		}
		if snapshot.PromptTokens != 3 || snapshot.CompletionTokens != 4 {
			t.Fatalf("legacy session %d token accounting = %+v", i, snapshot)
		}
	}
}

func TestLegacyBrokerRejectsUnreviewedRelayIdentity(t *testing.T) {
	broker := newInferenceBroker(1)
	if _, err := broker.prepareLegacy(uuid.NewString(), protocol.BenchVersionV6, "http://127.0.0.1:11434", relayHealthSnapshot{
		Provider: "openrouter", ProfileRevision: "mutable-route", Model: llm.LockedHarnessModel,
	}); err == nil {
		t.Fatal("unreviewed legacy relay identity was accepted")
	}
}

func embeddingBrokerSession(t *testing.T, broker *inferenceBroker, source string) (string, string) {
	t.Helper()
	runID := uuid.NewString()
	id, err := broker.prepareLegacy(runID, protocol.BenchVersionV6, "http://127.0.0.1:11435", relayHealthSnapshot{
		Provider: "openrouter", ProfileRevision: llm.OpenRouterRelayProfileRevision,
		Model: llm.LockedHarnessModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !broker.bindSource(id, runID, source) {
		t.Fatal("failed to bind embedding broker source")
	}
	return id, runID
}

func admittedEmbeddingBrokerSession(t *testing.T, broker *inferenceBroker, source string) (string, string) {
	t.Helper()
	id, runID := embeddingBrokerSession(t, broker, source)
	if !broker.beginEmbeddingPhase(id, runID) {
		t.Fatal("failed to admit embedding phase")
	}
	t.Cleanup(func() { broker.endEmbeddingPhase(id, runID) })
	return id, runID
}

func writeTestEmbedding(w http.ResponseWriter, inputs int) {
	vector := make([]float64, embeddingDimensions)
	vectors := make([][]float64, inputs)
	for index := range vectors {
		vectors[index] = vector
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"embeddings":        vectors,
		"prompt_eval_count": inputs,
	})
}

func callEmbedding(broker *inferenceBroker, source, input string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, embeddingAPIPath, bytes.NewBufferString(
		`{"model":"embeddinggemma","input":[`+strconv.Quote(input)+`]}`,
	))
	request.RemoteAddr = source + ":4321"
	recorder := httptest.NewRecorder()
	broker.handleEmbedding(recorder, request)
	return recorder
}

func TestEmbeddingBrokerCapacityOneForwardsOnlyLockedOperation(t *testing.T) {
	vector := make([]float64, embeddingDimensions)
	for index := range vector {
		vector[index] = float64(index) / embeddingDimensions
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != embeddingAPIPath {
			t.Fatalf("unexpected embedding upstream route %s %s", r.Method, r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request) != 2 || request["model"] != embeddingModel {
			t.Fatalf("unlocked embedding request: %#v", request)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"model":             embeddingModel,
			"embeddings":        [][]float64{vector},
			"prompt_eval_count": 3,
		})
	}))
	defer upstream.Close()

	broker := newInferenceBroker(1)
	broker.embeddingURL = upstream.URL + embeddingAPIPath
	broker.client.Transport = upstream.Client().Transport
	admittedEmbeddingBrokerSession(t, broker, "192.0.2.60")
	request := httptest.NewRequest(http.MethodPost, embeddingAPIPath, bytes.NewBufferString(
		`{"model":"embeddinggemma","input":["bounded text"]}`,
	))
	request.RemoteAddr = "192.0.2.60:4321"
	recorder := httptest.NewRecorder()
	broker.handleEmbedding(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("embedding status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 2 || response["model"] != nil {
		t.Fatalf("embedding response leaked upstream metadata: %s", recorder.Body.String())
	}
}

func TestV7EmbeddingBrokerUsesSignedLockedPlatformRoute(t *testing.T) {
	vector := make([]float64, embeddingDimensions)
	var calls atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != platformEmbeddingAPIPath {
			t.Fatalf("unexpected hosted embedding route %s %s", r.Method, r.URL.Path)
		}
		for _, header := range []string{"Authorization", "X-Ditto-Grant", "X-Ditto-Generation", "X-Ditto-Nonce", "X-Ditto-Requested-At", "X-Ditto-Proof"} {
			if r.Header.Get(header) == "" {
				t.Fatalf("missing signed embedding header %s", header)
			}
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 4 || payload["model"] != hostedEmbeddingModel || payload["dimensions"] != float64(embeddingDimensions) || payload["encoding_format"] != "float" {
			t.Fatalf("unlocked hosted embedding payload: %#v", payload)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list", "model": hostedEmbeddingModel,
			"data":  []map[string]any{{"object": "embedding", "index": 0, "embedding": vector}},
			"usage": map[string]int{"prompt_tokens": 4, "total_tokens": 4},
		})
	}))
	defer upstream.Close()

	broker := newInferenceBroker(1)
	proxyURL := configureBrokerUpstream(broker, upstream)
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSessionFor(t, broker, prepared, proxyURL, "openrouter", "openrouter-route-0123456789abcdef-v1", llm.V7HarnessModel)
	runID := claimAndBindBrokerSession(t, broker, prepared["session_id"], "192.0.2.70", protocol.BenchVersionV7)
	if !broker.beginEmbeddingPhase(prepared["session_id"], runID) {
		t.Fatal("failed to admit v7 embedding phase")
	}
	defer broker.endEmbeddingPhase(prepared["session_id"], runID)
	response := callEmbedding(broker, "192.0.2.70", "hosted text")
	if response.Code != http.StatusOK {
		t.Fatalf("hosted embedding status=%d body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("hosted embedding calls=%d, want 1", calls.Load())
	}
}

func TestEmbeddingBrokerRejectsSiblingModelAndManagementProbes(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()
	broker := newInferenceBroker(1)
	broker.embeddingURL = upstream.URL + embeddingAPIPath
	broker.client.Transport = upstream.Client().Transport
	admittedEmbeddingBrokerSession(t, broker, "192.0.2.61")

	tests := []struct {
		method string
		path   string
		body   string
		ip     string
		status int
	}{
		{http.MethodPost, embeddingAPIPath, `{"model":"other","input":["x"]}`, "192.0.2.61", http.StatusBadRequest},
		{http.MethodPost, embeddingAPIPath, `{"model":"embeddinggemma","input":["x"],"keep_alive":"24h"}`, "192.0.2.61", http.StatusBadRequest},
		{http.MethodPost, "/api/pull", `{"model":"embeddinggemma"}`, "192.0.2.61", http.StatusNotFound},
		{http.MethodGet, embeddingAPIPath, "", "192.0.2.61", http.StatusNotFound},
		{http.MethodPost, embeddingAPIPath, `{"model":"embeddinggemma","input":["x"]}`, "192.0.2.62", http.StatusUnauthorized},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, bytes.NewBufferString(test.body))
		request.RemoteAddr = test.ip + ":4321"
		recorder := httptest.NewRecorder()
		broker.handleEmbedding(recorder, request)
		if recorder.Code != test.status {
			t.Fatalf("%s %s status=%d want=%d body=%s", test.method, test.path, recorder.Code, test.status, recorder.Body.String())
		}
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("rejected embedding probes reached upstream %d time(s)", upstreamCalls.Load())
	}
}

func TestEmbeddingBrokerRejectsPrePhaseAndLateUse(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		writeTestEmbedding(w, 1)
	}))
	defer upstream.Close()
	broker := newInferenceBroker(1)
	broker.embeddingURL = upstream.URL + embeddingAPIPath
	broker.client.Transport = upstream.Client().Transport
	id, runID := embeddingBrokerSession(t, broker, "192.0.2.62")

	if got := callEmbedding(broker, "192.0.2.62", "before").Code; got != http.StatusConflict {
		t.Fatalf("pre-phase embedding status = %d", got)
	}
	if !broker.beginEmbeddingPhase(id, runID) {
		t.Fatal("failed to begin embedding phase")
	}
	if got := callEmbedding(broker, "192.0.2.62", "during").Code; got != http.StatusOK {
		t.Fatalf("admitted embedding status = %d", got)
	}
	broker.endEmbeddingPhase(id, runID)
	if got := callEmbedding(broker, "192.0.2.62", "after").Code; got != http.StatusConflict {
		t.Fatalf("late embedding status = %d", got)
	}
	if broker.beginEmbeddingPhase(id, runID) {
		t.Fatal("ended embedding phase reopened")
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("pre/late probes reached upstream %d time(s)", upstreamCalls.Load())
	}
}

func TestEmbeddingBrokerOneRequestPerSessionDoesNotStarveSibling(t *testing.T) {
	firstArrived := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload embeddingRequest
		if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload.Input) != 1 {
			http.Error(w, "bad test request", http.StatusBadRequest)
			return
		}
		if payload.Input[0] == "hold" {
			firstArrived <- struct{}{}
			<-releaseFirst
		}
		writeTestEmbedding(w, 1)
	}))
	defer upstream.Close()
	broker := newInferenceBroker(2, 2)
	broker.embeddingURL = upstream.URL + embeddingAPIPath
	broker.client.Transport = upstream.Client().Transport
	admittedEmbeddingBrokerSession(t, broker, "192.0.2.63")
	admittedEmbeddingBrokerSession(t, broker, "192.0.2.64")

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- callEmbedding(broker, "192.0.2.63", "hold").Code
	}()
	select {
	case <-firstArrived:
	case <-time.After(time.Second):
		close(releaseFirst)
		t.Fatal("first embedding request did not reach upstream")
	}
	if got := callEmbedding(broker, "192.0.2.63", "same-session").Code; got != http.StatusTooManyRequests {
		close(releaseFirst)
		t.Fatalf("same-session concurrent embedding status = %d", got)
	}
	if got := callEmbedding(broker, "192.0.2.64", "sibling").Code; got != http.StatusOK {
		close(releaseFirst)
		t.Fatalf("sibling embedding status = %d", got)
	}
	close(releaseFirst)
	if got := <-firstDone; got != http.StatusOK {
		t.Fatalf("first embedding status = %d", got)
	}
}

func TestEmbeddingBrokerFailsClosedAtEverySessionBudget(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		writeTestEmbedding(w, 1)
	}))
	defer upstream.Close()
	broker := newInferenceBroker(1)
	broker.embeddingURL = upstream.URL + embeddingAPIPath
	broker.client.Transport = upstream.Client().Transport
	id, _ := admittedEmbeddingBrokerSession(t, broker, "192.0.2.65")
	broker.mu.RLock()
	session := broker.sessions[id]
	broker.mu.RUnlock()

	tests := []struct {
		name string
		set  func()
	}{
		{"requests", func() { session.embeddingRequests = embeddingSessionRequests }},
		{"inputs", func() { session.embeddingInputs = embeddingSessionInputs }},
		{"input-bytes", func() { session.embeddingInputBytes = embeddingSessionInputBytes }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session.mu.Lock()
			session.embeddingRequests = 0
			session.embeddingInputs = 0
			session.embeddingInputBytes = 0
			test.set()
			session.mu.Unlock()
			recorder := callEmbedding(broker, "192.0.2.65", "x")
			if recorder.Code != http.StatusTooManyRequests ||
				!strings.Contains(recorder.Body.String(), "budget exhausted") {
				t.Fatalf("budget status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("budget-exhausted requests reached upstream %d time(s)", upstreamCalls.Load())
	}
}

func TestMemoryAdmissionCapacityOneQueuesThenAdmitsSibling(t *testing.T) {
	broker := newInferenceBroker(2, 1)
	firstID, firstRun := embeddingBrokerSession(t, broker, "192.0.2.66")
	secondID, secondRun := embeddingBrokerSession(t, broker, "192.0.2.67")
	server := &server{memorySlots: make(chan struct{}, 1), broker: broker}
	endFirst, ok := server.beginMemoryPhase(context.Background(), firstID, firstRun)
	if !ok {
		t.Fatal("capacity-one first phase was not admitted")
	}

	type admission struct {
		end func()
		ok  bool
	}
	admitted := make(chan admission, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		end, admittedOK := server.beginMemoryPhase(ctx, secondID, secondRun)
		admitted <- admission{end: end, ok: admittedOK}
	}()
	select {
	case <-admitted:
		endFirst()
		t.Fatal("sibling bypassed capacity-one memory admission")
	case <-time.After(25 * time.Millisecond):
	}
	endFirst()
	select {
	case result := <-admitted:
		if !result.ok {
			t.Fatal("sibling was starved after the first phase released capacity")
		}
		result.end()
	case <-time.After(time.Second):
		t.Fatal("sibling did not acquire released memory capacity")
	}
}

func TestMemoryAdmissionDrainsHostileInFlightEmbeddingBeforeSibling(t *testing.T) {
	firstArrived := make(chan struct{})
	firstCanceled := make(chan struct{})
	releaseFirst := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload embeddingRequest
		if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload.Input) != 1 {
			http.Error(w, "bad test request", http.StatusBadRequest)
			return
		}
		if payload.Input[0] == "hostile-background" {
			close(firstArrived)
			select {
			case <-r.Context().Done():
				close(firstCanceled)
			case <-releaseFirst:
			}
			return
		}
		writeTestEmbedding(w, 1)
	}))
	defer upstream.Close()
	defer close(releaseFirst)

	broker := newInferenceBroker(2, 1)
	broker.embeddingURL = upstream.URL + embeddingAPIPath
	broker.client.Transport = upstream.Client().Transport
	firstID, firstRun := embeddingBrokerSession(t, broker, "192.0.2.68")
	secondID, secondRun := embeddingBrokerSession(t, broker, "192.0.2.69")
	server := &server{memorySlots: make(chan struct{}, 1), broker: broker}
	endFirst, ok := server.beginMemoryPhase(context.Background(), firstID, firstRun)
	if !ok {
		t.Fatal("capacity-one first phase was not admitted")
	}

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- callEmbedding(broker, "192.0.2.68", "hostile-background").Code
	}()
	select {
	case <-firstArrived:
	case <-time.After(time.Second):
		t.Fatal("hostile embedding request did not reach upstream")
	}

	type admission struct {
		end func()
		ok  bool
	}
	admitted := make(chan admission, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		end, admittedOK := server.beginMemoryPhase(ctx, secondID, secondRun)
		admitted <- admission{end: end, ok: admittedOK}
	}()
	select {
	case <-admitted:
		t.Fatal("sibling bypassed the first memory-phase admission")
	case <-time.After(25 * time.Millisecond):
	}

	endFirst()
	var sibling admission
	select {
	case sibling = <-admitted:
		if !sibling.ok {
			t.Fatal("sibling was not admitted after predecessor drain")
		}
	case <-time.After(time.Second):
		t.Fatal("sibling was starved after predecessor drain")
	}
	defer sibling.end()
	if got := len(broker.embeddingSlots); got != 0 {
		t.Fatalf("memory admission released with %d predecessor embedding slot(s) held", got)
	}
	select {
	case status := <-firstDone:
		if status != http.StatusBadGateway {
			t.Fatalf("canceled predecessor embedding status = %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled predecessor embedding request did not drain")
	}
	select {
	case <-firstCanceled:
	case <-time.After(time.Second):
		t.Fatal("predecessor embedding cancellation did not reach upstream")
	}
	if got := callEmbedding(broker, "192.0.2.69", "sibling").Code; got != http.StatusOK {
		t.Fatalf("sibling embedding inherited predecessor capacity: status=%d", got)
	}
}

func prepareBrokerSession(t *testing.T, broker *inferenceBroker) map[string]string {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/inference/session", nil)
	request.RemoteAddr = "127.0.0.1:4321"
	broker.prepare(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("prepare status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var prepared map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	return prepared
}

func activateBrokerSession(t *testing.T, broker *inferenceBroker, prepared map[string]string, proxyURL string) {
	t.Helper()
	activateBrokerSessionFor(t, broker, prepared, proxyURL, "", "", "")
}

func activateBrokerSessionFor(t *testing.T, broker *inferenceBroker, prepared map[string]string, proxyURL, provider, profile, model string) {
	t.Helper()
	ticketDeadline := time.Now().Add(2 * time.Minute)
	body, _ := json.Marshal(brokerActivation{
		ActivationSecret: prepared["activation_secret"],
		GrantID:          "00000000-0000-0000-0000-000000000001",
		AgentID:          testBrokerAgentID,
		SlotID:           "slot-0",
		TicketDeadline:   ticketDeadline,
		Bearer:           "platform-bearer-never-given-to-harness",
		ProxyURL:         proxyURL,
		Generation:       1,
		ExpiresAt:        time.Now().Add(time.Minute),
		Provider:         provider,
		ProfileRevision:  profile,
		Model:            model,
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/inference/session/id/activate", bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:4321"
	request.SetPathValue("id", prepared["session_id"])
	recorder := httptest.NewRecorder()
	broker.activate(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("activate status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func claimAndBindBrokerSession(t *testing.T, broker *inferenceBroker, sessionID, sourceIP string, benchVersion int) string {
	t.Helper()
	broker.mu.RLock()
	session := broker.sessions[sessionID]
	broker.mu.RUnlock()
	if session == nil {
		t.Fatal("prepared broker session disappeared")
	}
	session.mu.Lock()
	identity := brokerTicketIdentity{
		GrantID: session.grantID, AgentID: session.ticketAgentID,
		SlotID: session.ticketSlotID, TicketDeadline: session.ticketDeadline,
	}
	session.mu.Unlock()
	runID := uuid.NewString()
	if !broker.claimRun(sessionID, runID, identity, benchVersion) {
		t.Fatal("failed to claim prepared broker session")
	}
	if !broker.bindSource(sessionID, runID, sourceIP) {
		t.Fatal("failed to bind claimed broker session")
	}
	return runID
}

func configureBrokerUpstream(broker *inferenceBroker, upstream *httptest.Server) string {
	proxyURL := upstream.URL + platformInferenceAPIPath
	broker.platformProxyURL = proxyURL
	broker.client.Transport = upstream.Client().Transport
	return proxyURL
}

func activationResponse(
	broker *inferenceBroker,
	prepared map[string]string,
	proxyURL string,
	expiresAt time.Time,
) *httptest.ResponseRecorder {
	body, _ := json.Marshal(brokerActivation{
		ActivationSecret: prepared["activation_secret"],
		GrantID:          "00000000-0000-0000-0000-000000000001",
		Bearer:           "platform-bearer-never-given-to-harness",
		ProxyURL:         proxyURL,
		Generation:       1,
		ExpiresAt:        expiresAt,
	})
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/inference/session/id/activate",
		bytes.NewReader(body),
	)
	request.RemoteAddr = "127.0.0.1:4321"
	request.SetPathValue("id", prepared["session_id"])
	recorder := httptest.NewRecorder()
	broker.activate(recorder, request)
	return recorder
}

func TestInferenceControlPlaneRequiresLoopbackOrBearer(t *testing.T) {
	broker := newInferenceBroker(1)
	broker.controlToken = "control-secret"

	external := httptest.NewRequest(http.MethodPost, "/v1/inference/session", nil)
	external.RemoteAddr = "192.0.2.5:4321"
	recorder := httptest.NewRecorder()
	broker.prepare(recorder, external)
	if recorder.Code != http.StatusUnauthorized || len(broker.sessions) != 0 {
		t.Fatalf("unauthorized prepare status=%d sessions=%d", recorder.Code, len(broker.sessions))
	}

	external = httptest.NewRequest(http.MethodPost, "/v1/inference/session", nil)
	external.RemoteAddr = "192.0.2.5:4321"
	external.Header.Set("Authorization", "Bearer control-secret")
	recorder = httptest.NewRecorder()
	broker.prepare(recorder, external)
	if recorder.Code != http.StatusCreated || len(broker.sessions) != 1 {
		t.Fatalf("authorized prepare status=%d sessions=%d", recorder.Code, len(broker.sessions))
	}
}

func TestInferenceActivationRequiresConfiguredExactProxyAndBoundedExpiry(t *testing.T) {
	broker := newInferenceBroker(1)
	broker.platformProxyURL = "https://platform.example" + platformInferenceAPIPath
	prepared := prepareBrokerSession(t, broker)

	recorder := activationResponse(
		broker,
		prepared,
		"https://attacker.example"+platformInferenceAPIPath,
		time.Now().Add(time.Minute),
	)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured proxy status = %d", recorder.Code)
	}
	recorder = activationResponse(
		broker,
		prepared,
		broker.platformProxyURL,
		time.Now().Add(brokerMaximumSessionTTL+time.Minute),
	)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("overlong expiry status = %d", recorder.Code)
	}
	recorder = activationResponse(
		broker,
		prepared,
		broker.platformProxyURL,
		time.Now().Add(time.Minute),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("valid activation status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestInferenceActivationRequiresTicketIdentityForV7Route(t *testing.T) {
	broker := newInferenceBroker(1)
	broker.platformProxyURL = "https://platform.example" + platformInferenceAPIPath
	prepared := prepareBrokerSession(t, broker)
	body, _ := json.Marshal(brokerActivation{
		ActivationSecret: prepared["activation_secret"],
		GrantID:          "00000000-0000-0000-0000-000000000001",
		Bearer:           "platform-bearer-never-given-to-harness",
		ProxyURL:         broker.platformProxyURL,
		Generation:       1,
		ExpiresAt:        time.Now().Add(time.Minute),
		Provider:         "amazon-bedrock",
		ProfileRevision:  "openrouter-route-0123456789abcdef-v1",
		Model:            llm.V7HarnessModel,
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/inference/session/id/activate", bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:4321"
	request.SetPathValue("id", prepared["session_id"])
	recorder := httptest.NewRecorder()
	broker.activate(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("identity-free v7 activation status = %d", recorder.Code)
	}
}

func TestConfiguredPlatformProxyURLFailsClosed(t *testing.T) {
	valid := "https://platform.example" + platformInferenceAPIPath
	if got := configuredPlatformProxyURL("  " + valid + "  "); got != valid {
		t.Fatalf("configured proxy = %q", got)
	}
	for _, invalid := range []string{
		"http://platform.example" + platformInferenceAPIPath,
		"https://platform.example/other",
		valid + "?next=https://attacker.example",
		"https://user@platform.example" + platformInferenceAPIPath,
	} {
		if got := configuredPlatformProxyURL(invalid); got != "" {
			t.Errorf("unsafe proxy %q accepted as %q", invalid, got)
		}
	}
}

func TestInferenceBrokerDoesNotFollowPlatformRedirects(t *testing.T) {
	redirectFollowed := false
	destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectFollowed = true
	}))
	defer destination.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+platformInferenceAPIPath, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	broker := newInferenceBroker(1)
	proxyURL := configureBrokerUpstream(broker, redirector)
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSession(t, broker, prepared, proxyURL)
	claimAndBindBrokerSession(t, broker, prepared["session_id"], "192.0.2.50", protocol.BenchVersionV6)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/inference/v1/chat/completions",
		bytes.NewBufferString(`{"model":"qwen/qwen3-32b"}`),
	)
	request.RemoteAddr = "192.0.2.50:4321"
	request.SetPathValue("rest", "v1/chat/completions")
	recorder := httptest.NewRecorder()
	broker.handle(recorder, request)
	if recorder.Code != http.StatusBadGateway || redirectFollowed {
		t.Fatalf("redirect status=%d followed=%t", recorder.Code, redirectFollowed)
	}
}

func TestInferenceBrokerRejectsSiblingSource(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("wrong-source request reached upstream")
	}))
	defer upstream.Close()
	broker := newInferenceBroker(1)
	proxyURL := configureBrokerUpstream(broker, upstream)
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSession(t, broker, prepared, proxyURL)
	claimAndBindBrokerSession(t, broker, prepared["session_id"], "10.0.0.10", protocol.BenchVersionV6)

	request := httptest.NewRequest(http.MethodPost, "/v1/inference/id/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen/qwen3-32b"}`))
	request.RemoteAddr = "10.0.0.11:4321"
	request.SetPathValue("rest", "v1/chat/completions")
	recorder := httptest.NewRecorder()
	broker.handle(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong source status = %d", recorder.Code)
	}
}

func TestInferenceBrokerAddsProofWithoutExposingBearerToHarness(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, header := range []string{"Authorization", "X-Ditto-Grant", "X-Ditto-Generation", "X-Ditto-Nonce", "X-Ditto-Requested-At", "X-Ditto-Proof"} {
			if r.Header.Get(header) == "" {
				t.Errorf("missing trusted broker header %s", header)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4},"choices":[]}`))
	}))
	defer upstream.Close()
	broker := newInferenceBroker(1)
	proxyURL := configureBrokerUpstream(broker, upstream)
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSession(t, broker, prepared, proxyURL)
	claimAndBindBrokerSession(t, broker, prepared["session_id"], "192.0.2.10", protocol.BenchVersionV6)

	request := httptest.NewRequest(http.MethodPost, "/v1/inference/id/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen/qwen3-32b","max_tokens":32}`))
	request.RemoteAddr = "192.0.2.10:4321"
	request.SetPathValue("rest", "v1/chat/completions")
	recorder := httptest.NewRecorder()
	broker.handle(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("platform-bearer")) {
		t.Fatal("platform bearer leaked to harness response")
	}
}

func TestInferenceBrokerTrustedProbeUsesControlPlaneSession(t *testing.T) {
	const profile = "openrouter-route-0123456789abcdef-v1"
	embeddingCalls := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == platformEmbeddingAPIPath {
			embeddingCalls++
			vector := make([]float64, embeddingDimensions)
			writeJSON(w, http.StatusOK, map[string]any{
				"object": "list", "model": hostedEmbeddingModel,
				"data":  []map[string]any{{"object": "embedding", "index": 0, "embedding": vector}},
				"usage": map[string]int{"prompt_tokens": 3, "total_tokens": 3},
			})
			return
		}
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":2,"completion_tokens":1},"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer upstream.Close()
	broker := newInferenceBroker(1)
	proxyURL := configureBrokerUpstream(broker, upstream)
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSessionFor(t, broker, prepared, proxyURL, "amazon-bedrock", profile, llm.V7HarnessModel)
	claimAndBindBrokerSession(t, broker, prepared["session_id"], "192.0.2.30", protocol.BenchVersionV7)

	if err := broker.trustedProbe(context.Background(), prepared["session_id"]); err != nil {
		t.Fatalf("trusted probe: %v", err)
	}
	snapshot, err := broker.snapshot(prepared["session_id"])
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Requests != 1 || snapshot.Successes != 1 || snapshot.UsageAvailable != 1 {
		t.Fatalf("unexpected trusted probe accounting: %+v", snapshot)
	}
	if embeddingCalls != 1 {
		t.Fatalf("trusted v7 probe made %d embedding calls, want 1", embeddingCalls)
	}
	if snapshot.Model != llm.V7HarnessModel || snapshot.ProfileRevision != profile || snapshot.Provider != "amazon-bedrock" {
		t.Fatalf("v7 relay identity = %+v", snapshot)
	}
}

func TestInferenceBrokerCountsProviderDenialAsIncompleteUsage(t *testing.T) {
	const profile = "openrouter-route-0123456789abcdef-v1"
	requestCount := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		if requestCount == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":2,"completion_tokens":1},"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer upstream.Close()

	broker := newInferenceBroker(1)
	proxyURL := configureBrokerUpstream(broker, upstream)
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSessionFor(t, broker, prepared, proxyURL, "groq", profile, llm.V7HarnessModel)
	claimAndBindBrokerSession(t, broker, prepared["session_id"], "192.0.2.31", protocol.BenchVersionV7)
	start, err := broker.snapshot(prepared["session_id"])
	if err != nil {
		t.Fatal(err)
	}

	for i, wantStatus := range []int{http.StatusTooManyRequests, http.StatusOK} {
		request := httptest.NewRequest(http.MethodPost, "/v1/inference/id/v1/chat/completions", bytes.NewBufferString(`{"model":"openai/gpt-oss-20b"}`))
		request.RemoteAddr = "192.0.2.31:4321"
		request.SetPathValue("rest", "v1/chat/completions")
		recorder := httptest.NewRecorder()
		broker.handle(recorder, request)
		if recorder.Code != wantStatus {
			t.Fatalf("request %d status = %d, want %d: %s", i+1, recorder.Code, wantStatus, recorder.Body.String())
		}
	}

	end, err := broker.snapshot(prepared["session_id"])
	if err != nil {
		t.Fatal(err)
	}
	usage, err := relayUsageSince(start, end)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Requests != 2 || usage.Successes != 1 || usage.UsageAvailable != 1 || usage.UsageUnavailable != 1 || usage.Status != "unavailable" {
		t.Fatalf("mixed provider result accounting = %+v", usage)
	}
	if err := requireCompleteV7Usage(protocol.BenchVersionV7, usage); err == nil {
		t.Fatal("v7 accepted a provider denial hidden behind a later success")
	}
}

func TestInferenceBrokerRejectsWrongVersionModel(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("wrong-model request reached upstream")
	}))
	defer upstream.Close()
	broker := newInferenceBroker(1)
	proxyURL := configureBrokerUpstream(broker, upstream)
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSessionFor(t, broker, prepared, proxyURL, "amazon-bedrock", "openrouter-route-0123456789abcdef-v1", llm.V7HarnessModel)
	claimAndBindBrokerSession(t, broker, prepared["session_id"], "192.0.2.40", protocol.BenchVersionV7)
	request := httptest.NewRequest(http.MethodPost, "/v1/inference/id/v1/chat/completions", bytes.NewBufferString(`{"model":"qwen/qwen3-32b"}`))
	request.RemoteAddr = "192.0.2.40:4321"
	request.SetPathValue("rest", "v1/chat/completions")
	recorder := httptest.NewRecorder()
	broker.handle(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong model status = %d", recorder.Code)
	}
}

func TestInferenceBrokerClaimsTicketIdentityOnceAndRejectsSiblingRemoval(t *testing.T) {
	const profile = "openrouter-route-0123456789abcdef-v1"
	broker := newInferenceBroker(1)
	broker.platformProxyURL = "https://platform.example" + platformInferenceAPIPath
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSessionFor(
		t, broker, prepared, broker.platformProxyURL,
		"amazon-bedrock", profile, llm.V7HarnessModel,
	)

	broker.mu.RLock()
	session := broker.sessions[prepared["session_id"]]
	broker.mu.RUnlock()
	session.mu.Lock()
	identity := brokerTicketIdentity{
		GrantID: session.grantID, AgentID: session.ticketAgentID,
		SlotID: session.ticketSlotID, TicketDeadline: session.ticketDeadline,
	}
	session.mu.Unlock()

	wrongIdentity := identity
	wrongIdentity.AgentID = "00000000-0000-0000-0000-000000000003"
	if broker.claimRun(prepared["session_id"], uuid.NewString(), wrongIdentity, protocol.BenchVersionV7) {
		t.Fatal("session accepted a run for a sibling ticket identity")
	}
	ownerRunID := uuid.NewString()
	if !broker.claimRun(prepared["session_id"], ownerRunID, identity, protocol.BenchVersionV7) {
		t.Fatal("session rejected its ticket identity")
	}
	if broker.claimRun(prepared["session_id"], uuid.NewString(), identity, protocol.BenchVersionV7) {
		t.Fatal("session accepted a second run claim")
	}
	if broker.bindSource(prepared["session_id"], uuid.NewString(), "192.0.2.60") {
		t.Fatal("sibling run bound a source")
	}
	if !broker.bindSource(prepared["session_id"], ownerRunID, "192.0.2.60") {
		t.Fatal("owner run could not bind its source")
	}
	if broker.bindSource(prepared["session_id"], ownerRunID, "192.0.2.61") {
		t.Fatal("owner run rebound an already-bound source")
	}
	if broker.removeRun(prepared["session_id"], uuid.NewString()) {
		t.Fatal("sibling run removed the session")
	}
	broker.mu.RLock()
	stillPresent := broker.sessions[prepared["session_id"]] != nil
	broker.mu.RUnlock()
	if !stillPresent {
		t.Fatal("sibling removal deleted the owner session")
	}
	if !broker.removeRun(prepared["session_id"], ownerRunID) {
		t.Fatal("owner run could not remove its session")
	}
}

type readTrackingBody struct {
	reads int
}

func (b *readTrackingBody) Read([]byte) (int, error) {
	b.reads++
	return 0, io.EOF
}

func TestInferenceBrokerRejectsOverCapacityBeforeReadingBody(t *testing.T) {
	broker := newInferenceBroker(1)
	broker.platformProxyURL = "https://platform.example" + platformInferenceAPIPath
	prepared := prepareBrokerSession(t, broker)
	activateBrokerSession(t, broker, prepared, broker.platformProxyURL)
	claimAndBindBrokerSession(t, broker, prepared["session_id"], "192.0.2.70", protocol.BenchVersionV6)

	broker.mu.RLock()
	session := broker.sessions[prepared["session_id"]]
	broker.mu.RUnlock()
	session.mu.Lock()
	session.inFlight = brokerPerSourceConcurrency
	session.mu.Unlock()
	body := &readTrackingBody{}
	request := httptest.NewRequest(http.MethodPost, "/v1/inference/v1/chat/completions", body)
	request.RemoteAddr = "192.0.2.70:4321"
	request.SetPathValue("rest", "v1/chat/completions")
	recorder := httptest.NewRecorder()
	broker.handle(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("over-capacity status = %d", recorder.Code)
	}
	if body.reads != 0 {
		t.Fatalf("request body read %d time(s) before capacity rejection", body.reads)
	}
}

func TestInferenceBrokerHTTPServerSetsUntrustedListenerTimeouts(t *testing.T) {
	server := newInferenceBrokerHTTPServer(":11436", http.NewServeMux())
	if server.ReadHeaderTimeout != brokerReadHeaderTimeout ||
		server.ReadTimeout != brokerReadTimeout || server.WriteTimeout != brokerWriteTimeout ||
		server.IdleTimeout != brokerIdleTimeout || server.MaxHeaderBytes != brokerMaximumHeaderBytes {
		t.Fatalf("unexpected broker server limits: %+v", server)
	}
}

func TestInferenceBrokerPrunesUnactivatedSessionsBeforeCapacityCheck(t *testing.T) {
	broker := newInferenceBroker(1) // bounded to two prepared/active sessions
	first := prepareBrokerSession(t, broker)
	_ = prepareBrokerSession(t, broker)

	broker.mu.RLock()
	stale := broker.sessions[first["session_id"]]
	broker.mu.RUnlock()
	stale.mu.Lock()
	stale.preparedAt = time.Now().Add(-3 * time.Minute)
	stale.mu.Unlock()

	third := prepareBrokerSession(t, broker)
	if third["session_id"] == "" {
		t.Fatal("stale prepared session did not release broker capacity")
	}
}

func TestToolRouteIsSourceBoundAndRemoved(t *testing.T) {
	broker := newInferenceBroker(1)
	called := 0
	id, stop, err := broker.registerTool(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.URL.Path != "/tool" {
			t.Errorf("forwarded path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}), "192.0.2.20")
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/tools/route/tool", nil)
	request.SetPathValue("id", id)
	request.RemoteAddr = "192.0.2.21:1234"
	recorder := httptest.NewRecorder()
	broker.handleTool(recorder, request)
	if recorder.Code != http.StatusUnauthorized || called != 0 {
		t.Fatalf("sibling source status=%d called=%d", recorder.Code, called)
	}

	request.RemoteAddr = "192.0.2.20:1234"
	recorder = httptest.NewRecorder()
	broker.handleTool(recorder, request)
	if recorder.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("bound source status=%d called=%d", recorder.Code, called)
	}

	stop()
	recorder = httptest.NewRecorder()
	broker.handleTool(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("removed route status=%d", recorder.Code)
	}
}

func TestToolRouteRejectsOverCapacityBeforeReadingBody(t *testing.T) {
	broker := newInferenceBroker(1)
	called := false
	id, stop, err := broker.registerTool(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), "192.0.2.80")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	broker.mu.RLock()
	route := broker.tools[id]
	broker.mu.RUnlock()
	for range brokerPerSourceConcurrency {
		route.slots <- struct{}{}
	}
	body := &readTrackingBody{}
	request := httptest.NewRequest(http.MethodPost, "/v1/tools/route/tool", body)
	request.SetPathValue("id", id)
	request.RemoteAddr = "192.0.2.80:1234"
	recorder := httptest.NewRecorder()
	broker.handleTool(recorder, request)
	if recorder.Code != http.StatusTooManyRequests || called || body.reads != 0 {
		t.Fatalf("over-capacity tool status=%d called=%t body_reads=%d", recorder.Code, called, body.reads)
	}
}
