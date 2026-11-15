package proxy_test

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
	"model-router/internal/proxy"
)

// newFlakyBackend responds 200 normally, or drops the connection when fail is
// true (simulating a dial/response-header failure).
func newFlakyBackend(t *testing.T) (*httptest.Server, *atomic.Bool, func() int32, func() capturedRequest) {
	t.Helper()

	var (
		fail  atomic.Bool
		calls atomic.Int32
		mu    sync.Mutex
		got   *capturedRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = &capturedRequest{
			method:   r.Method,
			path:     r.URL.Path,
			rawQuery: r.URL.RawQuery,
			header:   r.Header.Clone(),
			body:     string(body),
		}
		mu.Unlock()

		if fail.Load() {
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

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	t.Cleanup(server.Close)

	return server, &fail, calls.Load, func() capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		if got == nil {
			t.Fatal("backend never received a request")
		}
		return *got
	}
}

func newResponseBackend(t *testing.T, responseBody string, delay time.Duration) (*httptest.Server, func() int32) {
	t.Helper()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(server.Close)
	return server, calls.Load
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newProxyWithOptions(t *testing.T, opts proxy.Options, instances ...config.Instance) *httptest.Server {
	t.Helper()

	handler, err := proxy.NewWithOptions(&config.Config{
		Listen:    "unused:0",
		AuthToken: serviceToken,
		Instances: instances,
	}, opts)
	if err != nil {
		t.Fatalf("proxy.NewWithOptions: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestCandidateChainOpenAI(t *testing.T) {
	t.Run("exact model is forwarded unchanged to later candidates", func(t *testing.T) {
		first, fail, firstCalls, _ := newFlakyBackend(t)
		fail.Store(true)
		second, secondCaptured, secondCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
		proxyServer := newProxy(t, serviceToken,
			openAIInstance(first.URL, "first-key", "gpt-x"),
			openAIInstance(second.URL, "second-key", "gpt-x"),
		)

		body := `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := firstCalls(); got != 1 {
			t.Errorf("first backend calls = %d, want 1", got)
		}
		if got := secondCalls(); got != 1 {
			t.Errorf("second backend calls = %d, want 1", got)
		}
		if got := secondCaptured().body; got != body {
			t.Errorf("second backend body = %q, want verbatim %q", got, body)
		}
	})

	t.Run("candidate without the model rewrites to models[0]", func(t *testing.T) {
		first, fail, _, _ := newFlakyBackend(t)
		fail.Store(true)
		second, secondCaptured, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
		proxyServer := newProxy(t, serviceToken,
			openAIInstance(first.URL, "first-key", "gpt-x"),
			openAIInstance(second.URL, "second-key", "gpt-b1", "gpt-b2"),
		)

		body := `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		assertModelRewritten(t, body, secondCaptured().body, "gpt-b1")
	})

	t.Run("base is the first exact match and earlier instances are excluded", func(t *testing.T) {
		first, _, firstCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
		second, secondCaptured, secondCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
		proxyServer := newProxy(t, serviceToken,
			openAIInstance(first.URL, "first-key", "gpt-a"),
			openAIInstance(second.URL, "second-key", "gpt-b"),
		)

		body := `{"model":"gpt-b"}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := firstCalls(); got != 0 {
			t.Errorf("first backend calls = %d, want 0 (excluded before base)", got)
		}
		if got := secondCalls(); got != 1 {
			t.Errorf("second backend calls = %d, want 1", got)
		}
		if got := secondCaptured().body; got != body {
			t.Errorf("body = %q, want verbatim %q", got, body)
		}
	})
}

func TestCandidateChainAnthropic(t *testing.T) {
	t.Run("role is rewritten for each candidate", func(t *testing.T) {
		first, fail, _, _ := newFlakyBackend(t)
		fail.Store(true)
		second, secondCaptured, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
		proxyServer := newProxy(t, serviceToken,
			anthropicInstance(first.URL, "first-key", map[string]string{"sonnet": "model-a"}),
			anthropicInstance(second.URL, "second-key", map[string]string{"sonnet": "model-b"}),
		)

		body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		assertModelRewritten(t, body, secondCaptured().body, "model-b")
	})

	t.Run("wildcard candidate serves a missing role", func(t *testing.T) {
		first, fail, _, _ := newFlakyBackend(t)
		fail.Store(true)
		second, secondCaptured, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
		proxyServer := newProxy(t, serviceToken,
			anthropicInstance(first.URL, "first-key", map[string]string{"sonnet": "model-a"}),
			anthropicInstance(second.URL, "second-key", map[string]string{"*": "fallback-b"}),
		)

		body := `{"model":"sonnet","messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		assertModelRewritten(t, body, secondCaptured().body, "fallback-b")
	})

	t.Run("instances without the role or wildcard are skipped", func(t *testing.T) {
		first, fail, _, _ := newFlakyBackend(t)
		fail.Store(true)
		skipped, _, skippedCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
		third, thirdCaptured, thirdCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
		proxyServer := newProxy(t, serviceToken,
			anthropicInstance(first.URL, "first-key", map[string]string{"sonnet": "model-a"}),
			anthropicInstance(skipped.URL, "skip-key", map[string]string{"opus": "model-b"}),
			anthropicInstance(third.URL, "third-key", map[string]string{"sonnet": "model-c"}),
		)

		body := `{"model":"sonnet","messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := skippedCalls(); got != 0 {
			t.Errorf("skipped backend calls = %d, want 0", got)
		}
		if got := thirdCalls(); got != 1 {
			t.Errorf("third backend calls = %d, want 1", got)
		}
		assertModelRewritten(t, body, thirdCaptured().body, "model-c")
	})

	t.Run("exact mapped model is forwarded unchanged", func(t *testing.T) {
		first, fail, _, _ := newFlakyBackend(t)
		fail.Store(true)
		second, secondCaptured, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
		proxyServer := newProxy(t, serviceToken,
			anthropicInstance(first.URL, "first-key", map[string]string{"sonnet": "shared-model"}),
			anthropicInstance(second.URL, "second-key", map[string]string{"opus": "shared-model"}),
		)

		body := `{"model":"shared-model","messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := secondCaptured().body; got != body {
			t.Errorf("body = %q, want verbatim %q", got, body)
		}
	})
}

func TestFallbackChainR5WithoutWildcard(t *testing.T) {
	backend, captured, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	proxyServer := newProxy(t, serviceToken,
		anthropicInstance(backend.URL, "key", map[string]string{"sonnet": "model-a"}),
	)

	body := `{"messages":[]}`
	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
	}
	if got := captured().body; got != body {
		t.Errorf("body = %q, want verbatim %q", got, body)
	}
}

func TestFallbackTemporaryAndSustained(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	first, fail, firstCalls, _ := newFlakyBackend(t)
	fail.Store(true)
	second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	proxyServer := newProxyWithOptions(t, proxy.Options{
		Now:              clock.Now,
		FailureThreshold: 3,
		DegradeDuration:  5 * time.Minute,
	},
		openAIInstance(first.URL, "first-key", "gpt-x"),
		openAIInstance(second.URL, "second-key", "gpt-x"),
	)
	body := `{"model":"gpt-x"}`

	for i := 1; i <= 2; i++ {
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, resp.StatusCode)
		}
	}
	if got := firstCalls(); got != 2 {
		t.Errorf("after 2 failures first calls = %d, want 2 (still tries primary)", got)
	}
	if got := secondCalls(); got != 2 {
		t.Errorf("second backend calls = %d, want 2", got)
	}

	// Third failure reaches the threshold and enters sustained fallback.
	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("third request status = %d, want 200", resp.StatusCode)
	}
	if got := firstCalls(); got != 3 {
		t.Errorf("first calls = %d, want 3", got)
	}

	// Fourth request skips the degraded primary.
	resp = send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fourth request status = %d, want 200", resp.StatusCode)
	}
	if got := firstCalls(); got != 3 {
		t.Errorf("first calls = %d, want 3 (degraded primary skipped)", got)
	}
	if got := secondCalls(); got != 4 {
		t.Errorf("second backend calls = %d, want 4", got)
	}
}

func TestFallbackRecoveryProbeFailure(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	first, fail, firstCalls, _ := newFlakyBackend(t)
	fail.Store(true)
	second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	proxyServer := newProxyWithOptions(t, proxy.Options{
		Now:              clock.Now,
		FailureThreshold: 3,
		DegradeDuration:  5 * time.Minute,
	},
		openAIInstance(first.URL, "first-key", "gpt-x"),
		openAIInstance(second.URL, "second-key", "gpt-x"),
	)
	body := `{"model":"gpt-x"}`

	for i := 0; i < 3; i++ {
		send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	}
	if got := firstCalls(); got != 3 {
		t.Fatalf("first calls = %d, want 3", got)
	}

	clock.Advance(5*time.Minute + time.Second)
	// The recovery probe fails: re-degrade for another window.
	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe request status = %d, want 200", resp.StatusCode)
	}
	if got := firstCalls(); got != 4 {
		t.Errorf("first calls = %d, want 4 (recovery probe attempted)", got)
	}
	if got := secondCalls(); got != 4 {
		t.Errorf("second backend calls = %d, want 4", got)
	}

	// Still inside the new window: skip again.
	resp = send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := firstCalls(); got != 4 {
		t.Errorf("first calls = %d, want 4 (re-degraded)", got)
	}
}

func TestFallbackSuccessResets(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	first, fail, firstCalls, _ := newFlakyBackend(t)
	fail.Store(true)
	second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	proxyServer := newProxyWithOptions(t, proxy.Options{
		Now:              clock.Now,
		FailureThreshold: 3,
		DegradeDuration:  5 * time.Minute,
	},
		openAIInstance(first.URL, "first-key", "gpt-x"),
		openAIInstance(second.URL, "second-key", "gpt-x"),
	)
	body := `{"model":"gpt-x"}`

	for i := 0; i < 3; i++ {
		send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	}

	clock.Advance(5*time.Minute + time.Second)
	fail.Store(false)
	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := firstCalls(); got != 4 {
		t.Errorf("first calls = %d, want 4 (recovered)", got)
	}
	if got := secondCalls(); got != 3 {
		t.Errorf("second backend calls = %d, want 3 (primary recovered)", got)
	}

	// Failure count was reset: the primary is tried again before degrading.
	fail.Store(true)
	send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if got := firstCalls(); got != 6 {
		t.Errorf("first calls = %d, want 6 (not degraded after reset)", got)
	}
}

func TestFallbackAllDegradedForcesHead(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	first, failFirst, firstCalls, _ := newFlakyBackend(t)
	failFirst.Store(true)
	second, failSecond, secondCalls, _ := newFlakyBackend(t)
	failSecond.Store(true)
	proxyServer := newProxyWithOptions(t, proxy.Options{
		Now:              clock.Now,
		FailureThreshold: 1,
		DegradeDuration:  5 * time.Minute,
	},
		openAIInstance(first.URL, "first-key", "gpt-x"),
		openAIInstance(second.URL, "second-key", "gpt-x"),
	)
	body := `{"model":"gpt-x"}`

	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if firstCalls() != 1 || secondCalls() != 1 {
		t.Fatalf("calls = %d/%d, want 1/1", firstCalls(), secondCalls())
	}

	// All degraded: force the chain head once, do not try the rest.
	resp = send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := firstCalls(); got != 2 {
		t.Errorf("first calls = %d, want 2 (forced head)", got)
	}
	if got := secondCalls(); got != 1 {
		t.Errorf("second calls = %d, want 1 (not tried again)", got)
	}
}

func TestFallbackConcurrent(t *testing.T) {
	first, fail, firstCalls, _ := newFlakyBackend(t)
	fail.Store(true)
	second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	proxyServer := newProxyWithOptions(t, proxy.Options{FailureThreshold: 1000},
		openAIInstance(first.URL, "first-key", "gpt-x"),
		openAIInstance(second.URL, "second-key", "gpt-x"),
	)
	body := `{"model":"gpt-x"}`

	const workers = 20
	var wg sync.WaitGroup
	errs := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/chat/completions", strings.NewReader(body))
			if err != nil {
				errs <- err.Error()
				return
			}
			req.Header.Set("Authorization", "Bearer "+serviceToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err.Error()
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusOK {
				errs <- "unexpected status " + resp.Status
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := firstCalls(); got != workers {
		t.Errorf("first calls = %d, want %d", got, workers)
	}
	if got := secondCalls(); got != workers {
		t.Errorf("second calls = %d, want %d", got, workers)
	}
}

func TestThroughputFailure(t *testing.T) {
	newSlowProxy := func(t *testing.T, proto string, slowBody string, slowDelay time.Duration) (*httptest.Server, func() int32) {
		t.Helper()
		slow, slowCalls := newResponseBackend(t, slowBody, slowDelay)
		fast, _ := newResponseBackend(t, `{"id":"fast"}`, 0)
		var first, second config.Instance
		if proto == config.ProtocolAnthropic {
			first = anthropicInstance(slow.URL, "slow-key", map[string]string{"sonnet": "slow-model"})
			second = anthropicInstance(fast.URL, "fast-key", map[string]string{"sonnet": "fast-model"})
		} else {
			first = openAIInstance(slow.URL, "slow-key", "gpt-x")
			second = openAIInstance(fast.URL, "fast-key", "gpt-x")
		}
		return newProxyWithOptions(t, proxy.Options{
			FailureThreshold:   1,
			DegradeDuration:    time.Hour,
			MinTokensPerSecond: 1e9,
		}, first, second), slowCalls
	}

	t.Run("openai low completion_tokens does NOT record failure", func(t *testing.T) {
		slowBody := `{"id":"slow","usage":{"completion_tokens":1},"choices":[]}`
		server, slowCalls := newSlowProxy(t, config.ProtocolOpenAI, slowBody, 20*time.Millisecond)
		body := `{"model":"gpt-x"}`

		resp := send(t, http.MethodPost, server.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (response already committed)", resp.StatusCode)
		}
		if got := readBody(t, resp); got != slowBody {
			t.Errorf("body = %q, want slow response forwarded verbatim", got)
		}
		// Below min sample (1 token < 20): skipped, not degraded; next request hits it again.
		resp = send(t, http.MethodPost, server.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("second status = %d, want 200", resp.StatusCode)
		}
		if got := slowCalls(); got != 2 {
			t.Errorf("slow backend calls = %d, want 2 (below min sample, no degradation)", got)
		}
	})

	t.Run("anthropic low output_tokens does NOT record failure", func(t *testing.T) {
		slowBody := `{"id":"slow","usage":{"output_tokens":1},"content":[]}`
		server, slowCalls := newSlowProxy(t, config.ProtocolAnthropic, slowBody, 20*time.Millisecond)
		body := `{"model":"sonnet"}`

		resp := send(t, http.MethodPost, server.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp = send(t, http.MethodPost, server.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("second status = %d, want 200", resp.StatusCode)
		}
		if got := slowCalls(); got != 2 {
			t.Errorf("slow backend calls = %d, want 2 (below min sample, no degradation)", got)
		}
	})

	t.Run("missing usage does not record failure", func(t *testing.T) {
		slow, slowCalls := newResponseBackend(t, `{"id":"no-usage"}`, 10*time.Millisecond)
		fast, fastCalls := newResponseBackend(t, `{"id":"fast"}`, 0)
		server := newProxyWithOptions(t, proxy.Options{
			FailureThreshold:   1,
			MinTokensPerSecond: 1e9,
		},
			openAIInstance(slow.URL, "slow-key", "gpt-x"),
			openAIInstance(fast.URL, "fast-key", "gpt-x"),
		)
		body := `{"model":"gpt-x"}`

		for i := 0; i < 2; i++ {
			resp := send(t, http.MethodPost, server.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
		}
		if got := slowCalls(); got != 2 {
			t.Errorf("slow backend calls = %d, want 2 (usage absent, no degradation)", got)
		}
		if got := fastCalls(); got != 0 {
			t.Errorf("fast backend calls = %d, want 0", got)
		}
	})
}

func newServerFromJSON(t *testing.T, configJSON string) *httptest.Server {
	t.Helper()

	cfg, err := config.Parse([]byte(configJSON))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	handler, err := proxy.New(cfg)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func openAIFallbackJSON(firstURL, secondURL, fallback string) string {
	return fmt.Sprintf(`{
		"auth_token": %q,
		"fallback": %s,
		"instances": {"openai": [
			{"base_url": %q, "api_key": "first-key", "models": ["gpt-x"]},
			{"base_url": %q, "api_key": "second-key", "models": ["gpt-x"]}
		]}
	}`, serviceToken, fallback, firstURL, secondURL)
}

func TestFallbackFromConfigThreshold(t *testing.T) {
	first, fail, firstCalls, _ := newFlakyBackend(t)
	fail.Store(true)
	second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	server := newServerFromJSON(t, openAIFallbackJSON(first.URL, second.URL,
		`{"failure_threshold": 2, "degrade_duration": "1h"}`))
	body := `{"model":"gpt-x"}`

	for i := 0; i < 2; i++ {
		resp := send(t, http.MethodPost, server.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i+1, resp.StatusCode)
		}
	}
	if got := firstCalls(); got != 2 {
		t.Fatalf("first calls = %d, want 2 (configured threshold reached)", got)
	}

	// Configured threshold 2 has been reached: the primary is now skipped.
	resp := send(t, http.MethodPost, server.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := firstCalls(); got != 2 {
		t.Errorf("first calls = %d, want 2 (degraded)", got)
	}
	if got := secondCalls(); got != 3 {
		t.Errorf("second calls = %d, want 3", got)
	}
}

func TestFallbackFromConfigDegradeDuration(t *testing.T) {
	first, fail, firstCalls, _ := newFlakyBackend(t)
	fail.Store(true)
	second, _, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	server := newServerFromJSON(t, openAIFallbackJSON(first.URL, second.URL,
		`{"failure_threshold": 1, "degrade_duration": "100ms"}`))
	body := `{"model":"gpt-x"}`

	send(t, http.MethodPost, server.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	send(t, http.MethodPost, server.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if got := firstCalls(); got != 1 {
		t.Fatalf("first calls = %d, want 1 (degraded immediately)", got)
	}

	time.Sleep(250 * time.Millisecond)
	// The configured 100ms window has elapsed: probe the primary again.
	send(t, http.MethodPost, server.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
	if got := firstCalls(); got != 2 {
		t.Errorf("first calls = %d, want 2 (recovery probe after configured duration)", got)
	}
}

func TestFallbackFromConfigDisableThroughput(t *testing.T) {
	slow, slowCalls := newResponseBackend(t, `{"id":"slow","usage":{"completion_tokens":1}}`, 20*time.Millisecond)
	fast, fastCalls := newResponseBackend(t, `{"id":"fast"}`, 0)
	server := newServerFromJSON(t, openAIFallbackJSON(slow.URL, fast.URL,
		`{"failure_threshold": 1, "degrade_duration": "1h", "min_tokens_per_second": 0}`))
	body := `{"model":"gpt-x"}`

	for i := 0; i < 2; i++ {
		resp := send(t, http.MethodPost, server.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	if got := slowCalls(); got != 2 {
		t.Errorf("slow calls = %d, want 2 (throughput judgement disabled)", got)
	}
	if got := fastCalls(); got != 0 {
		t.Errorf("fast calls = %d, want 0", got)
	}
}

func newSilentSSEBackend(t *testing.T, payload string) (*httptest.Server, func() int32) {
	t.Helper()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server, calls.Load
}

func newSlowSSEBackend(t *testing.T, chunks []string, delay time.Duration) (*httptest.Server, func() int32) {
	t.Helper()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(delay)
		}
	}))
	t.Cleanup(server.Close)
	return server, calls.Load
}

func TestIdleTimeoutAnthropicPauseTurn(t *testing.T) {
	const events = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

`
	upstream, _ := newSilentSSEBackend(t, events)
	proxyServer := newProxyWithOptions(t, proxy.Options{IdleTimeout: 50 * time.Millisecond},
		anthropicInstance(upstream.URL, "key", map[string]string{"sonnet": "model-a"}),
	)

	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages",
		`{"model":"sonnet","messages":[]}`, withAuth(config.ProtocolAnthropic, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)

	for _, want := range []string{
		"event: content_block_stop",
		`"index":0`,
		"event: message_delta",
		`"stop_reason":"pause_turn"`,
		`"stop_sequence":null`,
		`"output_tokens":1`,
		"event: message_stop",
		`"type":"message_stop"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream body missing %q:\n%s", want, body)
		}
	}
}

func TestIdleTimeoutOpenAITruncates(t *testing.T) {
	const events = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hi"}}]}

`
	upstream, _ := newSilentSSEBackend(t, events)
	fallback, _, fallbackCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	proxyServer := newProxyWithOptions(t, proxy.Options{
		IdleTimeout:      50 * time.Millisecond,
		FailureThreshold: 1,
	},
		openAIInstance(upstream.URL, "first-key", "gpt-x"),
		openAIInstance(fallback.URL, "second-key", "gpt-x"),
	)

	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-x"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+serviceToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call proxy: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr == nil {
		t.Fatalf("stream read error = nil, want a truncated connection (body %q)", body)
	}
	if strings.Contains(string(body), "[DONE]") {
		t.Errorf("stream body contains [DONE], want truncation: %q", body)
	}

	// The idle instance was recorded as failed; the next request uses the fallback.
	resp = send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions",
		`{"model":"gpt-x"}`, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", resp.StatusCode)
	}
	if got := fallbackCalls(); got != 1 {
		t.Errorf("fallback calls = %d, want 1 (idle instance degraded)", got)
	}
}

func TestIdleTimeoutFromConfig(t *testing.T) {
	const events = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"output_tokens":2}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`
	upstream, _ := newSilentSSEBackend(t, events)
	configJSON := fmt.Sprintf(`{
		"auth_token": %q,
		"fallback": {"idle_timeout": "50ms"},
		"instances": {"anthropic": [{"base_url": %q, "api_key": "k", "roles": {"sonnet": "m"}}]}
	}`, serviceToken, upstream.URL)
	proxyServer := newServerFromJSON(t, configJSON)

	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages",
		`{"model":"sonnet","messages":[]}`, withAuth(config.ProtocolAnthropic, nil))
	body := readBody(t, resp)
	if !strings.Contains(body, `"stop_reason":"pause_turn"`) || !strings.Contains(body, "event: message_stop") {
		t.Errorf("config idle_timeout not applied; body:\n%s", body)
	}
	if !strings.Contains(body, `"output_tokens":2`) {
		t.Errorf("tail did not reuse seen usage tokens; body:\n%s", body)
	}
}

// newTrickleSSEBackend emits one tiny SSE comment per interval and then tail.
// §19's real-time detector needs a genuinely long-lived, low-byte-rate stream.
func newTrickleSSEBackend(t *testing.T, chunks int, interval time.Duration, tail string) (*httptest.Server, func() int32) {
	t.Helper()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(interval)
		}
		_, _ = io.WriteString(w, tail)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server, calls.Load
}

func TestRealtimeSlowAnthropicPauseTurn(t *testing.T) {
	upstream, _ := newTrickleSSEBackend(t, 40, 300*time.Millisecond, "")
	fallback, _, fallbackCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	proxyServer := newProxyWithOptions(t, proxy.Options{
		FailureThreshold: 1,
		DegradeDuration:  time.Hour,
	},
		anthropicInstance(upstream.URL, "first-key", map[string]string{"sonnet": "model-a"}),
		anthropicInstance(fallback.URL, "second-key", map[string]string{"sonnet": "model-b"}),
	)
	logs := captureLogs(t)

	start := time.Now()
	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages",
		`{"model":"sonnet","messages":[]}`, withAuth(config.ProtocolAnthropic, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if elapsed := time.Since(start); elapsed < 10*time.Second {
		t.Fatalf("stream ended after %s, want the 10s window to fill before the verdict", elapsed)
	}
	for _, want := range []string{
		"event: message_delta",
		`"stop_reason":"pause_turn"`,
		`"stop_sequence":null`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream body missing %q:\n%s", want, body)
		}
	}
	if got := waitForLog(t, logs, "slow stream"); !strings.Contains(got, "slow stream (realtime)") {
		t.Errorf("log missing the realtime slow marker:\n%s", got)
	} else if strings.Contains(got, "slow stream:") {
		t.Errorf("post-hoc throughput judgement must be skipped after a realtime verdict:\n%s", got)
	}

	// The interrupted instance was recorded as failed; the next request falls back.
	resp = send(t, http.MethodPost, proxyServer.URL+"/v1/messages",
		`{"model":"sonnet","messages":[]}`, withAuth(config.ProtocolAnthropic, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if got := fallbackCalls(); got != 1 {
		t.Errorf("fallback calls = %d, want 1 (slow instance degraded)", got)
	}
}

func TestRealtimeSlowOpenAINotInterrupted(t *testing.T) {
	const chunks = 36
	upstream, _ := newTrickleSSEBackend(t, chunks, 300*time.Millisecond, "data: [DONE]\n\n")
	fallback, _, fallbackCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	proxyServer := newProxyWithOptions(t, proxy.Options{
		FailureThreshold: 1,
		DegradeDuration:  time.Hour,
	},
		openAIInstance(upstream.URL, "first-key", "gpt-x"),
		openAIInstance(fallback.URL, "second-key", "gpt-x"),
	)
	logs := captureLogs(t)

	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions",
		`{"model":"gpt-x"}`, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if count := strings.Count(body, "keepalive"); count != chunks {
		t.Errorf("forwarded chunks = %d, want %d (OpenAI slow streams must not be interrupted)", count, chunks)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream body missing the terminating [DONE]:\n%s", body)
	}
	if got := waitForLog(t, logs, "slow stream"); !strings.Contains(got, "slow stream (realtime)") {
		t.Errorf("log missing the realtime slow marker:\n%s", got)
	}

	resp = send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions",
		`{"model":"gpt-x"}`, withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if got := fallbackCalls(); got != 1 {
		t.Errorf("fallback calls = %d, want 1 (slow instance degraded)", got)
	}
}

func TestRealtimeSlowWindowNotFull(t *testing.T) {
	// Three tiny chunks over ~3s: the byte rate is far below the threshold, but
	// the 10s window is not full, so no real-time verdict may fire (§19.4).
	upstream, upstreamCalls := newTrickleSSEBackend(t, 3, time.Second, "")
	proxyServer := newProxyWithOptions(t, proxy.Options{
		FailureThreshold: 1,
		DegradeDuration:  time.Hour,
	},
		anthropicInstance(upstream.URL, "first-key", map[string]string{"sonnet": "model-a"}),
	)
	logs := captureLogs(t)

	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages",
		`{"model":"sonnet","messages":[]}`, withAuth(config.ProtocolAnthropic, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if strings.Contains(body, "pause_turn") {
		t.Errorf("stream below the 10s window must not be judged slow:\n%s", body)
	}
	// This test asserts the *absence* of a realtime verdict, so it cannot wait
	// for "slow stream" (it never appears). Waiting for the request log line
	// instead guarantees the handler finished judging before we check.
	if got := waitForLog(t, logs, " -> "); strings.Contains(got, "slow stream (realtime)") {
		t.Errorf("log reports a realtime slow verdict before the window filled:\n%s", got)
	}

	resp = send(t, http.MethodPost, proxyServer.URL+"/v1/messages",
		`{"model":"sonnet","messages":[]}`, withAuth(config.ProtocolAnthropic, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if got := upstreamCalls(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (no failure recorded)", got)
	}
}
