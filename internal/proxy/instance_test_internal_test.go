package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"model-router/internal/config"
)

// §24.32 M173–M186: instance connectivity test.

// itestRequest is one recorded upstream test request.
type itestRequest struct {
	method string
	path   string
	body   string
	header http.Header
}

// itestBackend is a fake upstream that records every request.
type itestBackend struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []itestRequest
}

func newITestBackend(t *testing.T, fn http.HandlerFunc) *itestBackend {
	t.Helper()
	b := &itestBackend{}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.requests = append(b.requests, itestRequest{
			method: r.Method,
			path:   r.URL.Path,
			body:   string(body),
			header: r.Header.Clone(),
		})
		b.mu.Unlock()
		fn(w, r)
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *itestBackend) calls() []itestRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]itestRequest, len(b.requests))
	copy(out, b.requests)
	return out
}

func (b *itestBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}

// itestSSEHandler answers with a minimal well-formed stream.
func itestSSEHandler(proto protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if proto == protocolAnthropic {
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"chunk-1\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chunk-2\"}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
}

func itestConfig(instances ...config.Instance) *config.Config {
	return &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances:     instances,
	}
}

func itestInstance(proto protocol, name, baseURL string, timeout time.Duration,
	models []string, roles map[string]string) config.Instance {
	return config.Instance{
		Protocol: string(proto),
		Name:     name,
		BaseURL:  baseURL,
		APIKey:   "upstream-" + name,
		Timeout:  config.Duration(timeout),
		Models:   models,
		Roles:    roles,
	}
}

func itestServer(t *testing.T, cfg *config.Config) (*Server, *httptest.Server) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	server, err := NewServer(configPath, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return server, httpServer
}

func itestRuntime(t *testing.T, server *Server, proto protocol, name string) *instanceRuntime {
	t.Helper()
	for _, inst := range server.state.Load().handler.byProtocol[proto] {
		if inst.name == name {
			return inst
		}
	}
	t.Fatalf("instance %s not found", name)
	return nil
}

// itestCall invokes POST /api/instances/test and decodes a successful body.
func itestCall(t *testing.T, base, body string, auth bool) (int, instanceTestResult, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+PathInstancesTest, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build test request: %v", err)
	}
	if auth {
		req.SetBasicAuth("admin", "secret")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", PathInstancesTest, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var payload instanceTestResult
	_ = json.Unmarshal(raw, &payload)
	return resp.StatusCode, payload, string(raw)
}

type itestStatsRow struct {
	Name          string `json:"name"`
	Protocol      string `json:"protocol"`
	Requests      int64  `json:"requests"`
	Success       int64  `json:"success"`
	Failed        int64  `json:"failed"`
	InTokens      int64  `json:"inTokens"`
	OutTokens     int64  `json:"outTokens"`
	State         string `json:"state"`
	Reason        string `json:"reason"`
	DegradedSince int64  `json:"degradedSince"`
}

type itestRecentError struct {
	InstName string `json:"instName"`
	Error    string `json:"error"`
}

type itestStatsPayload struct {
	Requests struct {
		Total   int64 `json:"total"`
		Success int64 `json:"success"`
		Failed  int64 `json:"failed"`
	} `json:"requests"`
	InTokens     int64              `json:"inTokens"`
	OutTokens    int64              `json:"outTokens"`
	Instances    []itestStatsRow    `json:"instances"`
	RecentErrors []itestRecentError `json:"recentErrors"`
}

func itestGetStats(t *testing.T, base string) itestStatsPayload {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+PathStats, nil)
	if err != nil {
		t.Fatalf("build stats request: %v", err)
	}
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/stats = %d, want 200", resp.StatusCode)
	}
	var payload itestStatsPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	return payload
}

func itestRow(rows []itestStatsRow, name string) *itestStatsRow {
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

// TestInstanceTestModelResolution covers §24.32 M181: models[0] wins, then the
// fixed role priority, then any role key.
func TestInstanceTestModelResolution(t *testing.T) {
	cases := []struct {
		name   string
		inst   *instanceRuntime
		want   string
		wantOK bool
	}{
		{
			name:   "first model wins",
			inst:   &instanceRuntime{models: []string{"m1", "m2"}},
			want:   "m1",
			wantOK: true,
		},
		{
			name:   "wildcard role outranks concrete roles",
			inst:   &instanceRuntime{roles: map[string]string{"sonnet": "s", "*": "w"}},
			want:   "w",
			wantOK: true,
		},
		{
			name:   "sonnet before fable",
			inst:   &instanceRuntime{roles: map[string]string{"fable": "f", "sonnet": "s"}},
			want:   "s",
			wantOK: true,
		},
		{
			name:   "fable before opus",
			inst:   &instanceRuntime{roles: map[string]string{"opus": "o", "fable": "f"}},
			want:   "f",
			wantOK: true,
		},
		{
			name:   "opus before haiku",
			inst:   &instanceRuntime{roles: map[string]string{"haiku": "h", "opus": "o"}},
			want:   "o",
			wantOK: true,
		},
		{
			name:   "haiku is the last named role",
			inst:   &instanceRuntime{roles: map[string]string{"haiku": "h"}},
			want:   "h",
			wantOK: true,
		},
		{
			name:   "any remaining role is taken deterministically",
			inst:   &instanceRuntime{roles: map[string]string{"zeta": "z", "alpha": "a"}},
			want:   "a",
			wantOK: true,
		},
		{
			name:   "no model declared",
			inst:   &instanceRuntime{},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, ok := instanceTestModel(tc.inst)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if model != tc.want {
				t.Fatalf("model = %q, want %q", model, tc.want)
			}
		})
	}
}

// TestInstanceTestSuccess covers §24.32 M15gk/M15gm: the probe is a direct
// streaming call to the instance and success returns every documented field.
func TestInstanceTestSuccess(t *testing.T) {
	t.Run("openai", func(t *testing.T) {
		backend := newITestBackend(t, itestSSEHandler(protocolOpenAI))
		cfg := itestConfig(itestInstance(protocolOpenAI, "oa", backend.server.URL, 2*time.Second,
			[]string{"model-a"}, nil))
		_, httpServer := itestServer(t, cfg)

		code, result, raw := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"oa"}`, true)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if !result.OK || result.Status != http.StatusOK || result.Error != "" {
			t.Fatalf("result = %+v, want ok/200/empty error", result)
		}
		if result.Name != "oa" || result.Protocol != config.ProtocolOpenAI {
			t.Errorf("name/protocol = %q/%q", result.Name, result.Protocol)
		}
		if result.State != stateClosed {
			t.Errorf("state = %q, want closed", result.State)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		for _, key := range []string{"name", "protocol", "ok", "status", "durationMs", "state", "error"} {
			if _, ok := fields[key]; !ok {
				t.Errorf("response missing field %q: %s", key, raw)
			}
		}
		if _, ok := fields["balance"]; ok {
			t.Errorf("instance without balance must omit the balance field: %s", raw)
		}

		calls := backend.calls()
		if len(calls) != 1 {
			t.Fatalf("upstream calls = %d, want 1", len(calls))
		}
		call := calls[0]
		if call.method != http.MethodPost || call.path != PathOpenAI {
			t.Errorf("request = %s %s, want POST %s", call.method, call.path, PathOpenAI)
		}
		if got := call.header.Get("Authorization"); got != "Bearer upstream-oa" {
			t.Errorf("Authorization = %q", got)
		}
		if got := call.header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		var body instanceTestBody
		if err := json.Unmarshal([]byte(call.body), &body); err != nil {
			t.Fatalf("decode probe body: %v", err)
		}
		if body.Model != "model-a" || body.MaxTokens > 32 || !body.Stream {
			t.Errorf("probe body = %+v, want model-a / max_tokens<=32 / stream", body)
		}
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" ||
			len(body.Messages[0].Content) < 40000 || !strings.HasSuffix(body.Messages[0].Content, "reply with OK") {
			t.Errorf("probe messages = role %q, content len %d, want a >=40k context ending in the instruction",
				body.Messages[0].Role, len(body.Messages[0].Content))
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		backend := newITestBackend(t, itestSSEHandler(protocolAnthropic))
		cfg := itestConfig(itestInstance(protocolAnthropic, "an", backend.server.URL, 2*time.Second,
			nil, map[string]string{"sonnet": "claude-sonnet-x"}))
		_, httpServer := itestServer(t, cfg)

		code, result, _ := itestCall(t, httpServer.URL, `{"protocol":"anthropic","name":"an"}`, true)
		if code != http.StatusOK || !result.OK || result.State != stateClosed {
			t.Fatalf("status = %d result = %+v", code, result)
		}
		call := backend.calls()[0]
		if call.path != PathAnthropic {
			t.Errorf("path = %q, want %q", call.path, PathAnthropic)
		}
		if got := call.header.Get("x-api-key"); got != "upstream-an" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := call.header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		var body instanceTestBody
		_ = json.Unmarshal([]byte(call.body), &body)
		if body.Model != "claude-sonnet-x" {
			t.Errorf("model = %q, want the declared sonnet role", body.Model)
		}
	})

	t.Run("balance field", func(t *testing.T) {
		upstream := newITestBackend(t, itestSSEHandler(protocolOpenAI))
		balance := newBalBackend(t, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"51.01"}]}`)
		inst := itestInstance(protocolOpenAI, "oa", upstream.server.URL, 2*time.Second,
			[]string{"model-a"}, nil)
		inst.Balance = &config.Balance{Kind: config.BalanceKindDeepSeek, URL: balance.server.URL, Interval: balNoPoll()}
		_, httpServer := itestServer(t, itestConfig(inst))

		code, result, raw := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"oa"}`, true)
		if code != http.StatusOK || !result.OK {
			t.Fatalf("status = %d result = %+v", code, result)
		}
		if result.Balance == nil {
			t.Fatalf("balance field missing: %s", raw)
		}
		if result.Balance.Kind != config.BalanceKindDeepSeek {
			t.Errorf("balance.kind = %q", result.Balance.Kind)
		}
	})
}

// TestInstanceTestRejectsBadRequests covers §24.32 M15gl: invalid protocol 400,
// unknown name 404 and the lock/unlock authentication gate.
func TestInstanceTestRejectsBadRequests(t *testing.T) {
	backend := newITestBackend(t, itestSSEHandler(protocolOpenAI))
	cfg := itestConfig(itestInstance(protocolOpenAI, "oa", backend.server.URL, 2*time.Second,
		[]string{"model-a"}, nil))
	_, httpServer := itestServer(t, cfg)

	t.Run("invalid protocol", func(t *testing.T) {
		code, _, _ := itestCall(t, httpServer.URL, `{"protocol":"gemini","name":"oa"}`, true)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
	})
	t.Run("unknown instance", func(t *testing.T) {
		code, _, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"missing"}`, true)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
	})
	t.Run("missing name", func(t *testing.T) {
		code, _, _ := itestCall(t, httpServer.URL, `{"protocol":"openai"}`, true)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
	})
	t.Run("unauthorized", func(t *testing.T) {
		code, _, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"oa"}`, false)
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})
	t.Run("method not allowed", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, httpServer.URL+PathInstancesTest, nil)
		req.SetBasicAuth("admin", "secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
	})
}

// TestInstanceTestUpstreamFailure covers §24.32 M15gn/M15go: a non-2xx answer
// and an unreachable instance both fail the test and open the breaker.
func TestInstanceTestUpstreamFailure(t *testing.T) {
	t.Run("non-2xx", func(t *testing.T) {
		bad := newITestBackend(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		})
		peer := newITestBackend(t, itestSSEHandler(protocolOpenAI))
		cfg := itestConfig(
			itestInstance(protocolOpenAI, "bad", bad.server.URL, 2*time.Second, []string{"model-a"}, nil),
			itestInstance(protocolOpenAI, "peer", peer.server.URL, 2*time.Second, []string{"model-a"}, nil),
		)
		_, httpServer := itestServer(t, cfg)

		code, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"bad"}`, true)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if result.OK {
			t.Fatalf("result = %+v, want ok=false", result)
		}
		if result.Status != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", result.Status)
		}
		if result.Error == "" || !strings.Contains(result.Error, "upstream status 500") {
			t.Errorf("error = %q, want a non-empty upstream status", result.Error)
		}
		if result.State != stateOpen {
			t.Errorf("state = %q, want open", result.State)
		}
		row := itestRow(itestGetStats(t, httpServer.URL).Instances, "bad")
		if row == nil || row.State != stateOpen || row.Reason != reasonTestFailed {
			t.Fatalf("stats row = %+v, want open/test_failed", row)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURL := dead.URL
		dead.Close()
		peer := newITestBackend(t, itestSSEHandler(protocolOpenAI))
		cfg := itestConfig(
			itestInstance(protocolOpenAI, "gone", deadURL, 2*time.Second, []string{"model-a"}, nil),
			itestInstance(protocolOpenAI, "peer", peer.server.URL, 2*time.Second, []string{"model-a"}, nil),
		)
		_, httpServer := itestServer(t, cfg)

		_, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"gone"}`, true)
		if result.OK || result.Status != 0 || result.Error == "" {
			t.Fatalf("result = %+v, want ok=false status=0 with error", result)
		}
		if result.State != stateOpen {
			t.Errorf("state = %q, want open", result.State)
		}
	})
}

// TestInstanceTestTimeout covers §24.32 M15gp: an upstream that never sends
// response headers still returns within the instance timeout.
func TestInstanceTestTimeout(t *testing.T) {
	block := make(chan struct{})
	backend := newITestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		<-block
	})
	t.Cleanup(func() { close(block) })
	peer := newITestBackend(t, itestSSEHandler(protocolOpenAI))
	cfg := itestConfig(
		itestInstance(protocolOpenAI, "slow", backend.server.URL, 150*time.Millisecond, []string{"model-a"}, nil),
		itestInstance(protocolOpenAI, "peer", peer.server.URL, 2*time.Second, []string{"model-a"}, nil),
	)
	_, httpServer := itestServer(t, cfg)

	start := time.Now()
	code, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"slow"}`, true)
	elapsed := time.Since(start)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if result.OK || result.Status != 0 {
		t.Fatalf("result = %+v, want ok=false status=0", result)
	}
	if !strings.HasPrefix(result.Error, "test timeout after") {
		t.Fatalf("error = %q, want the test timeout prefix", result.Error)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("test took %s, want the 150ms budget to bound it", elapsed)
	}
	if result.State != stateOpen {
		t.Errorf("state = %q, want open", result.State)
	}
}

// TestInstanceTestStreamTruncated covers §24.32 M15gq: 2xx headers followed by
// an interrupted stream are a failure.
func TestInstanceTestStreamTruncated(t *testing.T) {
	truncated := newITestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			panic(http.ErrAbortHandler)
		}
		conn, buf, err := hijacker.Hijack()
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		_, _ = io.WriteString(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 500\r\nConnection: close\r\n\r\n")
		_, _ = io.WriteString(buf, "data: {\"id\":\"one\"}\n\n")
		_ = buf.Flush()
		_ = conn.Close()
	})
	peer := newITestBackend(t, itestSSEHandler(protocolOpenAI))
	cfg := itestConfig(
		itestInstance(protocolOpenAI, "trunc", truncated.server.URL, 2*time.Second, []string{"model-a"}, nil),
		itestInstance(protocolOpenAI, "peer", peer.server.URL, 2*time.Second, []string{"model-a"}, nil),
	)
	_, httpServer := itestServer(t, cfg)

	code, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"trunc"}`, true)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if result.OK || result.Status != http.StatusOK || result.Error == "" {
		t.Fatalf("result = %+v, want ok=false at 200 with an error", result)
	}
	if result.State != stateOpen {
		t.Errorf("state = %q, want open", result.State)
	}
}

// TestInstanceTestRestoresOpenInstance covers §24.32 M15gr: success clears the
// whole breaker bookkeeping.
func TestInstanceTestRestoresOpenInstance(t *testing.T) {
	backend := newITestBackend(t, itestSSEHandler(protocolOpenAI))
	peer := newITestBackend(t, itestSSEHandler(protocolOpenAI))
	cfg := itestConfig(
		itestInstance(protocolOpenAI, "oa", backend.server.URL, 2*time.Second, []string{"model-a"}, nil),
		itestInstance(protocolOpenAI, "peer", peer.server.URL, 2*time.Second, []string{"model-a"}, nil),
	)
	server, httpServer := itestServer(t, cfg)
	inst := itestRuntime(t, server, protocolOpenAI, "oa")

	inst.recordFailure(inst.nowTime(), 1, time.Minute)
	if !inst.isOpen() {
		t.Fatal("precondition: the instance must be open")
	}

	_, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"oa"}`, true)
	if !result.OK || result.State != stateClosed {
		t.Fatalf("result = %+v, want ok/closed", result)
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.consecutiveFailures != 0 || !inst.degradedUntil.IsZero() ||
		!inst.degradedSince.IsZero() || inst.degradeReason != "" {
		t.Fatalf("state not cleared: failures=%d until=%s since=%s reason=%q",
			inst.consecutiveFailures, inst.degradedUntil, inst.degradedSince, inst.degradeReason)
	}
}

// TestInstanceTestRestoresExhausted covers §24.32 M15gs: a successful test also
// clears the quota exhaustion flag.
func TestInstanceTestRestoresExhausted(t *testing.T) {
	backend := newITestBackend(t, itestSSEHandler(protocolOpenAI))
	cfg := itestConfig(itestInstance(protocolOpenAI, "oa", backend.server.URL, 2*time.Second,
		[]string{"model-a"}, nil))
	server, httpServer := itestServer(t, cfg)
	inst := itestRuntime(t, server, protocolOpenAI, "oa")
	now := inst.nowTime()
	inst.mu.Lock()
	inst.quotaExhausted = true
	inst.quotaExhaustedSince = now
	inst.mu.Unlock()
	if inst.state() != stateExhausted {
		t.Fatal("precondition: the instance must be exhausted")
	}

	_, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"oa"}`, true)
	if !result.OK || result.State != stateClosed {
		t.Fatalf("result = %+v, want ok/closed", result)
	}
	inst.mu.Lock()
	exhausted, since := inst.quotaExhausted, inst.quotaExhaustedSince
	inst.mu.Unlock()
	if exhausted || !since.IsZero() {
		t.Fatalf("quota state not cleared: exhausted=%v since=%s", exhausted, since)
	}
	row := itestRow(itestGetStats(t, httpServer.URL).Instances, "oa")
	if row == nil || row.State != stateClosed || row.Reason != "" {
		t.Fatalf("stats row = %+v, want closed with an empty reason", row)
	}
}

// TestInstanceTestLockedUnchanged covers §24.32 M15gt: a pinned instance is
// still tested but its state is never touched.
func TestInstanceTestLockedUnchanged(t *testing.T) {
	bad := newITestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"down"}`)
	})
	peer := newITestBackend(t, itestSSEHandler(protocolOpenAI))
	cfg := itestConfig(
		itestInstance(protocolOpenAI, "oa", bad.server.URL, 2*time.Second, []string{"model-a"}, nil),
		itestInstance(protocolOpenAI, "peer", peer.server.URL, 2*time.Second, []string{"model-a"}, nil),
	)
	server, httpServer := itestServer(t, cfg)
	inst := itestRuntime(t, server, protocolOpenAI, "oa")
	ps := server.state.Load().handler.protocolRuntime(protocolOpenAI)
	if _, ok := ps.lockInstance("oa"); !ok {
		t.Fatal("precondition: lock failed")
	}
	beforeUntil := inst.nowTime().Add(time.Minute)
	inst.mu.Lock()
	inst.consecutiveFailures = 2
	inst.degradedUntil = beforeUntil
	inst.degradedSince = beforeUntil.Add(-time.Minute)
	inst.degradeReason = reasonFailureThreshold
	inst.quotaExhausted = true
	inst.quotaExhaustedSince = beforeUntil.Add(-2 * time.Minute)
	inst.mu.Unlock()

	captured := captureStreamLogs(t)
	code, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"oa"}`, true)
	logs := captured()

	if code != http.StatusOK || result.OK || result.Error == "" {
		t.Fatalf("status = %d result = %+v, want a failed probe", code, result)
	}
	// §24.32 M175: the response state mirrors /api/stats, where a pin outranks
	// the breaker and quota states.
	if result.State != stateLocked {
		t.Errorf("state = %q, want %q", result.State, stateLocked)
	}
	inst.mu.Lock()
	failures, until, since, reason := inst.consecutiveFailures, inst.degradedUntil, inst.degradedSince, inst.degradeReason
	exhausted, exhaustedSince := inst.quotaExhausted, inst.quotaExhaustedSince
	inst.mu.Unlock()
	if failures != 2 || !until.Equal(beforeUntil) || reason != reasonFailureThreshold ||
		!exhausted || !exhaustedSince.Equal(beforeUntil.Add(-2*time.Minute)) {
		t.Fatalf("locked instance state changed: failures=%d until=%s since=%s reason=%q exhausted=%v",
			failures, until, since, reason, exhausted)
	}
	if !strings.Contains(logs, "instance oa test failed in") || !strings.Contains(logs, "(locked, state unchanged)") {
		t.Errorf("missing locked test log line:\n%s", logs)
	}
}

// TestInstanceTestSingleInstanceProtocol covers §24.32 M15gu: a one-instance
// protocol never opens, but the API still reports the honest failure.
func TestInstanceTestSingleInstanceProtocol(t *testing.T) {
	bad := newITestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"down"}`)
	})
	cfg := itestConfig(itestInstance(protocolOpenAI, "solo", bad.server.URL, 2*time.Second,
		[]string{"model-a"}, nil))
	server, httpServer := itestServer(t, cfg)
	inst := itestRuntime(t, server, protocolOpenAI, "solo")

	code, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"solo"}`, true)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if result.OK || result.Error == "" || !strings.Contains(result.Error, "upstream status 503") {
		t.Fatalf("result = %+v, want the honest upstream failure", result)
	}
	if result.State != stateClosed || inst.state() != stateClosed {
		t.Errorf("state = %q/%q, want closed (single instance never opens)", result.State, inst.state())
	}
	inst.mu.Lock()
	failures, until := inst.consecutiveFailures, inst.degradedUntil
	inst.mu.Unlock()
	if failures != 0 || !until.IsZero() {
		t.Errorf("breaker changed: failures=%d until=%s", failures, until)
	}
}

// TestInstanceTestNoModel covers §24.32 M15gv: without a model name no request
// is sent and nothing changes.
func TestInstanceTestNoModel(t *testing.T) {
	backend := newITestBackend(t, itestSSEHandler(protocolOpenAI))
	cfg := itestConfig(itestInstance(protocolOpenAI, "bare", backend.server.URL, 2*time.Second,
		nil, nil))
	server, httpServer := itestServer(t, cfg)
	inst := itestRuntime(t, server, protocolOpenAI, "bare")

	code, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"bare"}`, true)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if result.OK || result.Error != errNoTestModel || result.Status != 0 {
		t.Fatalf("result = %+v, want the no-model error", result)
	}
	if result.State != stateClosed || inst.state() != stateClosed {
		t.Errorf("state = %q, want closed", result.State)
	}
	if backend.count() != 0 {
		t.Errorf("upstream calls = %d, want 0", backend.count())
	}
}

// TestInstanceTestMonitorRow covers §24.32 M15gw: the test appears as a full
// POST/TEST row with the real upstream path, operator client and chunks.
// itestMonitorRow returns the ended TEST row from the recent-request ring.
func itestMonitorRow(t *testing.T, server *Server) *monitorRequestSnapshot {
	t.Helper()
	ring := server.monitor.stats.recentRequests()
	for i := range ring {
		if ring[i].Tag == monitorTagTest {
			return &ring[i]
		}
	}
	t.Fatalf("no TEST row in the recent-request ring: %+v", ring)
	return nil
}

// TestInstanceTestMonitorRow covers §24.32 M15gw: the row shows POST, the real
// upstream path (including the base_url prefix) and TEST as the client label.
// TestInstanceTestMonitorRow covers §24.32 M15gw: the row shows POST and the
// same inbound path as a real request of the protocol, regardless of the
// instance base_url; TEST is rendered from the tag.
func TestInstanceTestMonitorRow(t *testing.T) {
	t.Run("base_url path prefix", func(t *testing.T) {
		backend := newITestBackend(t, itestSSEHandler(protocolAnthropic))
		cfg := itestConfig(itestInstance(protocolAnthropic, "an", backend.server.URL+"/anthropic",
			2*time.Second, nil, map[string]string{"sonnet": "model-a"}))
		server, httpServer := itestServer(t, cfg)

		_, result, _ := itestCall(t, httpServer.URL, `{"protocol":"anthropic","name":"an"}`, true)
		if !result.OK {
			t.Fatalf("result = %+v, want success", result)
		}
		row := itestMonitorRow(t, server)
		if row.Method != http.MethodPost || row.Path != PathAnthropicMessages {
			t.Errorf("method/path = %s %s, want POST %s", row.Method, row.Path, PathAnthropicMessages)
		}
		if row.ClientIP != "" || row.UserAgent != "" {
			t.Errorf("client fields = %q/%q, want empty (TEST is rendered from the tag)", row.ClientIP, row.UserAgent)
		}
		if row.Tag != monitorTagTest {
			t.Errorf("tag = %q, want %q", row.Tag, monitorTagTest)
		}
		if !row.Ended || row.Status != http.StatusOK {
			t.Errorf("ended/status = %v/%d, want true/200", row.Ended, row.Status)
		}
		if row.ReqModel != "model-a" || row.InstName != "an" || row.UpstreamModel != "model-a" {
			t.Errorf("model/inst = %q %q %q", row.ReqModel, row.InstName, row.UpstreamModel)
		}
		if row.Chunks < 1 || len(row.ChunksList) < 1 {
			t.Errorf("chunks = %d (list %d), want at least one", row.Chunks, len(row.ChunksList))
		}
		if active := server.monitor.activeSnapshots(time.Now()); len(active) != 0 {
			t.Errorf("active snapshots = %d, want 0 after the test", len(active))
		}
		calls := backend.calls()
		if len(calls) != 1 || calls[0].path != "/anthropic/v1/messages" {
			t.Fatalf("upstream calls = %+v, want one POST /anthropic/v1/messages", calls)
		}

		raw := server.monitor.snapshotPayload(0)
		var snapshot struct {
			Requests []struct {
				Tag      string `json:"tag"`
				Method   string `json:"method"`
				Path     string `json:"path"`
				ClientIP string `json:"clientIP"`
			} `json:"requests"`
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		found := false
		for _, item := range snapshot.Requests {
			if item.Tag == monitorTagTest && item.Method == http.MethodPost &&
				item.Path == PathAnthropicMessages && item.ClientIP == "" {
				found = true
			}
		}
		if !found {
			t.Errorf("snapshot payload does not carry the TEST row with the inbound path: %s", raw)
		}
	})

	t.Run("no prefix", func(t *testing.T) {
		backend := newITestBackend(t, itestSSEHandler(protocolAnthropic))
		cfg := itestConfig(itestInstance(protocolAnthropic, "an", backend.server.URL, 2*time.Second,
			nil, map[string]string{"sonnet": "model-a"}))
		server, httpServer := itestServer(t, cfg)

		_, result, _ := itestCall(t, httpServer.URL, `{"protocol":"anthropic","name":"an"}`, true)
		if !result.OK {
			t.Fatalf("result = %+v, want success", result)
		}
		if row := itestMonitorRow(t, server); row.Path != PathAnthropicMessages {
			t.Errorf("path = %q, want the inbound path %q even without a base_url prefix", row.Path, PathAnthropicMessages)
		}
		calls := backend.calls()
		if len(calls) != 1 || calls[0].path != PathAnthropic {
			t.Fatalf("upstream calls = %+v, want the bare upstream path %s", calls, PathAnthropic)
		}
	})

	t.Run("openai", func(t *testing.T) {
		backend := newITestBackend(t, itestSSEHandler(protocolOpenAI))
		cfg := itestConfig(itestInstance(protocolOpenAI, "oa", backend.server.URL, 2*time.Second,
			[]string{"model-a"}, nil))
		server, httpServer := itestServer(t, cfg)

		_, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"oa"}`, true)
		if !result.OK {
			t.Fatalf("result = %+v, want success", result)
		}
		if row := itestMonitorRow(t, server); row.Path != PathOpenAI {
			t.Errorf("path = %q, want %q", row.Path, PathOpenAI)
		}
	})
}

// TestInstanceTestTenKContext covers §24.32 M15gz-1: the probe carries a
// deterministic >=40,000-character context, identical across runs.
func TestInstanceTestTenKContext(t *testing.T) {
	backend := newITestBackend(t, itestSSEHandler(protocolOpenAI))
	cfg := itestConfig(itestInstance(protocolOpenAI, "oa", backend.server.URL, 5*time.Second,
		[]string{"model-a"}, nil))
	_, httpServer := itestServer(t, cfg)

	for i := 0; i < 2; i++ {
		if _, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"oa"}`, true); !result.OK {
			t.Fatalf("test %d result = %+v, want success", i+1, result)
		}
	}
	calls := backend.calls()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(calls))
	}
	if calls[0].body != calls[1].body {
		t.Fatalf("two probe bodies differ (%d vs %d bytes)", len(calls[0].body), len(calls[1].body))
	}
	var body instanceTestBody
	if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
		t.Fatalf("decode probe body: %v", err)
	}
	if len(body.Messages) != 1 || body.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user message", body.Messages)
	}
	content := body.Messages[0].Content
	if len(content) < 40000 {
		t.Errorf("content len = %d, want >= 40000", len(content))
	}
	if !strings.HasSuffix(content, "reply with OK") {
		t.Errorf("content does not end with the instruction: %q", content[len(content)-32:])
	}
	if body.MaxTokens > 32 || !body.Stream {
		t.Errorf("body = %+v, want max_tokens<=32 and stream", body)
	}
}

// TestInstanceTestClientCellBlank covers §24.32 M15gz-2: the client column
// renders blank for missing IP/UA and TEST for tagged rows, never "–" or a
// placeholder client name.
func TestInstanceTestClientCellBlank(t *testing.T) {
	// Server side: a constructed row without client data keeps empty strings.
	raw, err := json.Marshal(monitorRequestSnapshot{Tag: monitorTagTest})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var decoded struct {
		ClientIP  string `json:"clientIP"`
		UserAgent string `json:"userAgent"`
		Tag       string `json:"tag"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if decoded.ClientIP != "" || decoded.UserAgent != "" {
		t.Errorf("client fields = %q/%q, want empty", decoded.ClientIP, decoded.UserAgent)
	}
	if decoded.Tag != monitorTagTest {
		t.Errorf("tag = %q, want %q", decoded.Tag, monitorTagTest)
	}
	for _, unwanted := range []string{"–", "operator"} {
		if strings.Contains(string(raw), unwanted) {
			t.Errorf("snapshot JSON contains %q: %s", unwanted, raw)
		}
	}

	// Frontend side: clientCellText turns a missing IP/UA into blanks and a
	// tagged row into a TEST badge; the renderer clears the cell first so a
	// refreshed row never accumulates span elements.
	data, err := os.ReadFile("../web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	html := string(data)
	start := strings.Index(html, "function clientCellText(req) {")
	if start < 0 {
		t.Fatal("clientCellText helper missing")
	}
	end := strings.Index(html[start:], "\n  }")
	if end < 0 {
		t.Fatal("clientCellText helper is not closed")
	}
	block := html[start : start+end]
	for _, want := range []string{
		"return { tag: 'TEST', ip: '', ua: '', uaTitle: '', title: 'TEST' };",
		"var ip = req.clientIP || '';",
		"var ua = rawUA ? userAgentLabel(rawUA) : '';",
		"[ip, rawUA].filter(Boolean).join(' ')",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("clientCellText missing %q", want)
		}
	}
	for _, unwanted := range []string{"–", "operator"} {
		if strings.Contains(block, unwanted) {
			t.Errorf("clientCellText still uses %q", unwanted)
		}
	}

	refreshStart := strings.Index(html, "function refreshMonitorRow(req) {")
	if refreshStart < 0 {
		t.Fatal("refreshMonitorRow missing")
	}
	refreshEnd := strings.Index(html[refreshStart:], "\n  }")
	if refreshEnd < 0 {
		t.Fatal("refreshMonitorRow is not closed")
	}
	refresh := html[refreshStart : refreshStart+refreshEnd]
	clearCell := strings.Index(refresh, "req.cells.clientIp.textContent = '';")
	tagClass := strings.Index(refresh, "clientTag.className = 'req-client-tag';")
	appendTag := strings.Index(refresh, "req.cells.clientIp.appendChild(clientTag);")
	if clearCell < 0 || tagClass < 0 || appendTag < 0 {
		t.Fatalf("client badge rendering missing (clear=%d class=%d append=%d)", clearCell, tagClass, appendTag)
	}
	if clearCell > appendTag {
		t.Error("client cell must be cleared before the tag span is appended")
	}
}

// TestInstanceTestCountsInStats covers §24.32 M15gx: connectivity tests now use
// the ordinary request accounting path, so they move the business counters and
// a failed test lands in recentErrors.
func TestInstanceTestCountsInStats(t *testing.T) {
	good := newITestBackend(t, itestSSEHandler(protocolOpenAI))
	bad := newITestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	})
	cfg := itestConfig(
		itestInstance(protocolOpenAI, "good", good.server.URL, 2*time.Second, []string{"model-a"}, nil),
		itestInstance(protocolOpenAI, "bad", bad.server.URL, 2*time.Second, []string{"model-a"}, nil),
	)
	_, httpServer := itestServer(t, cfg)

	before := itestGetStats(t, httpServer.URL)
	itestCall(t, httpServer.URL, `{"protocol":"openai","name":"good"}`, true)
	afterSuccess := itestGetStats(t, httpServer.URL)

	if afterSuccess.Requests.Total != before.Requests.Total+1 {
		t.Errorf("total after success = %d, want %d", afterSuccess.Requests.Total, before.Requests.Total+1)
	}
	if afterSuccess.Requests.Success != before.Requests.Success+1 {
		t.Errorf("success after success = %d, want %d", afterSuccess.Requests.Success, before.Requests.Success+1)
	}
	if afterSuccess.Requests.Failed != before.Requests.Failed {
		t.Errorf("failed after success = %d, want %d", afterSuccess.Requests.Failed, before.Requests.Failed)
	}
	if len(afterSuccess.RecentErrors) != len(before.RecentErrors) {
		t.Errorf("successful test added recentErrors: %d -> %d", len(before.RecentErrors), len(afterSuccess.RecentErrors))
	}
	goodRow := itestRow(afterSuccess.Instances, "good")
	if goodRow == nil || goodRow.Requests != 1 || goodRow.Success != 1 || goodRow.Failed != 0 {
		t.Errorf("good row = %+v, want requests/success/failed = 1/1/0", goodRow)
	}
	badRow := itestRow(afterSuccess.Instances, "bad")
	if badRow == nil || badRow.Requests != 0 || badRow.Success != 0 || badRow.Failed != 0 {
		t.Errorf("bad row = %+v, want untouched counters", badRow)
	}

	itestCall(t, httpServer.URL, `{"protocol":"openai","name":"bad"}`, true)
	afterFailure := itestGetStats(t, httpServer.URL)

	if afterFailure.Requests.Total != afterSuccess.Requests.Total+1 {
		t.Errorf("total after failure = %d, want %d", afterFailure.Requests.Total, afterSuccess.Requests.Total+1)
	}
	if afterFailure.Requests.Success != afterSuccess.Requests.Success {
		t.Errorf("success after failure = %d, want %d", afterFailure.Requests.Success, afterSuccess.Requests.Success)
	}
	if afterFailure.Requests.Failed != afterSuccess.Requests.Failed+1 {
		t.Errorf("failed after failure = %d, want %d", afterFailure.Requests.Failed, afterSuccess.Requests.Failed+1)
	}
	badRow = itestRow(afterFailure.Instances, "bad")
	if badRow == nil || badRow.Requests != 1 || badRow.Success != 0 || badRow.Failed != 1 {
		t.Errorf("bad row = %+v, want requests/success/failed = 1/0/1", badRow)
	}
	goodRow = itestRow(afterFailure.Instances, "good")
	if goodRow == nil || goodRow.Requests != 1 || goodRow.Success != 1 || goodRow.Failed != 0 {
		t.Errorf("good row = %+v, want it untouched by the failed test", goodRow)
	}
	if len(afterFailure.RecentErrors) != len(afterSuccess.RecentErrors)+1 {
		t.Fatalf("recentErrors = %d, want %d", len(afterFailure.RecentErrors), len(afterSuccess.RecentErrors)+1)
	}
	latest := afterFailure.RecentErrors[0]
	if latest.InstName != "bad" || latest.Error == "" {
		t.Errorf("latest recentError = %+v, want instName=bad with a reason", latest)
	}
}

// TestInstanceTestUsageAccounting covers §24.32 M182: the probe parses stream
// usage best-effort and feeds it through the ordinary stats path.
func TestInstanceTestUsageAccounting(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		backend := newITestBackend(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: message_start\n"+
				"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10000,\"output_tokens\":1}}}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\n"+
				"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		})
		cfg := itestConfig(itestInstance(protocolAnthropic, "an", backend.server.URL, 2*time.Second,
			nil, map[string]string{"sonnet": "model-a"}))
		_, httpServer := itestServer(t, cfg)

		before := itestGetStats(t, httpServer.URL)
		_, result, _ := itestCall(t, httpServer.URL, `{"protocol":"anthropic","name":"an"}`, true)
		if !result.OK {
			t.Fatalf("result = %+v, want success", result)
		}
		after := itestGetStats(t, httpServer.URL)
		if after.InTokens != before.InTokens+10000 {
			t.Errorf("global inTokens = %d, want %d", after.InTokens, before.InTokens+10000)
		}
		if after.OutTokens != before.OutTokens+5 {
			t.Errorf("global outTokens = %d, want %d", after.OutTokens, before.OutTokens+5)
		}
		beforeRow := itestRow(before.Instances, "an")
		afterRow := itestRow(after.Instances, "an")
		if beforeRow == nil || afterRow == nil {
			t.Fatal("instance row missing")
		}
		if afterRow.InTokens != beforeRow.InTokens+10000 || afterRow.OutTokens != beforeRow.OutTokens+5 {
			t.Errorf("instance tokens = %d/%d, want %d/%d",
				afterRow.InTokens, afterRow.OutTokens, beforeRow.InTokens+10000, beforeRow.OutTokens+5)
		}
	})

	t.Run("openai", func(t *testing.T) {
		backend := newITestBackend(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":5}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		})
		cfg := itestConfig(itestInstance(protocolOpenAI, "oa", backend.server.URL, 2*time.Second,
			[]string{"model-a"}, nil))
		_, httpServer := itestServer(t, cfg)

		before := itestGetStats(t, httpServer.URL)
		_, result, _ := itestCall(t, httpServer.URL, `{"protocol":"openai","name":"oa"}`, true)
		if !result.OK {
			t.Fatalf("result = %+v, want success", result)
		}
		after := itestGetStats(t, httpServer.URL)
		if after.InTokens != before.InTokens+10000 || after.OutTokens != before.OutTokens+5 {
			t.Errorf("global tokens = %d/%d, want %d/%d",
				after.InTokens, after.OutTokens, before.InTokens+10000, before.OutTokens+5)
		}
		row := itestRow(after.Instances, "oa")
		if row == nil || row.InTokens != 10000 || row.OutTokens != 5 {
			t.Errorf("instance row = %+v, want inTokens/outTokens = 10000/5", row)
		}
	})

	t.Run("no usage", func(t *testing.T) {
		backend := newITestBackend(t, itestSSEHandler(protocolAnthropic))
		cfg := itestConfig(itestInstance(protocolAnthropic, "an", backend.server.URL, 2*time.Second,
			nil, map[string]string{"sonnet": "model-a"}))
		_, httpServer := itestServer(t, cfg)

		before := itestGetStats(t, httpServer.URL)
		_, result, _ := itestCall(t, httpServer.URL, `{"protocol":"anthropic","name":"an"}`, true)
		if !result.OK {
			t.Fatalf("result = %+v, want success without usage", result)
		}
		after := itestGetStats(t, httpServer.URL)
		if after.InTokens != before.InTokens || after.OutTokens != before.OutTokens {
			t.Errorf("tokens changed without usage: %d/%d -> %d/%d",
				before.InTokens, before.OutTokens, after.InTokens, after.OutTokens)
		}
		if row := itestRow(after.Instances, "an"); row == nil || row.InTokens != 0 || row.OutTokens != 0 {
			t.Errorf("instance row = %+v, want zero tokens", row)
		}
	})
}

// TestConsumeTestStream covers the §24.32 M178 verdict branches directly.
func TestConsumeTestStream(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "openai done", body: "data: {\"id\":\"x\"}\n\ndata: [DONE]\n\n", want: true},
		{name: "anthropic message_stop", body: "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", want: true},
		{name: "clean eof after an event", body: "data: {\"id\":\"x\"}\n\n", want: true},
		{name: "eof without events", body: "", want: false},
		{name: "comments only", body: ": keep-alive\n\n", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := consumeTestStream(strings.NewReader(tc.body))
			if (err == nil) != tc.want {
				t.Fatalf("err = %v, want success=%v", err, tc.want)
			}
		})
	}
}

// TestInstanceTestUI covers §24.32 M15gy: both instance tables carry the test
// column between state and lock, and the button navigates to the monitor page.
func TestInstanceTestUI(t *testing.T) {
	data, err := os.ReadFile("../web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	html := string(data)
	for _, id := range []string{"stat-instances-openai-table", "stat-instances-anthropic-table"} {
		table := strings.Index(html, `id="`+id+`"`)
		if table < 0 {
			t.Fatalf("table %s missing", id)
		}
		rest := html[table:]
		groupEnd := strings.Index(rest, "</colgroup>")
		if groupEnd < 0 {
			t.Fatalf("table %s has no colgroup", id)
		}
		if cols := strings.Count(rest[:groupEnd], "<col "); cols != 13 {
			t.Errorf("table %s has %d columns, want 13", id, cols)
		}
		state := strings.Index(rest, "<th>状态</th>")
		test := strings.Index(rest, `title="测试连通性">测试</th>`)
		lock := strings.Index(rest, `title="锁定 / 解锁">锁定</th>`)
		if state < 0 || test < 0 || lock < 0 || !(state < test && test < lock) {
			t.Errorf("table %s: test column must sit between state and lock (state=%d test=%d lock=%d)",
				id, state, test, lock)
		}
	}
	for _, want := range []string{
		`testButton.title = '测试连通性'`,
		`testButton.textContent = '⚡'`,
		`/api/instances/test`,
		`location.href = '/admin/monitor'`,
		`button.disabled = true`,
		`keepalive: true`,
		`function clientCellText(req)`,
		`return { tag: 'TEST', ip: '', ua: '', uaTitle: '', title: 'TEST' };`,
		`clientTag.className = 'req-client-tag';`,
		`req.cells.clientIp.appendChild(clientTag);`,
		`req.cells.clientIp.textContent = '';`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// §24.32 M183/M186a: the method column has no tag; TEST is a badge in the
	// client column, and missing client values render blank instead of "–".
	for _, unwanted := range []string{
		"req-test-tag",
		"appendChild(testTag)",
		"clientIP || '–'",
		"rawUA || '–'",
		"operator",
	} {
		if strings.Contains(html, unwanted) {
			t.Errorf("index.html still contains %q", unwanted)
		}
	}
	// §24.32 M185: the result is presented by the monitor TEST row; the stats
	// page only fires the request and navigates, so no in-place render helper
	// may linger as dead code.
	if strings.Contains(html, "applyInstanceTestResult") {
		t.Error("index.html still contains the dead applyInstanceTestResult helper")
	}
}
