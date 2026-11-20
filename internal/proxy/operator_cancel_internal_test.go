package proxy

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// §24.29 M15ej–M15es: operator cancellation of active requests.

// TestMonitorCancelUI covers M15ej/M15eq's static contract: the cancel column
// sits after the timeline, active rows render a × button, snapshot/ended rows
// hide it, and the click sends the documented text frame.
func TestMonitorCancelUI(t *testing.T) {
	data, err := os.ReadFile("../web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`<col class="col-cancel">`,
		`<th class="col-cancel"></th>`,
		`cancelBtn.className = 'req-cancel-btn';`,
		`cancelBtn.textContent = '×';`,
		`cancelBtn.hidden = !!req.ended;`,
		`JSON.stringify({ type: 'cancel', id: req.id })`,
		`req.cells.cancelBtn.textContent = req.cancelSent ? '已取消' : '×';`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	timeline := strings.Index(html, `<col class="col-timeline">`)
	cancel := strings.Index(html, `<col class="col-cancel">`)
	if timeline < 0 || cancel < 0 || cancel < timeline {
		t.Errorf("col-cancel must follow col-timeline (timeline=%d cancel=%d)", timeline, cancel)
	}
	rowAppend := strings.Index(html, "cells.tps, timelineCell, cancelCell]")
	if rowAppend < 0 {
		t.Error("cancel cell is not appended after the timeline cell")
	}
}

type cancelResult struct {
	status int
	body   string
	err    error
}

func doCancelRequest(serverURL, body string) cancelResult {
	req, err := http.NewRequest(http.MethodPost, serverURL+PathAnthropic, strings.NewReader(body))
	if err != nil {
		return cancelResult{err: err}
	}
	req.Header.Set("x-api-key", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cancelResult{err: err}
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	return cancelResult{status: resp.StatusCode, body: string(data), err: readErr}
}

func waitActiveID(t *testing.T, hub *monitorHub) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		hub.activeMu.Lock()
		id := ""
		if len(hub.active) > 0 {
			id = hub.active[len(hub.active)-1].id
		}
		hub.activeMu.Unlock()
		if id != "" {
			return id
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no active request registered")
	return ""
}

func waitNoActive(t *testing.T, hub *monitorHub) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		hub.activeMu.Lock()
		empty := len(hub.active) == 0
		hub.activeMu.Unlock()
		if empty {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("request stayed active")
}

// blockedHeaderBackend accepts requests, reads the body and never sends response
// headers until the test releases it. Cancellation aborts the client side
// without the server racing to write an empty 200.
func blockedHeaderBackend(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server
}

// TestOperatorCancelPreHeader covers M15ek/M15em/M15en/M15er: a cancel before
// headers returns 502, stops the chain, skips breaker accounting, and logs once.
func TestOperatorCancelPreHeader(t *testing.T) {
	blocked := blockedHeaderBackend(t)
	fast := newLXJSONBackend(t, http.StatusOK, `{"ok":true}`)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	},
		lxAnthropicInstance(blocked.URL, "slow-inst", 5*time.Second, map[string]string{"sonnet": "model-a"}),
		lxAnthropicInstance(fast.server.URL, "fast-inst", 5*time.Second, map[string]string{"sonnet": "model-b"}),
	)
	hub := newMonitorHub()
	handler.monitor = hub
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	done := make(chan cancelResult, 1)
	go func() { done <- doCancelRequest(server.URL, `{"model":"sonnet","messages":[]}`) }()
	id := waitActiveID(t, hub)
	if !hub.cancelRequest(id) {
		t.Fatal("cancelRequest returned false")
	}
	result := <-done
	if result.err != nil {
		t.Fatalf("read body: %v", result.err)
	}
	if result.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", result.status)
	}
	if !strings.Contains(result.body, "request cancelled by operator") {
		t.Errorf("body = %q, want the cancellation error", result.body)
	}
	if fast.calls() != 0 {
		t.Errorf("second candidate calls = %d, want 0", fast.calls())
	}
	if failures := streamInstanceFailures(handler, protocolAnthropic, 0); failures != 0 {
		t.Errorf("instance failures = %d, want 0", failures)
	}
	if _, degradedUntil := lxState(handler, protocolAnthropic, 0); !degradedUntil.IsZero() {
		t.Errorf("degradedUntil = %v, want zero", degradedUntil)
	}
	want := fmt.Sprintf("request %s cancelled by operator inst=slow-inst phase=pre-header", id)
	if got := strings.Count(logs(), want); got != 1 {
		t.Errorf("cancel log lines = %d, want 1 (%q):\n%s", got, want, logs())
	}
	if hub.cancelRequest(id) {
		t.Error("cancelling a finished request returned true")
	}
}

// TestOperatorCancelCommittedStream covers M15el/M15er: once headers are
// committed the client connection is truncated without a pause_turn tail.
func TestOperatorCancelCommittedStream(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseFn := func() { once.Do(func() { close(release) }) }
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, tiMessageStart()+tiText(0, "hello"))
		if flusher != nil {
			flusher.Flush()
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		releaseFn()
		backend.Close()
	})
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(backend.URL, "stream-inst", 5*time.Second, map[string]string{"sonnet": "model-a"}))
	hub := newMonitorHub()
	handler.monitor = hub
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	done := make(chan cancelResult, 1)
	go func() { done <- doCancelRequest(server.URL, `{"model":"sonnet","messages":[]}`) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("stream never started")
	}
	// Give the gateway time to commit the response headers to the client.
	time.Sleep(50 * time.Millisecond)
	id := waitActiveID(t, hub)
	if !hub.cancelRequest(id) {
		t.Fatal("cancelRequest returned false")
	}
	result := <-done
	if result.err == nil {
		t.Errorf("client read error = nil, want a truncated stream (body %q)", result.body)
	}
	if result.status != http.StatusOK {
		t.Errorf("status = %d, want the committed 200", result.status)
	}
	if strings.Contains(result.body, "pause_turn") {
		t.Errorf("cancelled committed stream carried pause_turn:\n%s", result.body)
	}
	if failures := streamInstanceFailures(handler, protocolAnthropic, 0); failures != 0 {
		t.Errorf("instance failures = %d, want 0", failures)
	}
	want := fmt.Sprintf("request %s cancelled by operator inst=stream-inst phase=streaming", id)
	if got := strings.Count(logs(), want); got != 1 {
		t.Errorf("cancel log lines = %d, want 1 (%q):\n%s", got, want, logs())
	}
}

// cancelWS is a minimal RFC 6455 client for the monitor command channel.
type cancelWS struct {
	conn   net.Conn
	reader *bufio.Reader
}

func dialCancelWS(t *testing.T, serverURL string) *cancelWS {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		conn.Close()
		t.Fatalf("rand: %v", err)
	}
	request := fmt.Sprintf("GET /api/ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		u.Host, base64.StdEncoding.EncodeToString(key))
	if _, err := io.WriteString(conn, request); err != nil {
		conn.Close()
		t.Fatalf("write handshake: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		t.Fatalf("read handshake: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		conn.Close()
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	client := &cancelWS{conn: conn, reader: reader}
	t.Cleanup(func() { conn.Close() })
	return client
}

func (c *cancelWS) writeFrame(opcode byte, payload []byte) error {
	mask := [4]byte{0x01, 0x02, 0x03, 0x04}
	frame := []byte{0x80 | opcode, 0x80 | byte(len(payload))}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := c.conn.Write(frame)
	return err
}

func (c *cancelWS) readFrame(timeout time.Duration) (byte, []byte, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, nil, err
	}
	var header [2]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0f
	length := int64(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

func (c *cancelWS) waitForOpcode(want byte, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		opcode, payload, err := c.readFrame(time.Until(deadline))
		if err != nil {
			return false
		}
		if opcode == want {
			return true
		}
		if opcode == 0x9 {
			_ = c.writeFrame(0xA, payload)
		}
	}
	return false
}

// TestMonitorCancelWebSocketCommands covers M15ep plus the text-frame parsing
// rules: valid cancels work, malformed/unknown commands are ignored without
// dropping the connection, and unknown ids have no effect.
func TestMonitorCancelWebSocketCommands(t *testing.T) {
	blocked := blockedHeaderBackend(t)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(blocked.URL, "ws-inst", 5*time.Second, map[string]string{"sonnet": "model-a"}))
	hub := newMonitorHub()
	handler.monitor = hub
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	wsServer := httptest.NewServer(http.HandlerFunc(hub.serveWS))
	t.Cleanup(wsServer.Close)
	ws := dialCancelWS(t, wsServer.URL)

	start := func() (string, chan cancelResult) {
		done := make(chan cancelResult, 1)
		go func() { done <- doCancelRequest(server.URL, `{"model":"sonnet","messages":[]}`) }()
		return waitActiveID(t, hub), done
	}

	id1, done1 := start()
	for _, payload := range []string{
		"not-json",
		`{"type":"cancel"}`,
		`{"type":"other","id":"` + id1 + `"}`,
		`{"id":"` + id1 + `"}`,
	} {
		if err := ws.writeFrame(0x1, []byte(payload)); err != nil {
			t.Fatalf("write %q: %v", payload, err)
		}
	}
	if err := ws.writeFrame(0x9, []byte("alive")); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	if !ws.waitForOpcode(0xA, 2*time.Second) {
		t.Fatal("WebSocket connection was dropped after invalid commands")
	}
	if err := ws.writeFrame(0x1, []byte(`{"type":"cancel","id":"`+id1+`"}`)); err != nil {
		t.Fatalf("write cancel: %v", err)
	}
	result1 := <-done1
	if result1.status != http.StatusBadGateway || !strings.Contains(result1.body, "request cancelled by operator") {
		t.Errorf("first request = %d %q, want 502 cancellation", result1.status, result1.body)
	}
	waitNoActive(t, hub)

	id2, done2 := start()
	if hub.cancelRequest("missing-id") {
		t.Error("unknown id returned true")
	}
	if err := ws.writeFrame(0x1, []byte(`{"type":"cancel","id":"missing-id"}`)); err != nil {
		t.Fatalf("write unknown cancel: %v", err)
	}
	select {
	case result := <-done2:
		t.Fatalf("unknown id terminated the request: %d %q", result.status, result.body)
	case <-time.After(150 * time.Millisecond):
	}
	if err := ws.writeFrame(0x1, []byte(`{"type":"cancel","id":"`+id2+`"}`)); err != nil {
		t.Fatalf("write cancel: %v", err)
	}
	result2 := <-done2
	if result2.status != http.StatusBadGateway || !strings.Contains(result2.body, "request cancelled by operator") {
		t.Errorf("second request = %d %q, want 502 cancellation", result2.status, result2.body)
	}
}
