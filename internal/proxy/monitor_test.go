package proxy_test

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

// testWSClient is a minimal RFC 6455 client used to exercise /api/monitor.
type testWSClient struct {
	conn   net.Conn
	reader *bufio.Reader
}

// dialMonitor performs the WebSocket handshake. On a non-101 response the
// connection is closed and the response returned for status assertions.
func dialMonitor(t *testing.T, serverURL, rawQuery string, header map[string]string) (*testWSClient, *http.Response) {
	t.Helper()

	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		t.Fatalf("generate websocket key: %v", err)
	}
	target := "/api/ws"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	var request strings.Builder
	fmt.Fprintf(&request, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n", target, u.Host)
	fmt.Fprintf(&request, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", base64.StdEncoding.EncodeToString(keyBytes))
	for name, value := range header {
		fmt.Fprintf(&request, "%s: %s\r\n", name, value)
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		conn.Close()
		t.Fatalf("write websocket handshake: %v", err)
	}

	reader := bufio.NewReader(conn)
	req, _ := http.NewRequest(http.MethodGet, serverURL+target, nil)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close()
		t.Fatalf("read websocket handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		conn.Close()
		return nil, resp
	}
	client := &testWSClient{conn: conn, reader: reader}
	t.Cleanup(func() { conn.Close() })
	return client, resp
}

func (c *testWSClient) readEvent(timeout time.Duration) (map[string]any, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	payload, opcode, err := readServerFrame(c.reader)
	if err != nil {
		return nil, err
	}
	switch opcode {
	case 0x8:
		return nil, io.EOF
	case 0x9:
		if err := c.writeClientFrame(0xA, payload); err != nil {
			return nil, err
		}
		return c.readEvent(timeout)
	case 0xA:
		return c.readEvent(timeout)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("event is not JSON: %w", err)
	}
	return event, nil
}

func (c *testWSClient) writeClientFrame(opcode byte, payload []byte) error {
	mask := [4]byte{0x01, 0x02, 0x03, 0x04}
	frame := []byte{0x80 | opcode, 0x80 | byte(len(payload))}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := c.conn.Write(frame)
	return err
}

func readServerFrame(reader *bufio.Reader) ([]byte, byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, 0, err
	}
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return nil, 0, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return nil, 0, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return nil, 0, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, 0, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, opcode, nil
}

func readEventsUntilEnd(t *testing.T, client *testWSClient, timeout time.Duration) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var events []map[string]any
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for end event after %d events", len(events))
		}
		event, err := client.readEvent(remaining)
		if err != nil {
			t.Fatalf("read event %d: %v", len(events), err)
		}
		if event["type"] == "snapshot" || event["type"] == "stats" {
			continue
		}
		events = append(events, event)
		if event["type"] == "end" {
			return events
		}
	}
}

func eventType(t *testing.T, event map[string]any) string {
	t.Helper()
	value, ok := event["type"].(string)
	if !ok {
		t.Fatalf("event has no string type: %#v", event)
	}
	return value
}

func eventInt(t *testing.T, event map[string]any, key string) int64 {
	t.Helper()
	value, ok := event[key].(float64)
	if !ok {
		t.Fatalf("event field %q is not numeric: %#v", key, event)
	}
	return int64(value)
}

func eventString(t *testing.T, event map[string]any, key string) string {
	t.Helper()
	value, ok := event[key].(string)
	if !ok {
		t.Fatalf("event field %q is not a string: %#v", key, event)
	}
	return value
}

func monitorAdminQuery() string {
	return url.Values{"user": {"admin"}, "password": {"secret"}}.Encode()
}

// monitorSSEBackend emits each chunk with a flush and a pause so the proxy reads
// them as distinct upstream trunks.
func monitorSSEBackend(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range chunks {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(30 * time.Millisecond)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func monitorAnthropicConfig(backendURL, name string) string {
	return fmt.Sprintf(`{
		"listen": "127.0.0.1:18080",
		"auth_token": %q,
		"admin_user": "admin",
		"admin_password": "secret",
		"instances": {"anthropic": [{
			"name": %q,
			"base_url": %q,
			"api_key": "upstream-key",
			"roles": {"sonnet": "model-a"}
		}]}
	}`, serviceToken, name, backendURL)
}

// TestMonitorAuth covers §24.2 F4/A1: no admin password disables the endpoint
// (404), a failed handshake is rejected with 401 without upgrading, and the
// query fallback works for browsers that cannot set the Authorization header.
func TestMonitorAuth(t *testing.T) {
	disabled, _ := newAdminServer(t, `{
		"auth_token": "tok",
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
	}`)
	resp := send(t, http.MethodGet, disabled.URL+"/api/monitor", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status without admin password = %d, want 404", resp.StatusCode)
	}

	server, _ := newAdminServer(t, adminTestConfig)

	if _, resp := dialMonitor(t, server.URL, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without credentials = %d, want 401", resp.StatusCode)
	}
	if _, resp := dialMonitor(t, server.URL, url.Values{"user": {"admin"}, "password": {"wrong"}}.Encode(), nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status with wrong query credentials = %d, want 401", resp.StatusCode)
	}

	client, resp := dialMonitor(t, server.URL, monitorAdminQuery(), nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status with query credentials = %d, want 101", resp.StatusCode)
	}
	client.conn.Close()

	auth := base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	client, resp = dialMonitor(t, server.URL, "", map[string]string{"Authorization": "Basic " + auth})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status with Basic header = %d, want 101", resp.StatusCode)
	}
	client.conn.Close()
}

// TestMonitorQueryCredentialFallback covers the frontend auth chain: browsers
// may send a stale cached Authorization header, so valid query credentials must
// still be accepted (the header and query are an OR).
func TestMonitorQueryCredentialFallback(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	stale := base64.StdEncoding.EncodeToString([]byte("admin:stale-password"))
	client, resp := dialMonitor(t, server.URL, monitorAdminQuery(), map[string]string{
		"Authorization": "Basic " + stale,
	})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status with stale header + valid query = %d, want 101", resp.StatusCode)
	}
	client.conn.Close()
}

// TestMonitorStreamEvents covers §24.2 F3/A3: one start, one chunk per upstream
// trunk, and a final end whose chunk count matches.
func TestMonitorStreamEvents(t *testing.T) {
	const deltas = 4
	chunks := make([]string, 0, deltas)
	for i := 0; i < deltas-1; i++ {
		chunks = append(chunks, fmt.Sprintf(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tok%d"}}

`, i))
	}
	// The last trunk carries the final delta and the terminating events in one
	// upstream write, so four writes produce exactly four chunk events.
	chunks = append(chunks, `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tok3"}}

event: message_delta
data: {"type":"message_delta","usage":{"input_tokens":11,"output_tokens":6}}

event: message_stop
data: {"type":"message_stop"}

`)
	backend := monitorSSEBackend(t, chunks)
	server, _ := newAdminServer(t, monitorAnthropicConfig(backend.URL, "MTI"))

	client, resp := dialMonitor(t, server.URL, monitorAdminQuery(), nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket handshake status = %d, want 101", resp.StatusCode)
	}

	stream := send(t, http.MethodPost, server.URL+"/v1/messages",
		`{"model":"sonnet","messages":[],"stream":true}`, withAuth(config.ProtocolAnthropic, nil))
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", stream.StatusCode)
	}
	_, _ = io.Copy(io.Discard, stream.Body)

	events := readEventsUntilEnd(t, client, 10*time.Second)
	if got := eventType(t, events[0]); got != "start" {
		t.Fatalf("first event type = %q, want start", got)
	}
	last := events[len(events)-1]
	if got := eventType(t, last); got != "end" {
		t.Fatalf("last event type = %q, want end", got)
	}

	var chunkEvents []map[string]any
	for _, event := range events {
		if eventType(t, event) == "chunk" {
			chunkEvents = append(chunkEvents, event)
		}
	}
	if len(chunkEvents) != deltas {
		t.Fatalf("chunk events = %d, want %d (upstream trunks)", len(chunkEvents), deltas)
	}
	var deltaSum int64
	for i, event := range chunkEvents {
		if got := eventInt(t, event, "seq"); got != int64(i) {
			t.Errorf("chunk %d seq = %d, want %d", i, got, i)
		}
		if got := eventInt(t, event, "bytes"); got <= 0 {
			t.Errorf("chunk %d bytes = %d, want >0", i, got)
		}
		if got := eventInt(t, event, "tsEnd"); got < eventInt(t, event, "tsStart") {
			t.Errorf("chunk %d tsEnd < tsStart", i)
		}
		deltaSum += eventInt(t, event, "deltaTokens")
	}
	if deltaSum != 6 {
		t.Errorf("sum of deltaTokens = %d, want 6 (usage output_tokens)", deltaSum)
	}

	start := events[0]
	if got := eventString(t, start, "path"); got != "/v1/messages" {
		t.Errorf("start.path = %q, want /v1/messages", got)
	}
	if got := eventString(t, start, "protocol"); got != "anthropic" {
		t.Errorf("start.protocol = %q, want anthropic", got)
	}
	if got := eventString(t, start, "reqModel"); got != "sonnet" {
		t.Errorf("start.reqModel = %q, want sonnet", got)
	}
	if got := eventString(t, start, "instName"); got != "MTI" {
		t.Errorf("start.instName = %q, want MTI", got)
	}
	if got := eventString(t, start, "upstreamModel"); got != "model-a" {
		t.Errorf("start.upstreamModel = %q, want model-a", got)
	}

	if got := eventInt(t, last, "status"); got != http.StatusOK {
		t.Errorf("end.status = %d, want 200", got)
	}
	if got := eventInt(t, last, "chunks"); got != deltas {
		t.Errorf("end.chunks = %d, want %d", got, deltas)
	}
	if got := eventInt(t, last, "inTokens"); got != 11 {
		t.Errorf("end.inTokens = %d, want 11", got)
	}
	if got := eventInt(t, last, "outTokens"); got != 6 {
		t.Errorf("end.outTokens = %d, want 6", got)
	}
	if _, ok := last["error"]; ok {
		t.Errorf("successful end event carries an error: %#v", last)
	}
}

// TestMonitorNonAIRequestsSilent covers §24.2 F2/A4: favicon-like calls and 401
// calls must not produce any event; only the routed AI request appears.
func TestMonitorNonAIRequestsSilent(t *testing.T) {
	backend, _, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	server, _ := newAdminServer(t, monitorAnthropicConfig(backend.URL, "MTI"))

	client, resp := dialMonitor(t, server.URL, monitorAdminQuery(), nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket handshake status = %d, want 101", resp.StatusCode)
	}

	favicon := send(t, http.MethodGet, server.URL+"/favicon.ico", "", withAuth(config.ProtocolAnthropic, nil))
	_, _ = io.Copy(io.Discard, favicon.Body)
	rejected := send(t, http.MethodPost, server.URL+"/v1/messages", `{"model":"sonnet"}`, nil)
	_, _ = io.Copy(io.Discard, rejected.Body)

	ok := send(t, http.MethodPost, server.URL+"/v1/messages", `{"model":"sonnet"}`, withAuth(config.ProtocolAnthropic, nil))
	_, _ = io.Copy(io.Discard, ok.Body)

	events := readEventsUntilEnd(t, client, 5*time.Second)
	var starts []map[string]any
	id := ""
	for _, event := range events {
		if eventType(t, event) == "start" {
			starts = append(starts, event)
			id = eventString(t, event, "id")
		}
	}
	if len(starts) != 1 {
		t.Fatalf("start events = %d, want 1 (non-AI calls must emit nothing)", len(starts))
	}
	if got := eventString(t, starts[0], "path"); got != "/v1/messages" {
		t.Fatalf("start.path = %q, want the routed AI request", got)
	}
	for _, event := range events {
		if got := eventString(t, event, "id"); got != id {
			t.Fatalf("event from an unexpected request: %#v", event)
		}
	}
}

// TestMonitorRetryThen502 covers §24.2 F3/A5: retry events follow candidate
// order and the end event reports 502 when every candidate fails.
func TestMonitorRetryThen502(t *testing.T) {
	closed := func() string {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		closedURL := server.URL
		server.Close()
		return closedURL
	}
	configJSON := fmt.Sprintf(`{
		"listen": "127.0.0.1:18080",
		"auth_token": %q,
		"admin_user": "admin",
		"admin_password": "secret",
		"instances": {"openai": [
			{"name": "A", "base_url": %q, "api_key": "k", "models": ["gpt-x"]},
			{"name": "B", "base_url": %q, "api_key": "k", "models": ["gpt-x"]},
			{"name": "C", "base_url": %q, "api_key": "k", "models": ["gpt-x"]}
		]}
	}`, serviceToken, closed(), closed(), closed())
	server, _ := newAdminServer(t, configJSON)

	client, resp := dialMonitor(t, server.URL, monitorAdminQuery(), nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket handshake status = %d, want 101", resp.StatusCode)
	}

	failed := send(t, http.MethodPost, server.URL+"/v1/chat/completions", `{"model":"gpt-x"}`,
		withAuth(config.ProtocolOpenAI, nil))
	if failed.StatusCode != http.StatusBadGateway {
		t.Fatalf("proxy status = %d, want 502", failed.StatusCode)
	}
	_, _ = io.Copy(io.Discard, failed.Body)

	events := readEventsUntilEnd(t, client, 5*time.Second)
	var retries []map[string]any
	for _, event := range events {
		switch eventType(t, event) {
		case "retry":
			retries = append(retries, event)
		case "chunk":
			t.Fatalf("chunk event for a pre-response failure: %#v", event)
		}
	}
	if len(retries) != 2 {
		t.Fatalf("retry events = %d, want 2 (three candidates)", len(retries))
	}
	for i, want := range []string{"B", "C"} {
		if got := eventString(t, retries[i], "instName"); got != want {
			t.Errorf("retry %d instName = %q, want %q", i, got, want)
		}
		if got := eventString(t, retries[i], "reason"); got != "error" {
			t.Errorf("retry %d reason = %q, want error", i, got)
		}
	}
	end := events[len(events)-1]
	if got := eventType(t, end); got != "end" {
		t.Fatalf("last event = %q, want end", got)
	}
	if got := eventInt(t, end, "status"); got != http.StatusBadGateway {
		t.Errorf("end.status = %d, want 502", got)
	}
	if got := eventString(t, end, "error"); got == "" {
		t.Error("end.error is empty for a failed request")
	}
	if got := eventString(t, end, "instName"); got != "C" {
		t.Errorf("end.instName = %q, want C (last attempted)", got)
	}
}

// TestMonitorFirstTokenTimeoutRetry covers the §24.2 F3 retry reason carried by
// the first-token-timeout path: the next candidate is reported and then used.
func TestMonitorFirstTokenTimeoutRetry(t *testing.T) {
	silent, silentCalls := newSilentSSEBackend(t, "")
	fast, _, fastCalls := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	configJSON := fmt.Sprintf(`{
		"listen": "127.0.0.1:18080",
		"auth_token": %q,
		"admin_user": "admin",
		"admin_password": "secret",
		"fallback": {"first_token_timeout": "50ms"},
		"instances": {"anthropic": [
			{"name": "SILENT", "base_url": %q, "api_key": "k", "roles": {"sonnet": "model-a"}},
			{"name": "FAST", "base_url": %q, "api_key": "k", "roles": {"sonnet": "model-b"}}
		]}
	}`, serviceToken, silent.URL, fast.URL)
	server, _ := newAdminServer(t, configJSON)

	client, resp := dialMonitor(t, server.URL, monitorAdminQuery(), nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket handshake status = %d, want 101", resp.StatusCode)
	}

	ok := send(t, http.MethodPost, server.URL+"/v1/messages", `{"model":"sonnet"}`,
		withAuth(config.ProtocolAnthropic, nil))
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200 after failover", ok.StatusCode)
	}
	_, _ = io.Copy(io.Discard, ok.Body)
	if got := silentCalls(); got != 1 {
		t.Errorf("silent backend calls = %d, want 1", got)
	}
	if got := fastCalls(); got != 1 {
		t.Errorf("fast backend calls = %d, want 1", got)
	}

	events := readEventsUntilEnd(t, client, 5*time.Second)
	var retry map[string]any
	for _, event := range events {
		if eventType(t, event) == "retry" {
			retry = event
		}
	}
	if retry == nil {
		t.Fatalf("no retry event: %#v", events)
	}
	if got := eventString(t, retry, "instName"); got != "FAST" {
		t.Errorf("retry.instName = %q, want FAST", got)
	}
	if got := eventString(t, retry, "reason"); got != "first_token_timeout" {
		t.Errorf("retry.reason = %q, want first_token_timeout", got)
	}
	end := events[len(events)-1]
	if got := eventInt(t, end, "status"); got != http.StatusOK {
		t.Errorf("end.status = %d, want 200", got)
	}
	if got := eventString(t, end, "instName"); got != "FAST" {
		t.Errorf("end.instName = %q, want FAST", got)
	}
}

// TestWSHistoryOnlyViaSnapshot replaces the pre-§24.11 no-history-replay test.
// History is now delivered once inside the snapshot frame (§24.11 H1); it must
// never be replayed as live start/chunk/end events.
func TestWSHistoryOnlyViaSnapshot(t *testing.T) {
	backend, _, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	server, _ := newAdminServer(t, monitorAnthropicConfig(backend.URL, "MTI"))

	first := send(t, http.MethodPost, server.URL+"/v1/messages", `{"model":"sonnet"}`, withAuth(config.ProtocolAnthropic, nil))
	_, _ = io.Copy(io.Discard, first.Body)

	client, resp := dialMonitor(t, server.URL, monitorAdminQuery(), nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket handshake status = %d, want 101", resp.StatusCode)
	}
	snapshot, err := client.readEvent(5 * time.Second)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if got := eventType(t, snapshot); got != "snapshot" {
		t.Fatalf("first frame = %q, want snapshot", got)
	}
	requests, _ := snapshot["requests"].([]any)
	if len(requests) != 1 {
		t.Fatalf("snapshot.requests = %d, want 1 completed request", len(requests))
	}
	request, _ := requests[0].(map[string]any)
	if ended, _ := request["ended"].(bool); !ended {
		t.Errorf("snapshot request ended = %v, want true", request["ended"])
	}
	// The initial stats frame follows the snapshot; it is not a replay.
	statsEvent, err := client.readEvent(5 * time.Second)
	if err != nil {
		t.Fatalf("read initial stats frame: %v", err)
	}
	if got := eventType(t, statsEvent); got != "stats" {
		t.Fatalf("second frame = %q, want stats", got)
	}
	if event, err := client.readEvent(300 * time.Millisecond); err == nil {
		t.Fatalf("received a replayed live event after snapshot: %#v", event)
	} else {
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("expected a read timeout, got %v", err)
		}
	}

	second := send(t, http.MethodPost, server.URL+"/v1/messages", `{"model":"sonnet"}`, withAuth(config.ProtocolAnthropic, nil))
	_, _ = io.Copy(io.Discard, second.Body)
	events := readEventsUntilEnd(t, client, 5*time.Second)
	if got := eventType(t, events[0]); got != "start" {
		t.Fatalf("first live event = %q, want start", got)
	}
}

// TestRemovedMonitorRoute covers §24.11 H2: the old endpoint is gone (404 with
// valid credentials) while /api/ws still upgrades.
func TestRemovedMonitorRoute(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	resp := sendBasic(t, http.MethodGet, server.URL+"/api/monitor", "", "admin", "secret")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /api/monitor with credentials = %d, want 404", resp.StatusCode)
	}
	resp = send(t, http.MethodGet, server.URL+"/api/monitor", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/monitor without credentials = %d, want 401", resp.StatusCode)
	}
	client, wsResp := dialMonitor(t, server.URL, monitorAdminQuery(), nil)
	if wsResp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("GET /api/ws = %d, want 101", wsResp.StatusCode)
	}
	client.conn.Close()
}

// TestMonitorSurvivesReload covers the reload constraint: replacing the handler
// must keep the existing monitor connection alive and monitor new traffic.
func TestMonitorSurvivesReload(t *testing.T) {
	first, _, _ := newBackend(t, http.StatusOK, nil, `{"id":"first"}`)
	server, _ := newAdminServer(t, monitorOpenAIConfig(first.URL, "tok", "gpt-x"))

	client, resp := dialMonitor(t, server.URL, monitorAdminQuery(), nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket handshake status = %d, want 101", resp.StatusCode)
	}

	second, _, calls := newBackend(t, http.StatusOK, nil, `{"id":"second"}`)
	replacement := monitorOpenAIConfig(second.URL, "new-token", "new-model")
	if resp := sendBasic(t, http.MethodPut, server.URL+"/api/config", replacement, "admin", "secret"); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT config status = %d, want 200", resp.StatusCode)
	}
	if resp := sendBasic(t, http.MethodPost, server.URL+"/api/reload", "", "admin", "secret"); resp.StatusCode != http.StatusOK {
		t.Fatalf("reload status = %d, want 200", resp.StatusCode)
	}

	req := send(t, http.MethodPost, server.URL+"/v1/chat/completions", `{"model":"new-model"}`,
		map[string]string{"Authorization": "Bearer new-token"})
	if req.StatusCode != http.StatusOK {
		t.Fatalf("proxy status after reload = %d, want 200", req.StatusCode)
	}
	_, _ = io.Copy(io.Discard, req.Body)
	if got := calls(); got != 1 {
		t.Errorf("new backend calls = %d, want 1", got)
	}

	events := readEventsUntilEnd(t, client, 5*time.Second)
	start := events[0]
	if got := eventString(t, start, "reqModel"); got != "new-model" {
		t.Errorf("start.reqModel = %q, want new-model", got)
	}
	end := events[len(events)-1]
	if got := eventInt(t, end, "status"); got != http.StatusOK {
		t.Errorf("end.status = %d, want 200", got)
	}
}

func monitorOpenAIConfig(backendURL, token, model string) string {
	return fmt.Sprintf(`{
		"listen": "127.0.0.1:18080",
		"auth_token": %q,
		"admin_user": "admin",
		"admin_password": "secret",
		"instances": {"openai": [{
			"name": "backend",
			"base_url": %q,
			"api_key": "upstream-key",
			"models": [%q]
		}]}
	}`, token, backendURL, model)
}
