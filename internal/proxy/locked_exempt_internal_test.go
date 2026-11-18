package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// §24.25 M15cq–M15dg: locked-instance timeout exemption, pre-content handoff
// from non-locked streams, and the shared mapping resolver.

func lxEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func lxBoolPtr(value bool) *bool { return &value }

func lxSSEHead(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	return flusher
}

func lxAnthropicInstance(url, name string, timeout time.Duration, roles map[string]string) config.Instance {
	return config.Instance{
		Protocol: config.ProtocolAnthropic,
		Name:     name,
		BaseURL:  url,
		APIKey:   "key-" + name,
		Timeout:  config.Duration(timeout),
		Roles:    roles,
	}
}

func lxLock(t *testing.T, handler *Handler, proto protocol, name string) {
	t.Helper()
	if _, ok := handler.protocolRuntime(proto).lockInstance(name); !ok {
		t.Fatalf("lock %q failed", name)
	}
}

func lxState(handler *Handler, proto protocol, index int) (failures int, degradedUntil time.Time) {
	inst := handler.byProtocol[proto][index]
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.consecutiveFailures, inst.degradedUntil
}

// lxPacedSSE writes one complete event per interval so the stream outlives the
// instance timeout without ever producing content.
func lxPacedSSE(t *testing.T, payload string, interval time.Duration) *httptest.Server {
	t.Helper()
	events := strings.SplitAfter(payload, "\n\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		flusher := lxSSEHead(w)
		for _, event := range events {
			if event == "" {
				continue
			}
			if _, err := io.WriteString(w, event); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(interval)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func lxLockedContentPayload(marker string) string {
	return lxEvent("message_start",
		`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`) +
		lxEvent("content_block_start",
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		lxEvent("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+marker+`"}}`) +
		lxEvent("message_stop", `{"type":"message_stop"}`)
}

// TestLockedStreamExemptFromTimeouts covers M15cq/M15cr: a locked stream that
// only sends pings runs past 3x the instance timeout, is delivered byte-for-
// byte, gets no pause_turn tail, and logs the locked exemption instead of the
// content-idle timeout.
func TestLockedStreamExemptFromTimeouts(t *testing.T) {
	var payload strings.Builder
	payload.WriteString(lxEvent("message_start",
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	for i := 0; i < 20; i++ {
		payload.WriteString(lxEvent("ping", `{"type":"ping"}`))
	}
	payload.WriteString(lxEvent("message_stop", `{"type":"message_stop"}`))
	want := payload.String()

	upstream := lxPacedSSE(t, want, 20*time.Millisecond)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(upstream.URL, "locked-inst", 50*time.Millisecond, map[string]string{"sonnet": "model-a"}))
	lxLock(t, handler, protocolAnthropic, "locked-inst")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	start := time.Now()
	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("stream ended after %s, want it to outlive 3x the 50ms timeout", elapsed)
	}
	if body != want {
		t.Errorf("client body differs from upstream bytes (got %d, want %d)", len(body), len(want))
	}
	if strings.Contains(body, "pause_turn") {
		t.Errorf("locked stream was interrupted with pause_turn:\n%s", body)
	}
	output := logs()
	if strings.Contains(output, "timeout: content-idle") {
		t.Errorf("locked stream logged the content-idle timeout:\n%s", output)
	}
	if !strings.Contains(output, "instance locked-inst locked; stream not interrupted") {
		t.Errorf("missing locked exemption log:\n%s", output)
	}
}

// TestLockedNoResponseHeaderTimeout covers M15ct: the locked client has no
// response-header timeout, so the gateway keeps waiting well past the instance
// timeout instead of disconnecting.
func TestLockedNoResponseHeaderTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFn)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	handler := newStreamHandler(t, Options{}, lxAnthropicInstance(upstream.URL, "locked-inst", 50*time.Millisecond, map[string]string{"sonnet": "model-a"}))
	lxLock(t, handler, protocolAnthropic, "locked-inst")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, server.URL+PathAnthropic, strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
		if err != nil {
			done <- result{err: err}
			return
		}
		req.Header.Set("x-api-key", "tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		_, readErr := io.ReadAll(resp.Body)
		done <- result{status: resp.StatusCode, err: readErr}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the request")
	}
	select {
	case got := <-done:
		t.Fatalf("gateway returned before the upstream sent headers: %+v", got)
	case <-time.After(300 * time.Millisecond):
	}
	output := logs()
	if strings.Contains(output, "timeout awaiting response headers") {
		t.Errorf("locked request hit the response-header timeout:\n%s", output)
	}
	if strings.Contains(output, "instance locked-inst timeout") {
		t.Errorf("locked request logged an instance timeout:\n%s", output)
	}

	releaseFn()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("request error: %v", got.err)
		}
		if got.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", got.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish after releasing the upstream")
	}
}

type lxJSONBackend struct {
	server   *httptest.Server
	status   int
	response string
	mu       sync.Mutex
	bodies   []string
}

func newLXJSONBackend(t *testing.T, status int, response string) *lxJSONBackend {
	t.Helper()
	b := &lxJSONBackend{status: status, response: response}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, string(body))
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(b.status)
		_, _ = io.WriteString(w, b.response)
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *lxJSONBackend) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.bodies)
}

func (b *lxJSONBackend) lastBody() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.bodies) == 0 {
		return ""
	}
	return b.bodies[len(b.bodies)-1]
}

// TestLockedFailureNotCountedAndFallsThrough covers M15cs/M15cu: a locked 400
// is not counted, the peer is tried next, and it was the locked instance that
// received the first request.
func TestLockedFailureNotCountedAndFallsThrough(t *testing.T) {
	locked := newLXJSONBackend(t, http.StatusBadRequest, `{"type":"error","error":{"message":"locked"}}`)
	peer := newLXJSONBackend(t, http.StatusOK, `{"ok":true}`)
	handler := newStreamHandler(t, Options{},
		lxAnthropicInstance(locked.server.URL, "locked", 5*time.Second, map[string]string{"sonnet": "model-a"}),
		lxAnthropicInstance(peer.server.URL, "peer", 5*time.Second, map[string]string{"sonnet": "model-b"}),
	)
	lxLock(t, handler, protocolAnthropic, "locked")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("response = %d %q, want 200 from the peer", status, body)
	}
	if locked.calls() != 1 || peer.calls() != 1 {
		t.Errorf("calls = %d/%d, want 1/1", locked.calls(), peer.calls())
	}
	failures, degradedUntil := lxState(handler, protocolAnthropic, 0)
	if failures != 0 || !degradedUntil.IsZero() {
		t.Errorf("locked state = failures %d degradedUntil %v, want 0/zero (M92)", failures, degradedUntil)
	}
}

// TestLockedFailureAllPeersFailForwardsLast covers M15da: when every peer also
// fails, the final pinned retry is the last real upstream response and is
// forwarded verbatim.
func TestLockedFailureAllPeersFailForwardsLast(t *testing.T) {
	lockedBody := `{"type":"error","error":{"message":"locked-400"}}`
	locked := newLXJSONBackend(t, http.StatusBadRequest, lockedBody)
	peer := newLXJSONBackend(t, http.StatusBadRequest, `{"type":"error","error":{"message":"peer-400"}}`)
	handler := newStreamHandler(t, Options{},
		lxAnthropicInstance(locked.server.URL, "locked", 5*time.Second, map[string]string{"sonnet": "model-a"}),
		lxAnthropicInstance(peer.server.URL, "peer", 5*time.Second, map[string]string{"sonnet": "model-b"}),
	)
	lxLock(t, handler, protocolAnthropic, "locked")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusBadRequest || body != lockedBody {
		t.Fatalf("response = %d %q, want the last real 400 %q", status, body, lockedBody)
	}
	if locked.calls() != 2 || peer.calls() != 1 {
		t.Errorf("calls = %d/%d, want 2/1 (pin, peer, pinned handoff)", locked.calls(), peer.calls())
	}
	if failures, _ := lxState(handler, protocolAnthropic, 0); failures != 0 {
		t.Errorf("locked failures = %d, want 0 (M92)", failures)
	}
	if failures, _ := lxState(handler, protocolAnthropic, 1); failures != 1 {
		t.Errorf("peer failures = %d, want 1 (only the pin is exempt)", failures)
	}
}

// TestLockedSuccessSkipsPeers covers M15cv.
func TestLockedSuccessSkipsPeers(t *testing.T) {
	locked := newLXJSONBackend(t, http.StatusOK, `{"ok":true}`)
	peer := newLXJSONBackend(t, http.StatusOK, `{"peer":true}`)
	handler := newStreamHandler(t, Options{},
		lxAnthropicInstance(locked.server.URL, "locked", 5*time.Second, map[string]string{"sonnet": "model-a"}),
		lxAnthropicInstance(peer.server.URL, "peer", 5*time.Second, map[string]string{"sonnet": "model-b"}),
	)
	lxLock(t, handler, protocolAnthropic, "locked")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	if status, _, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`); err != nil || status != http.StatusOK {
		t.Fatalf("request = %d, %v; want 200", status, err)
	}
	if locked.calls() != 1 || peer.calls() != 0 {
		t.Errorf("calls = %d/%d, want 1/0 (pin wins)", locked.calls(), peer.calls())
	}
}

// TestUnlockClearsLockedPeriodCounters covers M15cw: only the pinned instance
// is exempt while locked; unlock clears every counter without retroactive
// accounting.
func TestUnlockClearsLockedPeriodCounters(t *testing.T) {
	locked := newLXJSONBackend(t, http.StatusBadRequest, `{"type":"error","error":{"message":"locked"}}`)
	peer := newLXJSONBackend(t, http.StatusBadRequest, `{"type":"error","error":{"message":"peer"}}`)
	handler := newStreamHandler(t, Options{FailureThreshold: 1},
		lxAnthropicInstance(locked.server.URL, "locked", 5*time.Second, map[string]string{"sonnet": "model-a"}),
		lxAnthropicInstance(peer.server.URL, "peer", 5*time.Second, map[string]string{"sonnet": "model-b"}),
	)
	lxLock(t, handler, protocolAnthropic, "locked")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	if status, _, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`); err != nil || status != http.StatusBadRequest {
		t.Fatalf("request = %d, %v; want 400", status, err)
	}
	if failures, _ := lxState(handler, protocolAnthropic, 0); failures != 0 {
		t.Errorf("locked failures = %d, want 0", failures)
	}
	if failures, _ := lxState(handler, protocolAnthropic, 1); failures != 1 {
		t.Errorf("peer failures = %d, want 1", failures)
	}

	if !handler.protocolRuntime(protocolAnthropic).unlock() {
		t.Fatal("unlock returned false")
	}
	for i := range handler.byProtocol[protocolAnthropic] {
		failures, degradedUntil := lxState(handler, protocolAnthropic, i)
		if failures != 0 || !degradedUntil.IsZero() {
			t.Errorf("instance %d after unlock = failures %d degradedUntil %v, want 0/zero", i, failures, degradedUntil)
		}
	}
}

// TestLockedUsesMappedModel covers M15cx: the pinned candidate rewrites
// claude-sonnet-5 to the instance's sonnet mapping.
func TestLockedUsesMappedModel(t *testing.T) {
	locked := newLXJSONBackend(t, http.StatusOK, `{"ok":true}`)
	handler := newStreamHandler(t, Options{},
		lxAnthropicInstance(locked.server.URL, "locked", 5*time.Second, map[string]string{"sonnet": "deepseek-flash"}))
	lxLock(t, handler, protocolAnthropic, "locked")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	if status, _, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`); err != nil || status != http.StatusOK {
		t.Fatalf("request = %d, %v; want 200", status, err)
	}
	model, ok := requestModel([]byte(locked.lastBody()))
	if !ok || model != "deepseek-flash" {
		t.Errorf("upstream model = %q (usable=%v), want deepseek-flash; body %s", model, ok, locked.lastBody())
	}
}

// TestLockedAndFallbackMappingAgree covers M15cy: both candidates resolve the
// same model through the shared resolver for every role, with and without
// capability declarations.
func TestLockedAndFallbackMappingAgree(t *testing.T) {
	roleCases := []struct {
		name    string
		roles   map[string]string
		request string
		want    string
	}{
		{"wildcard", map[string]string{"*": "wild-model"}, "claude-sonnet-5", "wild-model"},
		{"sonnet", map[string]string{"sonnet": "son-model"}, "claude-sonnet-5", "son-model"},
		{"fable", map[string]string{"fable": "fab-model"}, "claude-fable-5", "fab-model"},
		{"opus", map[string]string{"opus": "opu-model"}, "claude-opus-5", "opu-model"},
		{"haiku", map[string]string{"haiku": "hai-model"}, "claude-haiku-4-5", "hai-model"},
	}
	for _, tc := range roleCases {
		for _, withCapabilities := range []bool{false, true} {
			name := tc.name
			if withCapabilities {
				name += "-with-capabilities"
			}
			t.Run(name, func(t *testing.T) {
				inst := &instanceRuntime{
					name:  "mapped",
					proto: protocolAnthropic,
					roles: tc.roles,
				}
				if withCapabilities {
					inst.capabilities = map[string]config.ModelCapability{
						tc.want: {MaxInputTokens: 1000, Thinking: lxBoolPtr(true)},
					}
				}
				body := []byte(fmt.Sprintf(`{"model":%q,"messages":[]}`, tc.request))
				locked := lockedCandidate(protocolAnthropic, []*instanceRuntime{inst}, body, 0)
				fallback := fallbackCandidate(protocolAnthropic, inst, body)
				if locked == nil || fallback == nil {
					t.Fatalf("candidates = %v/%v, want both resolved", locked, fallback)
				}
				if locked.model != fallback.model || locked.model != tc.want {
					t.Errorf("models = locked %q fallback %q, want %q", locked.model, fallback.model, tc.want)
				}
				if locked.rewrite != fallback.rewrite {
					t.Errorf("rewrite = locked %v fallback %v, want equal", locked.rewrite, fallback.rewrite)
				}
			})
		}
	}

	t.Run("exact-model", func(t *testing.T) {
		inst := &instanceRuntime{name: "exact", proto: protocolAnthropic, models: []string{"claude-sonnet-5"}}
		body := []byte(`{"model":"claude-sonnet-5","messages":[]}`)
		locked := lockedCandidate(protocolAnthropic, []*instanceRuntime{inst}, body, 0)
		fallback := fallbackCandidate(protocolAnthropic, inst, body)
		if locked == nil || fallback == nil || locked.model != "claude-sonnet-5" || locked.model != fallback.model {
			t.Fatalf("exact candidates = %+v / %+v", locked, fallback)
		}
		if locked.rewrite || fallback.rewrite {
			t.Errorf("exact match must not rewrite: locked %v fallback %v", locked.rewrite, fallback.rewrite)
		}
	})

	t.Run("no-mapping", func(t *testing.T) {
		inst := &instanceRuntime{name: "empty", proto: protocolAnthropic}
		body := []byte(`{"model":"claude-sonnet-5","messages":[]}`)
		locked := lockedCandidate(protocolAnthropic, []*instanceRuntime{inst}, body, 0)
		fallback := fallbackCandidate(protocolAnthropic, inst, body)
		if fallback != nil {
			t.Errorf("fallbackCandidate = %+v, want nil", fallback)
		}
		if locked == nil || locked.model != "claude-sonnet-5" || locked.rewrite {
			t.Errorf("lockedCandidate = %+v, want the original model without rewrite", locked)
		}
	})
}

type lxHandoffResult struct {
	body        string
	status      int
	err         error
	elapsed     time.Duration
	lockedCalls int32
	peerCalls   int32
	peerCancel  bool
}

// lxRunNoContentHandoff pins "locked" and makes its first attempt fail with
// 400; the peer then streams peerPayload without content and must be aborted
// before any client byte, after which the pinned instance succeeds.
func lxRunNoContentHandoff(t *testing.T, peerPayload string, peerTimeout time.Duration) lxHandoffResult {
	t.Helper()
	lockedPayload := lxLockedContentPayload("LOCKED-OK")
	var lockedCalls atomic.Int32
	locked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := lockedCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"message":"locked first"}}`)
			return
		}
		flusher := lxSSEHead(w)
		_, _ = io.WriteString(w, lockedPayload)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(locked.Close)

	var peerCalls atomic.Int32
	var peerCancel atomic.Bool
	peerDone := make(chan struct{}, 1)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		flusher := lxSSEHead(w)
		_, _ = io.WriteString(w, peerPayload)
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			peerCancel.Store(true)
		case <-time.After(5 * time.Second):
		}
		peerDone <- struct{}{}
	}))
	t.Cleanup(peer.Close)

	handler := newStreamHandler(t, Options{
		IdleTimeout:       30 * time.Second,
		FirstTokenTimeout: 30 * time.Second,
	},
		lxAnthropicInstance(locked.URL, "locked", 5*time.Second, map[string]string{"sonnet": "model-a"}),
		lxAnthropicInstance(peer.URL, "peer", peerTimeout, map[string]string{"sonnet": "model-b"}),
	)
	lxLock(t, handler, protocolAnthropic, "locked")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	start := time.Now()
	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`)
	elapsed := time.Since(start)
	select {
	case <-peerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("peer upstream was not closed after the handoff")
	}
	return lxHandoffResult{
		body:        body,
		status:      status,
		err:         err,
		elapsed:     elapsed,
		lockedCalls: lockedCalls.Load(),
		peerCalls:   peerCalls.Load(),
		peerCancel:  peerCancel.Load(),
	}
}

// TestLockedHandoffBeforeContent covers M15db/M15dc: the peer only sends
// non-content events, is closed immediately without waiting for its timeout,
// and the pinned instance serves the client.
func TestLockedHandoffBeforeContent(t *testing.T) {
	peerPayload := lxEvent("message_start",
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`) +
		lxEvent("ping", `{"type":"ping"}`)
	// §24.25 M15dc: the peer's own timeout is 10 minutes; the handoff must not
	// wait for it.
	got := lxRunNoContentHandoff(t, peerPayload, 10*time.Minute)
	if got.err != nil {
		t.Fatalf("read body: %v", got.err)
	}
	if got.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.status)
	}
	want := lxLockedContentPayload("LOCKED-OK")
	if got.body != want {
		t.Errorf("client body = %q, want the locked payload only %q", got.body, want)
	}
	if got.elapsed > 2*time.Second {
		t.Errorf("handoff took %s, want it to ignore the 10m peer timeout", got.elapsed)
	}
	if got.lockedCalls != 2 || got.peerCalls != 1 {
		t.Errorf("calls = locked %d peer %d, want 2/1", got.lockedCalls, got.peerCalls)
	}
	if !got.peerCancel {
		t.Error("peer upstream request was not cancelled")
	}
}

// TestLockedHandoffHappensOnce covers M94d: after a no-content peer is
// aborted, the pinned instance is tried once; if it fails too, the request does
// not return to the peer or retry the pin again.
func TestLockedHandoffHappensOnce(t *testing.T) {
	lockedBody := `{"type":"error","error":{"message":"locked-400"}}`
	var lockedCalls atomic.Int32
	locked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lockedCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, lockedBody)
	}))
	t.Cleanup(locked.Close)

	var peerCalls atomic.Int32
	peerDone := make(chan struct{}, 1)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		flusher := lxSSEHead(w)
		_, _ = io.WriteString(w, lxEvent("ping", `{"type":"ping"}`))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		peerDone <- struct{}{}
	}))
	t.Cleanup(peer.Close)

	handler := newStreamHandler(t, Options{
		IdleTimeout:       30 * time.Second,
		FirstTokenTimeout: 30 * time.Second,
	},
		lxAnthropicInstance(locked.URL, "locked", 5*time.Second, map[string]string{"sonnet": "model-a"}),
		lxAnthropicInstance(peer.URL, "peer", 5*time.Second, map[string]string{"sonnet": "model-b"}),
	)
	lxLock(t, handler, protocolAnthropic, "locked")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	select {
	case <-peerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("peer upstream was not closed")
	}
	if status != http.StatusBadRequest || body != lockedBody {
		t.Fatalf("response = %d %q, want the pinned 400 %q", status, body, lockedBody)
	}
	if lockedCalls.Load() != 2 || peerCalls.Load() != 1 {
		t.Errorf("calls = locked %d peer %d, want 2/1 (one handoff, no loop)",
			lockedCalls.Load(), peerCalls.Load())
	}
}

// TestLockedHandoffIgnoresNonContentEvents covers M15dg and M94a: pings,
// message_start, content_block_start, empty deltas and signature_delta do not
// count as content, so every variant still hands off.
func TestLockedHandoffIgnoresNonContentEvents(t *testing.T) {
	cases := map[string]string{
		"ping":                 lxEvent("ping", `{"type":"ping"}`),
		"message_start":        lxEvent("message_start", `{"type":"message_start","message":{}}`),
		"content_block_start":  lxEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		"empty_text_delta":     lxEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`),
		"empty_thinking_delta": lxEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`),
		"signature_delta":      lxEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}`),
	}
	for name, payload := range cases {
		name, payload := name, payload
		t.Run(name, func(t *testing.T) {
			got := lxRunNoContentHandoff(t, payload, 5*time.Second)
			if got.err != nil || got.status != http.StatusOK {
				t.Fatalf("response = %d, %v; want 200", got.status, got.err)
			}
			if got.body != lxLockedContentPayload("LOCKED-OK") {
				t.Errorf("client body = %q, want the locked payload", got.body)
			}
			if got.lockedCalls != 2 || got.peerCalls != 1 || !got.peerCancel {
				t.Errorf("handoff = locked %d peer %d cancel %v, want 2/1/true",
					got.lockedCalls, got.peerCalls, got.peerCancel)
			}
		})
	}
}

// TestLockedHandoffAfterContentDoesNotInterrupt covers M15dd/M94: once a real
// content block arrived, silence does not hand the request back.
func TestLockedHandoffAfterContentDoesNotInterrupt(t *testing.T) {
	lockedPayload := lxLockedContentPayload("LOCKED-OK")
	var lockedCalls atomic.Int32
	locked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := lockedCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"message":"locked first"}}`)
			return
		}
		flusher := lxSSEHead(w)
		_, _ = io.WriteString(w, lockedPayload)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(locked.Close)

	first := lxEvent("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"PEER-OK"}}`)
	rest := lxEvent("ping", `{"type":"ping"}`) +
		lxEvent("ping", `{"type":"ping"}`) +
		lxEvent("message_stop", `{"type":"message_stop"}`)
	peerPayload := first + rest

	var peerCalls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		flusher := lxSSEHead(w)
		_, _ = io.WriteString(w, first)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, rest)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(peer.Close)

	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	},
		lxAnthropicInstance(locked.URL, "locked", 5*time.Second, map[string]string{"sonnet": "model-a"}),
		lxAnthropicInstance(peer.URL, "peer", 5*time.Second, map[string]string{"sonnet": "model-b"}),
	)
	lxLock(t, handler, protocolAnthropic, "locked")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body != peerPayload {
		t.Errorf("client body = %q, want the peer stream %q", body, peerPayload)
	}
	if lockedCalls.Load() != 1 || peerCalls.Load() != 1 {
		t.Errorf("calls = locked %d peer %d, want 1/1 (no handoff after content)",
			lockedCalls.Load(), peerCalls.Load())
	}
}

// TestLockedHandoffOnPeerFirstByteFailure covers M15de: a peer that fails at
// the connection/status stage is followed by one final pinned attempt, and the
// client only sees the pinned response.
func TestLockedHandoffOnPeerFirstByteFailure(t *testing.T) {
	lockedPayload := lxLockedContentPayload("LOCKED-OK")
	var lockedCalls atomic.Int32
	locked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := lockedCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"message":"locked first"}}`)
			return
		}
		flusher := lxSSEHead(w)
		_, _ = io.WriteString(w, lockedPayload)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(locked.Close)

	peer := newLXJSONBackend(t, http.StatusBadRequest, `{"type":"error","error":{"message":"peer first byte"}}`)
	handler := newStreamHandler(t, Options{},
		lxAnthropicInstance(locked.URL, "locked", 5*time.Second, map[string]string{"sonnet": "model-a"}),
		lxAnthropicInstance(peer.server.URL, "peer", 5*time.Second, map[string]string{"sonnet": "model-b"}),
	)
	lxLock(t, handler, protocolAnthropic, "locked")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK || body != lockedPayload {
		t.Fatalf("response = %d %q, want the pinned stream %q", status, body, lockedPayload)
	}
	if lockedCalls.Load() != 2 || peer.calls() != 1 {
		t.Errorf("calls = locked %d peer %d, want 2/1", lockedCalls.Load(), peer.calls())
	}
}

// TestNoLockStreamBytesUnchanged covers M15df: without a pin, the stream is
// forwarded byte-for-byte exactly as before.
func TestNoLockStreamBytesUnchanged(t *testing.T) {
	payload := lxEvent("message_start",
		`{"type":"message_start","message":{"usage":{"input_tokens":2}}}`) +
		lxEvent("content_block_start",
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		lxEvent("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`) +
		lxEvent("ping", `{"type":"ping"}`) +
		lxEvent("message_stop", `{"type":"message_stop"}`)
	upstream := lxPacedSSE(t, payload, 0)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(upstream.URL, "plain", 5*time.Second, map[string]string{"sonnet": "model-a"}))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"claude-sonnet-5","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body != payload {
		t.Errorf("unlocked stream changed:\n got: %q\nwant: %q", body, payload)
	}
}
