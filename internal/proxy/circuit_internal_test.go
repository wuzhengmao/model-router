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
	"model-router/internal/web"
)

// cbClock is a fake clock for the §24.17 breaker tests.
type cbClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *cbClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *cbClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type cbRequest struct {
	method string
	path   string
	body   string
	header http.Header
	at     time.Time
}

// cbBackend is a controllable upstream used by the circuit-breaker tests.
type cbBackend struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []cbRequest
	status   atomic.Int32
	fail     atomic.Bool
	delay    time.Duration

	// truncate answers 200 with a body shorter than its Content-Length: headers
	// commit, then the connection dies mid-body.
	truncate atomic.Bool
}

func newCBBackend(t *testing.T, status int) *cbBackend {
	t.Helper()
	b := &cbBackend{}
	b.status.Store(int32(status))
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.requests = append(b.requests, cbRequest{
			method: r.Method,
			path:   r.URL.Path,
			body:   string(body),
			header: r.Header.Clone(),
			at:     time.Now(),
		})
		delay := b.delay
		b.mu.Unlock()

		if b.fail.Load() {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				panic(http.ErrAbortHandler)
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				panic(http.ErrAbortHandler)
			}
			_ = conn.Close()
			return
		}
		if b.truncate.Load() {
			// 200 with a Content-Length longer than the body: the response is
			// committed, then the read fails. The proxy records a failed request.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				panic(http.ErrAbortHandler)
			}
			conn, buf, err := hijacker.Hijack()
			if err != nil {
				panic(http.ErrAbortHandler)
			}
			_, _ = io.WriteString(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\nConnection: close\r\n\r\n{\"id\":\"ok\"}")
			_ = buf.Flush()
			_ = conn.Close()
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(b.status.Load()))
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *cbBackend) setStatus(status int) { b.status.Store(int32(status)) }

func (b *cbBackend) setFail(fail bool) { b.fail.Store(fail) }

func (b *cbBackend) setTruncated(truncate bool) { b.truncate.Store(truncate) }

func (b *cbBackend) setDelay(delay time.Duration) {
	b.mu.Lock()
	b.delay = delay
	b.mu.Unlock()
}

func (b *cbBackend) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}

func (b *cbBackend) last() cbRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		return cbRequest{}
	}
	return b.requests[len(b.requests)-1]
}

func cbOpenAIInstance(backend *cbBackend, name string, models ...string) config.Instance {
	return config.Instance{
		Protocol: config.ProtocolOpenAI,
		Name:     name,
		BaseURL:  backend.server.URL,
		APIKey:   "upstream-" + name,
		Timeout:  config.Duration(2 * time.Second),
		Models:   models,
	}
}

func cbAnthropicInstance(backend *cbBackend, name string, roles map[string]string, timeout time.Duration) config.Instance {
	return config.Instance{
		Protocol: config.ProtocolAnthropic,
		Name:     name,
		BaseURL:  backend.server.URL,
		APIKey:   "upstream-" + name,
		Timeout:  config.Duration(timeout),
		Roles:    roles,
	}
}

func newCBHandler(t *testing.T, clock *cbClock, threshold int, degrade time.Duration, instances ...config.Instance) *Handler {
	t.Helper()
	handler, err := NewWithOptions(&config.Config{
		AuthToken: "tok",
		Instances: instances,
	}, Options{
		Now:              clock.Now,
		FailureThreshold: threshold,
		DegradeDuration:  degrade,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	return handler
}

func cbPath(proto protocol) string {
	if proto == protocolAnthropic {
		return PathAnthropic
	}
	return PathOpenAI
}

func cbSend(t *testing.T, serverURL string, proto protocol, model string) int {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
	req, err := http.NewRequest(http.MethodPost, serverURL+cbPath(proto), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if proto == protocolAnthropic {
		req.Header.Set("x-api-key", "tok")
	} else {
		req.Header.Set("Authorization", "Bearer tok")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func (inst *instanceRuntime) cbDeadline() time.Time {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.degradedUntil
}

// TestFirstClosedRouting covers M15ag: A is open, B/C are closed, so requests
// go to B; once A's degradedUntil elapses, requests return to A without any
// probe or extra request.
func TestFirstClosedRouting(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusOK)
	first.setFail(true)
	second := newCBBackend(t, http.StatusOK)
	third := newCBBackend(t, http.StatusOK)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
		cbOpenAIInstance(third, "third", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// Request 1: first fails and opens, the same request falls through to second.
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", code)
	}
	if got := handler.byProtocol[protocolOpenAI][0].state(); got != stateOpen {
		t.Fatalf("first state = %q, want open", got)
	}
	if got := handler.byProtocol[protocolOpenAI][1].state(); got != stateClosed {
		t.Fatalf("second state = %q, want closed", got)
	}
	if first.calls() != 1 || second.calls() != 1 || third.calls() != 0 {
		t.Fatalf("calls = %d/%d/%d, want 1/1/0", first.calls(), second.calls(), third.calls())
	}

	// Request 2: first (open) is skipped, second is the first closed instance.
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("request 2 status = %d, want 200", code)
	}
	if got := second.calls(); got != 2 {
		t.Errorf("second calls = %d, want 2 (first closed instance)", got)
	}
	if got := third.calls(); got != 0 {
		t.Errorf("third calls = %d, want 0", got)
	}

	// The degrade window elapses without any request; first is closed again.
	first.setFail(false)
	clock.Advance(time.Minute + time.Second)
	if got := handler.byProtocol[protocolOpenAI][0].state(); got != stateClosed {
		t.Fatalf("first state after deadline = %q, want closed", got)
	}
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("request 3 status = %d, want 200", code)
	}
	if got := first.calls(); got != 2 {
		t.Errorf("first calls = %d, want 2 (recovered instance is routed again)", got)
	}
	if got := second.calls(); got != 2 {
		t.Errorf("second calls = %d, want 2 (no probe traffic)", got)
	}
}

// cbAllOpenThree drives three failing instances into the all-open state: the
// first request runs with every breaker closed, so each instance is tried once
// and opens (failure_threshold is 1 in these tests).
func cbAllOpenThree(t *testing.T) (*cbClock, []*cbBackend, *Handler, *httptest.Server) {
	t.Helper()
	clock := &cbClock{now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	backends := []*cbBackend{
		newCBBackend(t, http.StatusOK),
		newCBBackend(t, http.StatusOK),
		newCBBackend(t, http.StatusOK),
	}
	for _, backend := range backends {
		backend.setFail(true)
	}
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(backends[0], "first", "gpt-x"),
		cbOpenAIInstance(backends[1], "second", "gpt-x"),
		cbOpenAIInstance(backends[2], "third", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusBadGateway {
		t.Fatalf("setup request status = %d, want 502", code)
	}
	for i := range backends {
		if state := handler.byProtocol[protocolOpenAI][i].state(); state != stateOpen {
			t.Fatalf("setup instance %d state = %q, want open", i, state)
		}
	}
	return clock, backends, handler, server
}

// TestAllOpenRetriesEveryInstance covers M15av/M46: with every instance open the
// request walks the whole chain from index 0, so a failing head falls through to
// the next instance instead of ending the request.
func TestAllOpenRetriesEveryInstance(t *testing.T) {
	_, backends, handler, server := cbAllOpenThree(t)
	backends[1].setFail(false)

	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("all-open request status = %d, want 200", code)
	}
	if got := backends[0].calls(); got != 2 {
		t.Errorf("first calls = %d, want 2 (head retried while open)", got)
	}
	if got := backends[1].calls(); got != 2 {
		t.Errorf("second calls = %d, want 2 (tried after the head failed)", got)
	}
	if got := backends[2].calls(); got != 1 {
		t.Errorf("third calls = %d, want 1 (chain stopped at the successful instance)", got)
	}
	if state := handler.byProtocol[protocolOpenAI][1].state(); state != stateClosed {
		t.Errorf("second state = %q, want closed after its success", state)
	}
}

// TestAllOpenFirstSuccessNoPrefetch covers M15av-b: when the first instance of an
// all-open chain succeeds, no later instance receives a request.
func TestAllOpenFirstSuccessNoPrefetch(t *testing.T) {
	_, backends, handler, server := cbAllOpenThree(t)
	backends[0].setFail(false)

	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("all-open request status = %d, want 200", code)
	}
	if got := backends[0].calls(); got != 2 {
		t.Errorf("first calls = %d, want 2", got)
	}
	if got := backends[1].calls(); got != 1 {
		t.Errorf("second calls = %d, want 1 (no prefetch after the head succeeded)", got)
	}
	if got := backends[2].calls(); got != 1 {
		t.Errorf("third calls = %d, want 1 (no prefetch after the head succeeded)", got)
	}
	if state := handler.byProtocol[protocolOpenAI][0].state(); state != stateClosed {
		t.Errorf("first state = %q, want closed after its success", state)
	}
}

// TestAllOpenAllFail covers M15aw: an all-open request tries each instance
// exactly once and returns 502 once the chain is exhausted.
func TestAllOpenAllFail(t *testing.T) {
	_, backends, _, server := cbAllOpenThree(t)
	before := []int{backends[0].calls(), backends[1].calls(), backends[2].calls()}

	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusBadGateway {
		t.Fatalf("all-open request status = %d, want 502", code)
	}
	for i, backend := range backends {
		if got := backend.calls() - before[i]; got != 1 {
			t.Errorf("instance %d calls delta = %d, want 1", i, got)
		}
	}
}

// TestMixedClosedAndOpenSkipsOpen covers M15ax/M48: open candidates are still
// skipped — they receive no request — when closed candidates remain.
func TestMixedClosedAndOpenSkipsOpen(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusOK)
	first.setFail(true)
	second := newCBBackend(t, http.StatusOK)
	third := newCBBackend(t, http.StatusOK)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
		cbOpenAIInstance(third, "third", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// Request 1: first fails and opens; the closed second serves the request.
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", code)
	}
	if state := handler.byProtocol[protocolOpenAI][0].state(); state != stateOpen {
		t.Fatalf("first state = %q, want open", state)
	}

	// Request 2: first is open and skipped; second fails and falls through to
	// the closed third instance.
	second.setFail(true)
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("request 2 status = %d, want 200", code)
	}
	if got := first.calls(); got != 1 {
		t.Errorf("first calls = %d, want 1 (open instance skipped)", got)
	}
	if got := second.calls(); got != 2 {
		t.Errorf("second calls = %d, want 2", got)
	}
	if got := third.calls(); got != 1 {
		t.Errorf("third calls = %d, want 1", got)
	}
}

// TestAllOpenFailureBookkeeping covers M15ay: retrying an open candidate still
// records the failure and re-times its degradedUntil.
func TestAllOpenFailureBookkeeping(t *testing.T) {
	clock, backends, handler, server := cbAllOpenThree(t)
	openedAt := clock.Now()
	for i := range backends {
		if got := handler.byProtocol[protocolOpenAI][i].cbDeadline(); !got.Equal(openedAt.Add(time.Minute)) {
			t.Fatalf("setup instance %d degradedUntil = %s, want %s", i, got, openedAt.Add(time.Minute))
		}
	}

	clock.Advance(30 * time.Second)
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusBadGateway {
		t.Fatalf("all-open request status = %d, want 502", code)
	}
	want := clock.Now().Add(time.Minute)
	for i, backend := range backends {
		if got := handler.byProtocol[protocolOpenAI][i].cbDeadline(); !got.Equal(want) {
			t.Errorf("instance %d degradedUntil = %s, want %s (failure re-times the window)", i, got, want)
		}
		if got := backend.calls(); got != 2 {
			t.Errorf("instance %d calls = %d, want 2", i, got)
		}
	}
}

// TestFailureRetimesBackoff covers M15ai: a failure while already open resets
// degradedUntil to now+degrade_duration with no exponential growth.
func TestFailureRetimesBackoff(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
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
	openedAt := clock.Now()

	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusBadGateway {
		t.Fatalf("request 1 status = %d, want 502", code)
	}
	if got := handler.byProtocol[protocolOpenAI][0].cbDeadline(); !got.Equal(openedAt.Add(time.Minute)) {
		t.Fatalf("first degradedUntil = %s, want %s", got, openedAt.Add(time.Minute))
	}

	clock.Advance(59 * time.Second)
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusBadGateway {
		t.Fatalf("request 2 status = %d, want 502", code)
	}
	if got := first.calls(); got != 2 {
		t.Errorf("first calls = %d, want 2 (all-open retry starts at the head)", got)
	}
	want := clock.Now().Add(time.Minute)
	if got := handler.byProtocol[protocolOpenAI][0].cbDeadline(); !got.Equal(want) {
		t.Errorf("first degradedUntil = %s, want %s (reset, no exponential growth)", got, want)
	}
	// §24.19 M45–M48: the all-open retry chain reaches the second instance too,
	// so its failure re-times its own deadline to now + degrade_duration.
	if got := handler.byProtocol[protocolOpenAI][1].cbDeadline(); !got.Equal(want) {
		t.Errorf("second degradedUntil = %s, want %s (retried and re-timed)", got, want)
	}
	if got := second.calls(); got != 2 {
		t.Errorf("second calls = %d, want 2 (retried while open)", got)
	}
}

// TestIdleRecoveryWithoutRequests covers M15aj: the deadline alone closes the
// breaker; no request or probe is needed.
func TestIdleRecoveryWithoutRequests(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusOK)
	first.setFail(true)
	second := newCBBackend(t, http.StatusOK)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	ps := handler.protocolRuntime(protocolOpenAI)

	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", code)
	}
	if got := handler.byProtocol[protocolOpenAI][0].state(); got != stateOpen {
		t.Fatalf("first state = %q, want open", got)
	}

	// No request at all: advancing the clock past degradedUntil closes it.
	clock.Advance(time.Minute + time.Second)
	if got := handler.byProtocol[protocolOpenAI][0].state(); got != stateClosed {
		t.Fatalf("first state after idle deadline = %q, want closed", got)
	}
	if state, since, reason, _ := ps.stateFor(0); state != stateClosed || since != 0 || reason != "" {
		t.Errorf("display state after idle deadline = %q/%d/%q, want closed/0/empty", state, since, reason)
	}

	// The next request is served by the recovered first instance.
	first.setFail(false)
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("request 2 status = %d, want 200", code)
	}
	if got := first.calls(); got != 2 {
		t.Errorf("first calls = %d, want 2 (recovered head routed again)", got)
	}
	if got := second.calls(); got != 1 {
		t.Errorf("second calls = %d, want 1", got)
	}
}

// TestNoActiveProbing covers M15al: with every instance open, five minutes of
// clock time and no requests produce no upstream traffic and no statistics.
func TestNoActiveProbing(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusOK)
	first.setFail(true)
	second := newCBBackend(t, http.StatusOK)
	second.setFail(true)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	handler.monitor = newMonitorHub()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusBadGateway {
		t.Fatalf("request status = %d, want 502", code)
	}
	before := handler.monitor.stats.snapshot(0, time.Now())
	if before.Requests.Total != 1 {
		t.Fatalf("requests.total = %d, want 1", before.Requests.Total)
	}
	firstCalls, secondCalls := first.calls(), second.calls()

	clock.Advance(5 * time.Minute)
	if got := first.calls(); got != firstCalls {
		t.Errorf("first calls = %d, want %d (no background probe)", got, firstCalls)
	}
	if got := second.calls(); got != secondCalls {
		t.Errorf("second calls = %d, want %d (no background probe)", got, secondCalls)
	}
	after := handler.monitor.stats.snapshot(0, time.Now())
	if after.Requests.Total != before.Requests.Total {
		t.Errorf("requests.total = %d, want %d (internal behaviour must not count)",
			after.Requests.Total, before.Requests.Total)
	}
	if got := handler.byProtocol[protocolOpenAI][0].state(); got != stateClosed {
		t.Errorf("first state after idle deadline = %q, want closed (time-based recovery)", got)
	}
}

// TestLockRoutesEverythingAndUnlockResets covers M15ao.
func TestLockRoutesEverythingAndUnlockResets(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusOK)
	second := newCBBackend(t, http.StatusOK)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	ps := handler.protocolRuntime(protocolOpenAI)

	if _, ok := ps.lockInstance("second"); !ok {
		t.Fatal("lock second failed")
	}
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("locked request status = %d, want 200", code)
	}
	if first.calls() != 0 || second.calls() != 1 {
		t.Fatalf("calls = %d/%d, want 0/1 (all requests to locked instance)", first.calls(), second.calls())
	}
	if state, _, _, locked := ps.stateFor(0); state != "-" || locked {
		t.Errorf("first display = %q locked=%v, want - / false", state, locked)
	}
	if state, _, _, locked := ps.stateFor(1); state != stateLocked || !locked {
		t.Errorf("second display = %q locked=%v, want locked / true", state, locked)
	}

	// §24.25 M92/M93: the breaker is paused for the pinned instance, but a
	// failing pin now falls through to the remaining instances. second fails,
	// first serves the request. The locked failure is not counted.
	second.setFail(true)
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("locked failing request status = %d, want 200 from the next instance", code)
	}
	if first.calls() != 1 || second.calls() != 2 {
		t.Errorf("calls = %d/%d, want 1/2 (locked failure falls through)", first.calls(), second.calls())
	}
	if got := streamInstanceFailures(handler, protocolOpenAI, 1); got != 0 {
		t.Errorf("locked instance failures = %d, want 0 (M92)", got)
	}
	locked := handler.byProtocol[protocolOpenAI][1]
	locked.mu.Lock()
	lockedUntil := locked.degradedUntil
	locked.mu.Unlock()
	if !lockedUntil.IsZero() {
		t.Errorf("locked instance degradedUntil = %v, want zero (M92)", lockedUntil)
	}
	if state, _, _, isLocked := ps.stateFor(1); state != stateLocked || !isLocked {
		t.Errorf("state after locked failure = %q locked=%v, want locked / true", state, isLocked)
	}

	// Moving the lock is allowed and routes to the new instance. Both upstreams
	// now fail at the connection layer: the pinned first is tried, then second,
	// then the pinned instance gets one final handoff (§24.25 M94d/M15de).
	first.setFail(true)
	if _, ok := ps.lockInstance("first"); !ok {
		t.Fatal("lock first failed")
	}
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusBadGateway {
		t.Fatalf("request to locked first status = %d, want 502", code)
	}
	if first.calls() != 3 || second.calls() != 3 {
		t.Errorf("calls = %d/%d, want 3/3 (locked, peer, locked handoff)", first.calls(), second.calls())
	}
	if got := streamInstanceFailures(handler, protocolOpenAI, 0); got != 0 {
		t.Errorf("locked first failures = %d, want 0 (M92)", got)
	}
	if got := streamInstanceFailures(handler, protocolOpenAI, 1); got != 1 {
		t.Errorf("non-locked second failures = %d, want 1 (M92 only exempts the pin)", got)
	}

	if !ps.unlock() {
		t.Fatal("unlock returned false")
	}
	for i := range handler.byProtocol[protocolOpenAI] {
		if state := handler.byProtocol[protocolOpenAI][i].state(); state != stateClosed {
			t.Errorf("instance %d state after unlock = %q, want closed", i, state)
		}
		if got := streamInstanceFailures(handler, protocolOpenAI, i); got != 0 {
			t.Errorf("instance %d failures after unlock = %d, want 0", i, got)
		}
	}

	// Unlocking restarts routing at the first closed instance: first is tried
	// (and fails), then the request falls through to second.
	second.setFail(false)
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("post-unlock request status = %d, want 200", code)
	}
	if first.calls() != 4 || second.calls() != 4 {
		t.Errorf("calls = %d/%d, want 4/4 (original order restored)", first.calls(), second.calls())
	}
}

// TestLockedOpenInstanceStillServes covers M15ao: a pin overrides the breaker.
func TestLockedOpenInstanceStillServes(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusOK)
	second := newCBBackend(t, http.StatusOK)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-x"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	ps := handler.protocolRuntime(protocolOpenAI)

	handler.recordFailure(handler.byProtocol[protocolOpenAI][0])
	if got := handler.byProtocol[protocolOpenAI][0].state(); got != stateOpen {
		t.Fatalf("first state = %q, want open", got)
	}
	if _, ok := ps.lockInstance("first"); !ok {
		t.Fatal("lock first failed")
	}
	if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusOK {
		t.Fatalf("locked request status = %d, want 200", code)
	}
	if first.calls() != 1 || second.calls() != 0 {
		t.Errorf("calls = %d/%d, want 1/0 (pin wins over open state)", first.calls(), second.calls())
	}
}

// TestSingleInstanceProtocolNeverOpens covers M15ap.
func TestSingleInstanceProtocolNeverOpens(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	backend := newCBBackend(t, http.StatusOK)
	backend.setFail(true)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(backend, "only", "gpt-x"),
	)
	handler.monitor = newMonitorHub()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	ps := handler.protocolRuntime(protocolOpenAI)
	inst := handler.byProtocol[protocolOpenAI][0]

	const failures = 3
	for i := 0; i < failures; i++ {
		if code := cbSend(t, server.URL, protocolOpenAI, "gpt-x"); code != http.StatusBadGateway {
			t.Fatalf("request %d status = %d, want 502", i+1, code)
		}
		state, since, reason, _ := ps.stateFor(0)
		if state != stateClosed || since != 0 || reason != "" {
			t.Fatalf("after failure %d: state/since/reason = %q/%d/%q, want closed/0/empty",
				i+1, state, since, reason)
		}
	}
	if got := inst.cbDeadline(); !got.IsZero() {
		t.Errorf("degradedUntil = %s, want zero (M32 keeps the breaker closed)", got)
	}
	if got := backend.calls(); got != failures {
		t.Errorf("backend calls = %d, want %d", got, failures)
	}

	snapshot := handler.monitor.stats.snapshot(0, time.Now())
	if snapshot.Requests.Total != failures || snapshot.Requests.Failed != failures {
		t.Errorf("total/failed = %d/%d, want %d/%d",
			snapshot.Requests.Total, snapshot.Requests.Failed, failures, failures)
	}
	if len(snapshot.RecentErrors) != failures {
		t.Errorf("recentErrors = %d, want %d", len(snapshot.RecentErrors), failures)
	}
	var only *statsInstance
	for i := range snapshot.Instances {
		if snapshot.Instances[i].Protocol == "openai" && snapshot.Instances[i].Name == "only" {
			only = &snapshot.Instances[i]
		}
	}
	if only == nil || only.Requests != failures || only.Failed != failures {
		t.Errorf("instance counters = %+v, want requests/failed %d/%d", only, failures, failures)
	}

	clock.Advance(5 * time.Minute)
	if state, _, _, _ := ps.stateFor(0); state != stateClosed {
		t.Errorf("state after idle clock advance = %q, want closed", state)
	}
	if got := backend.calls(); got != failures {
		t.Errorf("backend calls = %d, want %d (no background traffic)", got, failures)
	}
}

// TestMonitorPageHasNoProbeUI covers M15am at the source level: the embedded
// page must no longer contain any probe badge or probe status wording.
func TestMonitorPageHasNoProbeUI(t *testing.T) {
	page := string(web.IndexHTML)
	for _, needle := range []string{"probe-badge", "probe-row", "stat-probe", "探测中", "探测成功", "探测失败"} {
		if strings.Contains(page, needle) {
			t.Errorf("monitor page still contains %q", needle)
		}
	}
}
