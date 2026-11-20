package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// §24.20 M49–M55 and §24.21 M56–M62: upstream status classification, the
// fallback round and final error forwarding. These tests live in package proxy
// so they can observe breaker counters and the stats aggregator directly.

func instFailureCount(inst *instanceRuntime) int {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.consecutiveFailures
}

func sendProxyRequest(t *testing.T, serverURL string, proto protocol, body string, auth bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, serverURL+cbPath(proto), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if auth {
		if proto == protocolAnthropic {
			req.Header.Set("x-api-key", "tok")
		} else {
			req.Header.Set("Authorization", "Bearer tok")
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// TestUpstream400FailsOverAndRecordsFailure covers M15ba: a 400 received before
// the response head was committed fails the candidate over to the next one and
// counts one failure against that instance.
func TestUpstream400FailsOverAndRecordsFailure(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusBadRequest)
	second := newCBBackend(t, http.StatusOK)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`, true)
	if status != http.StatusOK {
		t.Fatalf("client status = %d, want 200 (fell over to the second instance)", status)
	}
	if body != `{"id":"ok"}` {
		t.Errorf("client body = %q, want the second instance's body", body)
	}
	if first.calls() != 1 || second.calls() != 1 {
		t.Errorf("calls = %d/%d, want 1/1", first.calls(), second.calls())
	}
	if got := instFailureCount(handler.byProtocol[protocolOpenAI][0]); got != 1 {
		t.Errorf("first consecutiveFailures = %d, want 1", got)
	}
}

// TestUpstream500FailsOverAndRecordsFailure covers M15bb: the same classification
// applies to 5xx.
func TestUpstream500FailsOverAndRecordsFailure(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusInternalServerError)
	second := newCBBackend(t, http.StatusOK)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, _ := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`, true)
	if status != http.StatusOK {
		t.Fatalf("client status = %d, want 200 (fell over to the second instance)", status)
	}
	if first.calls() != 1 || second.calls() != 1 {
		t.Errorf("calls = %d/%d, want 1/1", first.calls(), second.calls())
	}
	if got := instFailureCount(handler.byProtocol[protocolOpenAI][0]); got != 1 {
		t.Errorf("first consecutiveFailures = %d, want 1", got)
	}
}

// TestUpstream400AllCandidatesFailedForwardsOriginal covers M15bc: once the
// candidate list is exhausted, the client receives the upstream 400 and its body,
// and recentErrors carries the status, a non-empty error and the body excerpt.
func TestUpstream400AllCandidatesFailedForwardsOriginal(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusBadRequest)
	second := newCBBackend(t, http.StatusBadRequest)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	handler.monitor = newMonitorHub()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`, true)
	if status != http.StatusBadRequest {
		t.Fatalf("client status = %d, want 400 (upstream status preserved)", status)
	}
	if body != `{"id":"ok"}` {
		t.Errorf("client body = %q, want the original upstream body", body)
	}
	snapshot := handler.monitor.stats.snapshot(0, time.Now())
	if len(snapshot.RecentErrors) != 1 {
		t.Fatalf("recentErrors = %d, want 1", len(snapshot.RecentErrors))
	}
	entry := snapshot.RecentErrors[0]
	if entry.Status != http.StatusBadRequest {
		t.Errorf("recentErrors status = %d, want 400", entry.Status)
	}
	if entry.Error == "" {
		t.Error("recentErrors error is empty, want the upstream status message")
	}
	if entry.UpstreamBody == "" {
		t.Error("recentErrors upstreamBody is empty, want the response body excerpt")
	}
	if entry.InstName != "second" {
		t.Errorf("recentErrors instName = %q, want second", entry.InstName)
	}
}

// TestCommittedStreamIsNotRetried covers M15bd: once the first SSE chunk has
// committed the response, a later read error is not a failover trigger.
func TestCommittedStreamIsNotRetried(t *testing.T) {
	var firstCalls atomic.Int32
	streaming := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(streaming.Close)

	second := newCBBackend(t, http.StatusOK)
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	handler := newCBHandler(t, clock, 1, time.Minute,
		config.Instance{
			Protocol: config.ProtocolOpenAI,
			Name:     "stream",
			BaseURL:  streaming.URL,
			APIKey:   "stream-key",
			Timeout:  config.Duration(2 * time.Second),
			Models:   []string{"gpt-x"},
		},
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, _ := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x","stream":true}`, true)
	if status != http.StatusOK {
		t.Fatalf("client status = %d, want the committed 200", status)
	}
	if got := firstCalls.Load(); got != 1 {
		t.Errorf("streaming instance calls = %d, want 1", got)
	}
	if got := second.calls(); got != 0 {
		t.Errorf("second instance calls = %d, want 0 (committed response is not retried)", got)
	}
}

// TestUpstream5xxOpensBreakerAtThreshold covers M15be: consecutive 5xx responses
// count as instance failures and eventually open the breaker.
func TestUpstream5xxOpensBreakerAtThreshold(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusInternalServerError)
	second := newCBBackend(t, http.StatusInternalServerError)
	handler := newCBHandler(t, clock, 2, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	for i := 0; i < 2; i++ {
		if status, _ := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`, true); status != http.StatusInternalServerError {
			t.Fatalf("request %d client status = %d, want 500 (upstream status preserved)", i+1, status)
		}
	}
	if got := instFailureCount(handler.byProtocol[protocolOpenAI][0]); got != 2 {
		t.Errorf("first consecutiveFailures = %d, want 2", got)
	}
	if state := handler.byProtocol[protocolOpenAI][0].state(); state != stateOpen {
		t.Errorf("first state = %q, want open after failure_threshold 5xx responses", state)
	}
}

// TestLocalAuthErrorDoesNotCountAgainstInstance covers M15bf: locally generated
// errors never reach an instance and never touch breaker counters.
func TestLocalAuthErrorDoesNotCountAgainstInstance(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusOK)
	second := newCBBackend(t, http.StatusOK)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, _ := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`, false)
	if status != http.StatusUnauthorized {
		t.Fatalf("client status = %d, want 401", status)
	}
	if first.calls() != 0 || second.calls() != 0 {
		t.Errorf("upstream calls = %d/%d, want 0/0", first.calls(), second.calls())
	}
	for i, inst := range handler.byProtocol[protocolOpenAI] {
		if got := instFailureCount(inst); got != 0 {
			t.Errorf("instance %d consecutiveFailures = %d, want 0", i, got)
		}
		if deadline := inst.cbDeadline(); !deadline.IsZero() {
			t.Errorf("instance %d degradedUntil = %s, want zero", i, deadline)
		}
	}
}

// TestOpenInstancesTriedAfterClosedCandidates covers M15bh: three closed
// instances fail first, then the two open instances are tried in configuration
// order.
func TestOpenInstancesTriedAfterClosedCandidates(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	var backends []*cbBackend
	for i := 0; i < 5; i++ {
		backend := newCBBackend(t, http.StatusOK)
		backend.setFail(true)
		backends = append(backends, backend)
	}
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(backends[0], "a", "gpt-x"),
		cbOpenAIInstance(backends[1], "b", "gpt-x"),
		cbOpenAIInstance(backends[2], "c", "gpt-x"),
		cbOpenAIInstance(backends[3], "d", "gpt-x"),
		cbOpenAIInstance(backends[4], "e", "gpt-x"),
	)
	insts := handler.byProtocol[protocolOpenAI]
	handler.recordFailure(insts[3])
	handler.recordFailure(insts[4])
	for _, i := range []int{3, 4} {
		if state := insts[i].state(); state != stateOpen {
			t.Fatalf("instance %d state = %q, want open", i, state)
		}
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	if status, _ := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`, true); status != http.StatusBadGateway {
		t.Fatalf("client status = %d, want 502", status)
	}
	for i, backend := range backends {
		if got := backend.calls(); got != 1 {
			t.Errorf("instance %d calls = %d, want 1", i, got)
		}
	}
	openD := backends[3].last().at
	openE := backends[4].last().at
	if openE.Before(openD) {
		t.Errorf("open instances were tried out of order: d=%s e=%s", openD, openE)
	}
}

// TestLastUpstream400ForwardedToClient covers M15bi: when every instance fails
// and the final failure is a real 400 (here from the fallback round), the
// client receives that status and body rather than a 502.
func TestLastUpstream400ForwardedToClient(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusInternalServerError)
	second := newCBBackend(t, http.StatusOK)
	second.setFail(true)
	last := newCBBackend(t, http.StatusBadRequest)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
		cbOpenAIInstance(last, "last", "gpt-x"),
	)
	handler.recordFailure(handler.byProtocol[protocolOpenAI][2])
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`, true)
	if status != http.StatusBadRequest {
		t.Fatalf("client status = %d, want 400", status)
	}
	if body != `{"id":"ok"}` {
		t.Errorf("client body = %q, want the original upstream 400 body", body)
	}
	if first.calls() != 1 || second.calls() != 1 || last.calls() != 1 {
		t.Errorf("calls = %d/%d/%d, want 1/1/1", first.calls(), second.calls(), last.calls())
	}
}

// TestAllConnectionFailuresReturn502 covers M15bj: only when every candidate
// fails before a response head does the client get the generic 502.
func TestAllConnectionFailuresReturn502(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusOK)
	first.setFail(true)
	second := newCBBackend(t, http.StatusOK)
	second.setFail(true)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`, true)
	if status != http.StatusBadGateway {
		t.Fatalf("client status = %d, want 502", status)
	}
	if !strings.Contains(body, "upstream request failed") {
		t.Errorf("client body = %q, want the generic upstream failure message", body)
	}
}

// TestFallbackUsesInstanceMappedModel covers M15bk: the fallback round sends
// the instance's own mapped model, never the client's original model name.
func TestFallbackUsesInstanceMappedModel(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusOK)
	first.setFail(true)
	second := newCBBackend(t, http.StatusOK)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbAnthropicInstance(first, "first", map[string]string{"sonnet": "model-a"}, 2*time.Second),
		cbAnthropicInstance(second, "second", map[string]string{"opus": "model-opus"}, 2*time.Second),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
	status, _ := sendProxyRequest(t, server.URL, protocolAnthropic, body, true)
	if status != http.StatusOK {
		t.Fatalf("client status = %d, want 200 (fallback instance served the request)", status)
	}
	if second.calls() != 1 {
		t.Fatalf("fallback instance calls = %d, want 1", second.calls())
	}
	var firstPayload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(first.last().body), &firstPayload); err != nil {
		t.Fatalf("first-round body is not JSON: %v", err)
	}
	if firstPayload.Model != "model-a" {
		t.Errorf("first-round model = %q, want the first instance's mapped model model-a", firstPayload.Model)
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(second.last().body), &payload); err != nil {
		t.Fatalf("fallback body is not JSON: %v", err)
	}
	if payload.Model != "model-opus" {
		t.Errorf("fallback model = %q, want the second instance's mapped model model-opus", payload.Model)
	}
	if payload.Model == "claude-sonnet-4-5" {
		t.Error("fallback reused the client's original model name")
	}
}

// TestFiveInstancesCalledAtMostOnce covers M15bl: across the first round and
// the fallback round a five-instance protocol never calls an instance twice,
// and a first round that already covered every instance produces no fallback.
func TestFiveInstancesCalledAtMostOnce(t *testing.T) {
	newBackends := func(t *testing.T) ([]*cbBackend, []config.Instance) {
		t.Helper()
		backends := make([]*cbBackend, 5)
		instances := make([]config.Instance, 5)
		for i := range backends {
			backends[i] = newCBBackend(t, http.StatusOK)
			backends[i].setFail(true)
			instances[i] = cbOpenAIInstance(backends[i], fmt.Sprintf("i%d", i), "gpt-x")
		}
		return backends, instances
	}
	assertOneCallEach := func(t *testing.T, backends []*cbBackend) {
		t.Helper()
		for i, backend := range backends {
			if got := backend.calls(); got != 1 {
				t.Errorf("instance %d calls = %d, want 1", i, got)
			}
		}
	}

	t.Run("fallback round completes the open instances", func(t *testing.T) {
		clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
		backends, instances := newBackends(t)
		handler := newCBHandler(t, clock, 1, time.Minute, instances...)
		insts := handler.byProtocol[protocolOpenAI]
		handler.recordFailure(insts[3])
		handler.recordFailure(insts[4])
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)

		if status, _ := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`, true); status != http.StatusBadGateway {
			t.Fatalf("client status = %d, want 502", status)
		}
		assertOneCallEach(t, backends)
	})

	t.Run("a chain covering every instance produces no fallback", func(t *testing.T) {
		clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
		backends, instances := newBackends(t)
		handler := newCBHandler(t, clock, 1, time.Minute, instances...)
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)

		if status, _ := sendProxyRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`, true); status != http.StatusBadGateway {
			t.Fatalf("client status = %d, want 502", status)
		}
		assertOneCallEach(t, backends)
	})
}

// TestFallbackFailuresStillCount covers M15bm: failures recorded inside the
// fallback round still increase the instance failure counter.
func TestFallbackFailuresStillCount(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusOK)
	first.setFail(true)
	second := newCBBackend(t, http.StatusOK)
	second.setFail(true)
	third := newCBBackend(t, http.StatusOK)
	third.setFail(true)
	handler := newCBHandler(t, clock, 3, time.Minute,
		cbAnthropicInstance(first, "first", map[string]string{"sonnet": "model-a"}, 2*time.Second),
		cbAnthropicInstance(second, "second", map[string]string{"sonnet": "model-b"}, 2*time.Second),
		cbAnthropicInstance(third, "third", map[string]string{"opus": "model-c"}, 2*time.Second),
	)
	insts := handler.byProtocol[protocolAnthropic]
	for i := 0; i < 3; i++ {
		handler.recordFailure(insts[1])
	}
	if state := insts[1].state(); state != stateOpen {
		t.Fatalf("second state = %q, want open", state)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	if status, _ := sendProxyRequest(t, server.URL, protocolAnthropic,
		`{"model":"claude-sonnet-4-5","messages":[]}`, true); status != http.StatusBadGateway {
		t.Fatalf("client status = %d, want 502", status)
	}
	if got := instFailureCount(insts[1]); got != 4 {
		t.Errorf("open chain instance failures = %d, want 4 (3 setup + 1 fallback)", got)
	}
	if got := instFailureCount(insts[2]); got != 1 {
		t.Errorf("chain-external instance failures = %d, want 1", got)
	}
}

// TestRetryReasonUpstreamStatus covers M54: a non-2xx upstream response maps to
// the upstream_status retry reason, and the fallback round constant is exposed
// for M60's fallback_retry.
func TestRetryReasonUpstreamStatus(t *testing.T) {
	if retryReasonUpstreamStatus != "upstream_status" {
		t.Errorf("retryReasonUpstreamStatus = %q, want upstream_status", retryReasonUpstreamStatus)
	}
	if retryReasonFallbackRetry != "fallback_retry" {
		t.Errorf("retryReasonFallbackRetry = %q, want fallback_retry", retryReasonFallbackRetry)
	}
	err := &upstreamResponseError{status: http.StatusBadRequest}
	if got := retryReason(err); got != retryReasonUpstreamStatus {
		t.Errorf("retryReason(upstream 400) = %q, want %q", got, retryReasonUpstreamStatus)
	}
	if err.Error() == "" {
		t.Error("upstreamResponseError.Error() is empty, want a status description")
	}
}
