package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// §24.28 M15ee–M15ei: connection identifier in hdrprof and stream summaries.

type connTestBackend struct {
	server *httptest.Server
	mu     sync.Mutex
	remote []string
}

func newConnTestBackend(t *testing.T, delay time.Duration, contentType, body string) *connTestBackend {
	t.Helper()
	b := &connTestBackend{}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		b.mu.Lock()
		b.remote = append(b.remote, r.RemoteAddr)
		b.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *connTestBackend) remoteAddrs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.remote...)
}

func connLineFor(t *testing.T, logs, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, prefix) {
			return line
		}
	}
	t.Fatalf("missing %q line:\n%s", prefix, logs)
	return ""
}

var connPortRE = regexp.MustCompile(`\bconn=([0-9]+)\b`)

func connPort(t *testing.T, line string) string {
	t.Helper()
	match := connPortRE.FindStringSubmatch(line)
	if match == nil {
		t.Fatalf("line has no numeric conn: %s", line)
	}
	return match[1]
}

func addrPort(t *testing.T, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	return port
}

func assertFieldOrder(t *testing.T, line string, fields ...string) {
	t.Helper()
	previous := -1
	for _, field := range fields {
		index := strings.Index(line, field)
		if index < 0 {
			t.Fatalf("missing %q in line: %s", field, line)
		}
		if index < previous {
			t.Fatalf("%q is out of order in line: %s", field, line)
		}
		previous = index
	}
}

// TestHdrProfConnMatchesConnection covers M15ee: remote and conn describe the
// same connection, and the response is unchanged.
func TestHdrProfConnMatchesConnection(t *testing.T) {
	backend := newConnTestBackend(t, 300*time.Millisecond, "application/json", `{"ok":true}`)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(backend.server.URL, "conn-inst", 500*time.Millisecond, map[string]string{"sonnet": "model-a"}))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("response = %d %q, want 200 and unchanged bytes", status, body)
	}

	line := connLineFor(t, logs(), "hdrprof inst=conn-inst")
	if !strings.Contains(line, "remote=127.0.0.1:") {
		t.Errorf("hdrprof missing remote host:port: %s", line)
	}
	assertFieldOrder(t, line, "remote=", "conn=", "budget=500ms")
	if got := connPort(t, line); got != addrPort(t, backend.remoteAddrs()[0]) {
		t.Errorf("conn = %s, want the client port seen by the backend %s", got, backend.remoteAddrs()[0])
	}
}

// TestHdrProfConnUnknown covers M15ef: without GotConn the line prints conn=-
// and every other field keeps its format.
func TestHdrProfConnUnknown(t *testing.T) {
	logs := captureStreamLogs(t)
	profile := &headerProfile{}
	profile.log("unknown-inst", time.Now().Add(-time.Second), time.Now(), time.Second, 0, time.Second)

	line := connLineFor(t, logs(), "hdrprof inst=unknown-inst")
	if !strings.Contains(line, "conn=-") {
		t.Errorf("conn should be unknown: %s", line)
	}
	assertFieldOrder(t, line, "connect=", "tls=", "upload=", "server_wait=", "body_bytes=0", "reused=0", "remote=-", "conn=-", "budget=1s")
	if got := connPortRE.FindString(line); got != "" {
		t.Errorf("unknown connection printed a port: %s", line)
	}
}

// TestStreamPhaseConnField covers M15eg and M15ei: the stream line carries conn
// after last_gap, matching the backend connection, and bytes/status are
// unchanged.
func TestStreamPhaseConnField(t *testing.T) {
	payload := tiMessageStart() + tiText(0, "hello") + tiMessageDelta(2) + tiMessageStop()
	backend := newConnTestBackend(t, 0, "text/event-stream", payload)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, lxAnthropicInstance(backend.server.URL, "conn-stream", 5*time.Second, map[string]string{"sonnet": "model-a"}))
	handler.monitor = newMonitorHub()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK || body != payload {
		t.Fatalf("response = %d %q, want 200 and unchanged stream bytes", status, body)
	}

	line := connLineFor(t, logs(), "stream conn-stream hdr=")
	assertFieldOrder(t, line, "last_gap=", "conn=")
	if got := connPort(t, line); got != addrPort(t, backend.remoteAddrs()[0]) {
		t.Errorf("stream conn = %s, want the client port seen by the backend %s", got, backend.remoteAddrs()[0])
	}
	if recent, _ := reqShapeStats(t, handler); len(recent) != 0 {
		t.Errorf("recentErrors = %d, want 0", len(recent))
	}
}

// TestConnIdentityStableAndDistinct covers M15eh: sequential requests on one
// transport share the port; a fresh transport gets a different one.
func TestConnIdentityStableAndDistinct(t *testing.T) {
	backend := newConnTestBackend(t, 300*time.Millisecond, "application/json", `{"ok":true}`)
	newHandler := func(name string) *Handler {
		return newStreamHandler(t, Options{
			IdleTimeout:       5 * time.Second,
			FirstTokenTimeout: 5 * time.Second,
		}, lxAnthropicInstance(backend.server.URL, name, 500*time.Millisecond, map[string]string{"sonnet": "model-a"}))
	}
	first := newHandler("conn-a")
	firstServer := httptest.NewServer(first)
	t.Cleanup(firstServer.Close)
	second := newHandler("conn-b")
	secondServer := httptest.NewServer(second)
	t.Cleanup(secondServer.Close)
	logs := captureStreamLogs(t)

	for i := 0; i < 2; i++ {
		if status, _, err := doStreamRequest(t, firstServer.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`); err != nil || status != http.StatusOK {
			t.Fatalf("first handler request %d = %d, %v; want 200", i, status, err)
		}
	}
	if status, _, err := doStreamRequest(t, secondServer.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`); err != nil || status != http.StatusOK {
		t.Fatalf("second handler request = %d, %v; want 200", status, err)
	}

	var ports []string
	for _, line := range strings.Split(logs(), "\n") {
		if strings.Contains(line, "hdrprof inst=conn-a") {
			ports = append(ports, connPort(t, line))
		}
	}
	if len(ports) != 2 {
		t.Fatalf("conn-a hdrprof lines = %d, want 2:\n%s", len(ports), logs())
	}
	if ports[0] != ports[1] {
		t.Errorf("sequential requests on one transport used different conns: %v", ports)
	}
	lineB := connLineFor(t, logs(), "hdrprof inst=conn-b")
	portB := connPort(t, lineB)
	if portB == ports[0] {
		t.Errorf("a fresh transport reused conn=%s, want a distinct port", portB)
	}

	remotes := backend.remoteAddrs()
	if len(remotes) != 3 {
		t.Fatalf("backend requests = %d, want 3", len(remotes))
	}
	if addrPort(t, remotes[0]) != addrPort(t, remotes[1]) {
		t.Errorf("the first two backend requests did not reuse one connection: %v", remotes[:2])
	}
	if addrPort(t, remotes[2]) == addrPort(t, remotes[0]) {
		t.Errorf("the fresh transport reused the same backend connection: %v", remotes)
	}
	if ports[0] != addrPort(t, remotes[0]) || portB != addrPort(t, remotes[2]) {
		t.Errorf("conn values do not match backend client ports: conns=%v remotes=%v", append(ports, portB), remotes)
	}
}
