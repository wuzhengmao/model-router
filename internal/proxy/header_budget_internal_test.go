package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// §24.26 M15dp/M15dq/M15dr/M15ds/M15dw: pre-header budget and hdrprof.

type slowUploadListener struct {
	ln      net.Listener
	release chan struct{}
	once    sync.Once
}

func newSlowUploadListener(t *testing.T) *slowUploadListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &slowUploadListener{ln: ln, release: make(chan struct{})}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Keep the receive window tiny so a multi-MB upload blocks instead of
		// being absorbed by kernel buffers.
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetReadBuffer(4096)
		}
		<-s.release
		_ = conn.Close()
	}()
	t.Cleanup(s.close)
	return s
}

func (s *slowUploadListener) close() {
	s.once.Do(func() {
		close(s.release)
		_ = s.ln.Close()
	})
}

func (s *slowUploadListener) URL() string {
	return "http://" + s.ln.Addr().String()
}

func hbBigBody(t *testing.T, size int) string {
	t.Helper()
	return `{"model":"claude-sonnet-5","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"` +
		strings.Repeat("x", size) + `"}]}]}`
}

// TestHeaderBudgetExceeded covers M15dq and M15dz/M15ea/M15ec: a slow upload is
// cut at the instance timeout, the dedicated error text is kept, and a
// hdrprof line is emitted at the trigger moment.
func TestHeaderBudgetExceeded(t *testing.T) {
	slow := newSlowUploadListener(t)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(slow.URL(), "slow-inst", 200*time.Millisecond, map[string]string{"sonnet": "model-a"}))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	start := time.Now()
	status, _, err := doStreamRequest(t, server.URL, protocolAnthropic, hbBigBody(t, 2<<20))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (single candidate budget failure)", status)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("request finished after %s, want the 200ms budget to fire", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("request took %s, want the 200ms budget to fire", elapsed)
	}
	out := logs()
	if !strings.Contains(out, "response header budget exceeded after 200ms:") {
		t.Errorf("missing/incorrect budget error:\n%s", out)
	}
	if got := strings.Count(out, "hdrprof inst=slow-inst"); got != 1 {
		t.Errorf("hdrprof lines = %d, want exactly 1:\n%s", got, out)
	}
	var profile string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "hdrprof inst=slow-inst") {
			profile = line
			break
		}
	}
	for _, field := range []string{"connect=", "tls=", "upload=", "server_wait=", "body_bytes=", "reused=", "remote=", "budget=200ms"} {
		if !strings.Contains(profile, field) {
			t.Errorf("hdrprof missing %q: %s", field, profile)
		}
	}
	if failures := streamInstanceFailures(handler, protocolAnthropic, 0); failures != 1 {
		t.Errorf("failures = %d, want 1", failures)
	}
}

// TestHeaderBudgetLockedExempt covers M15dr: a pinned instance has no budget.
func TestHeaderBudgetLockedExempt(t *testing.T) {
	slow := newSlowUploadListener(t)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(slow.URL(), "slow-inst", 200*time.Millisecond, map[string]string{"sonnet": "model-a"}))
	lxLock(t, handler, protocolAnthropic, "slow-inst")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	done := make(chan struct{}, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, server.URL+PathAnthropic, strings.NewReader(hbBigBody(t, 2<<20)))
		if err != nil {
			done <- struct{}{}
			return
		}
		req.Header.Set("x-api-key", "tok")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- struct{}{}
	}()

	select {
	case <-done:
		t.Fatal("gateway returned before the locked instance was released")
	case <-time.After(600 * time.Millisecond):
	}
	slow.close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish after closing the upstream")
	}
	if out := logs(); strings.Contains(out, "response header budget exceeded") {
		t.Errorf("locked instance was budgeted:\n%s", out)
	} else if strings.Contains(out, "hdrprof") {
		t.Errorf("locked instance printed hdrprof:\n%s", out)
	}
}

// TestHeaderBudgetFallsThrough covers M15ds: after the budget failure the next
// candidate serves the request.
func TestHeaderBudgetFallsThrough(t *testing.T) {
	slow := newSlowUploadListener(t)
	good := newLXJSONBackend(t, http.StatusOK, `{"ok":true}`)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	},
		lxAnthropicInstance(slow.URL(), "slow-inst", 200*time.Millisecond, map[string]string{"sonnet": "model-a"}),
		lxAnthropicInstance(good.server.URL, "good-inst", 5*time.Second, map[string]string{"sonnet": "model-b"}),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, hbBigBody(t, 2<<20))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("response = %d %q, want 200 from the next candidate", status, body)
	}
	if out := logs(); !strings.Contains(out, "response header budget exceeded") {
		t.Errorf("missing budget error:\n%s", out)
	}
	if failures := streamInstanceFailures(handler, protocolAnthropic, 0); failures != 1 {
		t.Errorf("slow failures = %d, want 1", failures)
	}
	if good.calls() != 1 {
		t.Errorf("good calls = %d, want 1", good.calls())
	}
}

// TestHeaderBudgetFastRequestUnaffected covers M15dp: a small request is not
// delayed and does not print hdrprof.
func TestHeaderBudgetFastRequestUnaffected(t *testing.T) {
	good := newLXJSONBackend(t, http.StatusOK, `{"ok":true}`)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(good.server.URL, "fast-inst", 2*time.Second, map[string]string{"sonnet": "model-a"}))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	start := time.Now()
	status, _, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if err != nil || status != http.StatusOK {
		t.Fatalf("request = %d, %v; want 200", status, err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("fast request took %s", elapsed)
	}
	if out := logs(); strings.Contains(out, "hdrprof") {
		t.Errorf("hdrprof printed for a fast request:\n%s", out)
	}
}

// TestHeaderProfileSlowHeaders covers M15dw: hdr > timeout/2 prints a
// parseable stage breakdown.
func TestHeaderProfileSlowHeaders(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(backend.Close)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(backend.URL, "slow-hdr", 600*time.Millisecond, map[string]string{"sonnet": "model-a"}))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	status, _, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if err != nil || status != http.StatusOK {
		t.Fatalf("request = %d, %v; want 200", status, err)
	}
	out := logs()
	line := ""
	for _, candidate := range strings.Split(out, "\n") {
		if strings.Contains(candidate, "hdrprof inst=slow-hdr") {
			line = candidate
			break
		}
	}
	if line == "" {
		t.Fatalf("missing hdrprof line:\n%s", out)
	}
	for _, field := range []string{"connect=", "tls=", "upload=", "server_wait=", "body_bytes=", "reused=", "remote=", "budget=600ms"} {
		if !strings.Contains(line, field) {
			t.Errorf("hdrprof missing %q: %s", field, line)
		}
	}
	if strings.Contains(line, "server_wait=-") {
		t.Errorf("server_wait should be measured: %s", line)
	}

	fast := newLXJSONBackend(t, http.StatusOK, `{"ok":true}`)
	fastHandler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(fast.server.URL, "fast-hdr", 2*time.Second, map[string]string{"sonnet": "model-a"}))
	fastServer := httptest.NewServer(fastHandler)
	t.Cleanup(fastServer.Close)
	fastLogs := captureStreamLogs(t)
	if status, _, err := doStreamRequest(t, fastServer.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`); err != nil || status != http.StatusOK {
		t.Fatalf("fast request = %d, %v; want 200", status, err)
	}
	if strings.Contains(fastLogs(), "hdrprof") {
		t.Errorf("hdrprof printed for a fast request:\n%s", fastLogs())
	}
}
