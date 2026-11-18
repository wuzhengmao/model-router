package proxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
)

// streamLogWriter lets this file capture the standard logger without relying on
// the proxy_test package helper (these tests live in package proxy).
type streamLogWriter func([]byte) (int, error)

func (f streamLogWriter) Write(p []byte) (int, error) { return f(p) }

func captureStreamLogs(t *testing.T) func() string {
	t.Helper()
	var (
		mu  sync.Mutex
		buf strings.Builder
	)
	previous := log.Writer()
	log.SetOutput(streamLogWriter(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(previous) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

func streamAnthropicInstance(url, name string, timeout time.Duration) config.Instance {
	return config.Instance{
		Protocol: config.ProtocolAnthropic,
		Name:     name,
		BaseURL:  url,
		APIKey:   "upstream-key",
		Timeout:  config.Duration(timeout),
		Roles:    map[string]string{"sonnet": "model-a"},
	}
}

func streamOpenAIInstance(url, name string, timeout time.Duration, models ...string) config.Instance {
	return config.Instance{
		Protocol: config.ProtocolOpenAI,
		Name:     name,
		BaseURL:  url,
		APIKey:   "upstream-key",
		Timeout:  config.Duration(timeout),
		Models:   models,
	}
}

func newStreamHandler(t *testing.T, opts Options, instances ...config.Instance) *Handler {
	t.Helper()
	handler, err := NewWithOptions(&config.Config{AuthToken: "tok", Instances: instances}, opts)
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	return handler
}

// streamInstanceFailures reads the failure counter without racing the handler.
func streamInstanceFailures(handler *Handler, proto protocol, index int) int {
	inst := handler.byProtocol[proto][index]
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.consecutiveFailures
}

// doStreamRequest performs one proxied streaming call. The read error is
// returned because a truncated OpenAI stream is a success for the proxy.
func doStreamRequest(t *testing.T, serverURL string, proto protocol, body string) (int, string, error) {
	t.Helper()
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
	data, readErr := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), readErr
}

// streamHeartbeatBackend emits SSE chunks at a fixed interval until the caller
// disconnects. prefix is written once before the loop.
func streamHeartbeatBackend(t *testing.T, prefix, chunk string, interval time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if prefix != "" {
			_, _ = io.WriteString(w, prefix)
			writes.Add(1)
			if flusher != nil {
				flusher.Flush()
			}
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := io.WriteString(w, chunk); err != nil {
					return
				}
				writes.Add(1)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, &writes
}

// TestContentIdleTimeoutAnthropic covers M15bw (Anthropic half) and M15cd: a
// stream that only sends message_start/content_block_start/heartbeats is
// terminated once the instance timeout elapses, and the client gets the same
// pause_turn tail as an idle_timeout.
func TestContentIdleTimeoutAnthropic(t *testing.T) {
	const prefix = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"
	upstream, _ := streamHeartbeatBackend(t, prefix, "event: ping\ndata: {\"type\":\"ping\"}\n\n", 10*time.Millisecond)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, streamAnthropicInstance(upstream.URL, "ping-probe", 200*time.Millisecond))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	start := time.Now()
	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("stream ended after %s, want the 200ms instance timeout to fire", elapsed)
	}
	for _, want := range []string{
		`"stop_reason":"pause_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if got := streamInstanceFailures(handler, protocolAnthropic, 0); got != 1 {
		t.Errorf("consecutiveFailures = %d, want 1", got)
	}
	output := logs()
	reason := `instance ping-probe timeout: content-idle for 200ms (no content block); stream terminated`
	if !strings.Contains(output, reason) {
		t.Errorf("log missing content-idle reason line %q:\n%s", reason, output)
	}
	if got := strings.Count(output, "stream ping-probe hdr="); got != 1 {
		t.Errorf("stage summary lines = %d, want 1:\n%s", got, output)
	}
	if !strings.Contains(output, "first_content=-") {
		t.Errorf("stage summary should report no content block:\n%s", output)
	}
}

// TestContentIdleIgnoresBoundaryEvents covers M15bz: message_start,
// content_block_start and content_block_stop must not reset the content-idle
// timer even when they repeat forever.
func TestContentIdleIgnoresBoundaryEvents(t *testing.T) {
	const chunk = "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"
	upstream, writes := streamHeartbeatBackend(t, "", chunk, 10*time.Millisecond)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, streamAnthropicInstance(upstream.URL, "boundary-probe", 200*time.Millisecond))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	start := time.Now()
	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("stream ended after %s, want the content-idle timer to fire", elapsed)
	}
	if !strings.Contains(body, `"stop_reason":"pause_turn"`) {
		t.Errorf("boundary-only stream was not terminated with pause_turn:\n%s", body)
	}
	if got := writes.Load(); got < 2 {
		t.Errorf("upstream writes = %d, want the stream to be fed repeatedly", got)
	}
}

// TestContentIdleTimeoutOpenAI covers M15bw (OpenAI half): a chunk stream with
// no content is cut off by truncating the client connection.
func TestContentIdleTimeoutOpenAI(t *testing.T) {
	const chunk = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{}}]}` + "\n\n"
	upstream, _ := streamHeartbeatBackend(t, "", chunk, 10*time.Millisecond)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, streamOpenAIInstance(upstream.URL, "oa-probe", 200*time.Millisecond, "gpt-x"))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	start := time.Now()
	status, body, readErr := doStreamRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-x"}`)
	elapsed := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if readErr == nil {
		t.Fatalf("read error = nil, want a truncated connection (body %q)", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("body contains [DONE], want truncation: %q", body)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("stream ended after %s, want the 200ms instance timeout to fire", elapsed)
	}
	if got := streamInstanceFailures(handler, protocolOpenAI, 0); got != 1 {
		t.Errorf("consecutiveFailures = %d, want 1", got)
	}
	if output := logs(); !strings.Contains(output, "instance oa-probe timeout: content-idle for 200ms") {
		t.Errorf("log missing content-idle reason line:\n%s", output)
	}
}

// TestContentIdleContentKeepsStreamAlive covers M15bx and M15ce: heartbeats
// interleaved with content never trip the content-idle timer, and the client
// body is byte-identical to what the upstream sent.
func TestContentIdleContentKeepsStreamAlive(t *testing.T) {
	var payload strings.Builder
	for i := 0; i < 10; i++ {
		payload.WriteString("event: ping\ndata: {\"type\":\"ping\"}\n\n")
		fmt.Fprintf(&payload, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"t%d\"}}\n\n", i)
	}
	payload.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	const interval = 20 * time.Millisecond
	// A finite stream: a generic heartbeat backend would never end.
	finite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range strings.SplitAfter(payload.String(), "\n\n") {
			if chunk == "" {
				continue
			}
			_, _ = io.WriteString(w, chunk)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(interval)
		}
	}))
	t.Cleanup(finite.Close)

	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, streamAnthropicInstance(finite.URL, "content-probe", 150*time.Millisecond))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body != payload.String() {
		t.Errorf("client body differs from upstream bytes:\n got: %q\nwant: %q", body, payload.String())
	}
	if strings.Contains(body, "pause_turn") {
		t.Errorf("content-bearing stream must not be interrupted:\n%s", body)
	}
	if got := streamInstanceFailures(handler, protocolAnthropic, 0); got != 0 {
		t.Errorf("consecutiveFailures = %d, want 0", got)
	}
	output := logs()
	if strings.Contains(output, "timeout: content-idle") {
		t.Errorf("content-idle fired on a healthy stream:\n%s", output)
	}
	if !strings.Contains(output, "content=10") {
		t.Errorf("stage summary should count 10 content events:\n%s", output)
	}
}

// TestSilentUpstreamFirstTokenTimeoutUnchanged covers M15by: an upstream that
// sends no byte at all is still terminated by first_token_timeout, not by the
// content-idle timer.
func TestSilentUpstreamFirstTokenTimeoutUnchanged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 150 * time.Millisecond,
	}, streamAnthropicInstance(upstream.URL, "silent-probe", 5*time.Second))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	start := time.Now()
	status, _, readErr := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if readErr != nil {
		t.Fatalf("read body: %v", readErr)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (single candidate failed)", status)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("request took %s, want the 150ms first-token timeout", elapsed)
	}
	if got := streamInstanceFailures(handler, protocolAnthropic, 0); got != 1 {
		t.Errorf("consecutiveFailures = %d, want 1", got)
	}
	output := logs()
	if !strings.Contains(output, "instance silent-probe timeout: first-token for 150ms; stream terminated") {
		t.Errorf("log missing first-token reason line:\n%s", output)
	}
	if strings.Contains(output, "content-idle") {
		t.Errorf("silent upstream must not report content-idle:\n%s", output)
	}
	summary := output[strings.Index(output, "stream silent-probe hdr="):]
	if !strings.Contains(summary, "first_block=-") || !strings.Contains(summary, "first_content=-") {
		t.Errorf("stage summary should print - for unreached phases:\n%s", output)
	}
}

// TestContentIdleMatchesIdleClientBytes covers M15ca: both timers share one
// handler branch, so the client sees the same bytes for the same upstream.
func TestContentIdleMatchesIdleClientBytes(t *testing.T) {
	run := func(t *testing.T, proto protocol, payload string, instance func(url, name string, timeout time.Duration) config.Instance, instanceTimeout time.Duration, opts Options, wantReadErr bool) (string, error) {
		t.Helper()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, payload)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		}))
		t.Cleanup(upstream.Close)
		handler := newStreamHandler(t, opts, instance(upstream.URL, "same-probe", instanceTimeout))
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		body := `{"model":"sonnet","messages":[]}`
		if proto == protocolOpenAI {
			body = `{"model":"gpt-x"}`
		}
		status, got, readErr := doStreamRequest(t, server.URL, proto, body)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if wantReadErr && readErr == nil {
			t.Errorf("read error = nil, want truncation")
		}
		if !wantReadErr && readErr != nil {
			t.Errorf("read error = %v, want clean EOF", readErr)
		}
		return got, readErr
	}

	// Both cases use a 100ms timer. "idle" keeps the instance timeout long and
	// uses a 100ms idle_timeout; "content" does the opposite, so each timer is
	// the only one that can fire.

	t.Run("anthropic", func(t *testing.T) {
		const payload = "event: message_start\n" +
			`data: {"type":"message_start","message":{"usage":{"output_tokens":2}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"
		inst := func(url, name string, timeout time.Duration) config.Instance {
			return streamAnthropicInstance(url, name, timeout)
		}
		byIdle, err := run(t, protocolAnthropic, payload, inst, 5*time.Second, Options{
			IdleTimeout:       100 * time.Millisecond,
			FirstTokenTimeout: 5 * time.Second,
		}, false)
		if err != nil {
			t.Fatalf("idle case: %v", err)
		}
		byContent, err := run(t, protocolAnthropic, payload, inst, 100*time.Millisecond, Options{
			IdleTimeout:       5 * time.Second,
			FirstTokenTimeout: 5 * time.Second,
		}, false)
		if err != nil {
			t.Fatalf("content-idle case: %v", err)
		}
		if byIdle != byContent {
			t.Errorf("client bytes differ between idle and content-idle:\n idle: %q\ncontent: %q", byIdle, byContent)
		}
		if !strings.Contains(byContent, `"stop_reason":"pause_turn"`) {
			t.Errorf("content-idle did not produce the pause_turn tail:\n%s", byContent)
		}
	})

	t.Run("openai", func(t *testing.T) {
		const payload = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{}}]}` + "\n\n"
		inst := func(url, name string, timeout time.Duration) config.Instance {
			return streamOpenAIInstance(url, name, timeout, "gpt-x")
		}
		byIdle, idleErr := run(t, protocolOpenAI, payload, inst, 5*time.Second, Options{
			IdleTimeout:       100 * time.Millisecond,
			FirstTokenTimeout: 5 * time.Second,
		}, true)
		byContent, contentErr := run(t, protocolOpenAI, payload, inst, 100*time.Millisecond, Options{
			IdleTimeout:       5 * time.Second,
			FirstTokenTimeout: 5 * time.Second,
		}, true)
		if idleErr == nil || contentErr == nil {
			t.Fatalf("both cases must truncate the connection (idle=%v content=%v)", idleErr, contentErr)
		}
		if byIdle != byContent {
			t.Errorf("client bytes differ between idle and content-idle:\n idle: %q\ncontent: %q", byIdle, byContent)
		}
	})
}

// TestStreamPhaseSummaryOncePerStream covers M15cb and M15cc: one and only one
// fixed-order stage line per streaming request, and none for non-streaming.
func TestStreamPhaseSummaryOncePerStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "gpt-plain") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"plain"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"a"}}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"b"}}]}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)
	handler := newStreamHandler(t, Options{},
		streamOpenAIInstance(upstream.URL, "phase-probe", 5*time.Second, "gpt-stream", "gpt-plain"))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	status, body, err := doStreamRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-stream"}`)
	if err != nil {
		t.Fatalf("streaming read: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("streaming status = %d, want 200", status)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream body incomplete:\n%s", body)
	}

	status, plain, err := doStreamRequest(t, server.URL, protocolOpenAI, `{"model":"gpt-plain"}`)
	if err != nil {
		t.Fatalf("plain read: %v", err)
	}
	if status != http.StatusOK || plain != `{"id":"plain"}` {
		t.Fatalf("plain response = %d %q, want 200 {\"id\":\"plain\"}", status, plain)
	}

	output := logs()
	if got := strings.Count(output, "stream phase-probe hdr="); got != 1 {
		t.Fatalf("stage summary lines = %d, want exactly 1:\n%s", got, output)
	}
	var line string
	for _, candidate := range strings.Split(output, "\n") {
		if strings.Contains(candidate, "stream phase-probe hdr=") {
			line = candidate
			break
		}
	}
	fields := []string{"hdr=", "first_block=", "first_content=", "blocks=", "pings=", " content=", "last_gap="}
	previous := -1
	for _, field := range fields {
		idx := strings.Index(line, field)
		if idx < 0 {
			t.Fatalf("stage line missing %q: %s", field, line)
		}
		if idx < previous {
			t.Errorf("stage field %q is out of order: %s", field, line)
		}
		previous = idx
	}
	if !strings.Contains(line, "content=2") || !strings.Contains(line, "pings=1") {
		t.Errorf("stage line should count 2 content events and 1 non-content event: %s", line)
	}
	if !strings.Contains(line, "first_content=") || strings.Contains(line, "first_content=-") {
		t.Errorf("first_content should be reached in a content stream: %s", line)
	}
}
