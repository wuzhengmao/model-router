package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
	"model-router/internal/proxy"
)

const (
	serviceToken = "service-token"
	openAIKey    = "openai-upstream-key"
	anthropicKey = "anthropic-upstream-key"
)

type capturedRequest struct {
	method   string
	path     string
	rawQuery string
	header   http.Header
	body     string
}

// newBackend starts a mock upstream that records the last request it received
// and replies with the given status, headers and body. It also reports how many
// requests reached the backend.
func newBackend(t *testing.T, status int, headers map[string]string, responseBody string) (*httptest.Server, func() capturedRequest, func() int32) {
	t.Helper()

	var (
		mu    sync.Mutex
		got   *capturedRequest
		calls atomic.Int32
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read backend request body: %v", err)
			return
		}
		mu.Lock()
		got = &capturedRequest{
			method:   r.Method,
			path:     r.URL.Path,
			rawQuery: r.URL.RawQuery,
			header:   r.Header.Clone(),
			body:     string(body),
		}
		mu.Unlock()

		for key, value := range headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(status)
		if _, err := io.WriteString(w, responseBody); err != nil {
			t.Errorf("write backend response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	return server, func() capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		if got == nil {
			t.Fatal("backend never received a request")
		}
		return *got
	}, calls.Load
}

func backendConfig(proto, backendURL, apiKey, model string, matches ...string) config.Backend {
	return config.Backend{
		Protocol: proto,
		BaseURL:  backendURL,
		APIKey:   apiKey,
		Model:    model,
		Matches:  matches,
		Timeout:  config.Duration(5 * time.Second),
	}
}

func withRoles(b config.Backend, roles ...string) config.Backend {
	b.Roles = roles
	return b
}

func newProxy(t *testing.T, authToken string, backends ...config.Backend) *httptest.Server {
	t.Helper()

	handler, err := proxy.New(&config.Config{
		Listen:    "unused:0",
		AuthToken: authToken,
		Backends:  backends,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func withAuth(proto string, extra map[string]string) map[string]string {
	headers := make(map[string]string, len(extra)+1)
	if proto == config.ProtocolAnthropic {
		headers["x-api-key"] = serviceToken
	} else {
		headers["Authorization"] = "Bearer " + serviceToken
	}
	for key, value := range extra {
		headers[key] = value
	}
	return headers
}

func send(t *testing.T, method, url, body string, headers map[string]string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call proxy: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(body)
}

func assertJSONContentType(t *testing.T, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("error Content-Type = %q, want application/json", got)
	}
}

type openAIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeOpenAIError(t *testing.T, resp *http.Response) openAIError {
	t.Helper()
	assertJSONContentType(t, resp)

	body := readBody(t, resp)
	var apiErr openAIError
	if err := json.Unmarshal([]byte(body), &apiErr); err != nil {
		t.Fatalf("error body is not JSON: %v (body %q)", err, body)
	}
	if apiErr.Error.Message == "" || apiErr.Error.Type == "" || apiErr.Error.Code == "" {
		t.Errorf("incomplete OpenAI error body: %q", body)
	}
	return apiErr
}

type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeAnthropicError(t *testing.T, resp *http.Response) anthropicError {
	t.Helper()
	assertJSONContentType(t, resp)

	body := readBody(t, resp)
	var apiErr anthropicError
	if err := json.Unmarshal([]byte(body), &apiErr); err != nil {
		t.Fatalf("error body is not JSON: %v (body %q)", err, body)
	}
	if apiErr.Type != "error" {
		t.Errorf("top-level type = %q, want %q (body %q)", apiErr.Type, "error", body)
	}
	if apiErr.Error.Type == "" || apiErr.Error.Message == "" {
		t.Errorf("incomplete Anthropic error body: %q", body)
	}
	return apiErr
}

func assertModelRewritten(t *testing.T, originalBody, forwardedBody, wantModel string) {
	t.Helper()

	var original, forwarded map[string]any
	if err := json.Unmarshal([]byte(originalBody), &original); err != nil {
		t.Fatalf("unmarshal original body: %v", err)
	}
	if err := json.Unmarshal([]byte(forwardedBody), &forwarded); err != nil {
		t.Fatalf("forwarded body is not JSON: %v (body %q)", err, forwardedBody)
	}
	if got, _ := forwarded["model"].(string); got != wantModel {
		t.Errorf("forwarded model = %q, want %q", forwarded["model"], wantModel)
	}
	original["model"] = wantModel
	if !reflect.DeepEqual(original, forwarded) {
		t.Errorf("forwarded body semantics changed:\n original(with new model) = %#v\n forwarded = %#v", original, forwarded)
	}
}

func TestOpenAIPassThroughVerbatim(t *testing.T) {
	const (
		requestBody  = `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}],"stream":false}`
		responseBody = `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`
	)

	backend, captured, _ := newBackend(t, http.StatusCreated, map[string]string{
		"Content-Type":    "application/json",
		"X-Upstream-Name": "openai-mock",
	}, responseBody)
	proxyServer := newProxy(t, serviceToken, backendConfig(config.ProtocolOpenAI, backend.URL, openAIKey, "gpt-test"))

	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions?trace=abc", requestBody, withAuth(config.ProtocolOpenAI, map[string]string{
		"Content-Type":    "application/json",
		"X-Custom-Header": "keep-me",
	}))

	got := captured()
	if got.method != http.MethodPost {
		t.Errorf("backend method = %q, want %q", got.method, http.MethodPost)
	}
	if got.path != "/v1/chat/completions" {
		t.Errorf("backend path = %q, want %q", got.path, "/v1/chat/completions")
	}
	if got.rawQuery != "trace=abc" {
		t.Errorf("backend query = %q, want %q", got.rawQuery, "trace=abc")
	}
	if want := "Bearer " + openAIKey; got.header.Get("Authorization") != want {
		t.Errorf("backend Authorization = %q, want configured key %q", got.header.Get("Authorization"), want)
	}
	if got.header.Get("Authorization") == "Bearer "+serviceToken {
		t.Errorf("backend received the client service token; it must never be forwarded upstream")
	}
	if want := "application/json"; got.header.Get("Content-Type") != want {
		t.Errorf("backend Content-Type = %q, want %q", got.header.Get("Content-Type"), want)
	}
	if want := "keep-me"; got.header.Get("X-Custom-Header") != want {
		t.Errorf("backend X-Custom-Header = %q, want %q", got.header.Get("X-Custom-Header"), want)
	}
	if got.body != requestBody {
		t.Errorf("backend body = %q, want verbatim %q", got.body, requestBody)
	}

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("proxy status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if want := "application/json"; resp.Header.Get("Content-Type") != want {
		t.Errorf("proxy Content-Type = %q, want %q", resp.Header.Get("Content-Type"), want)
	}
	if want := "openai-mock"; resp.Header.Get("X-Upstream-Name") != want {
		t.Errorf("proxy X-Upstream-Name = %q, want %q", resp.Header.Get("X-Upstream-Name"), want)
	}
	if body := readBody(t, resp); body != responseBody {
		t.Errorf("proxy body = %q, want verbatim %q", body, responseBody)
	}
}

func TestAnthropicPassThroughVerbatim(t *testing.T) {
	const (
		requestBody  = `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
		responseBody = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}]}`
	)

	backend, captured, _ := newBackend(t, http.StatusOK, map[string]string{
		"Content-Type":      "application/json",
		"anthropic-version": "2023-06-01",
		"X-Upstream-Name":   "anthropic-mock",
	}, responseBody)
	proxyServer := newProxy(t, serviceToken, backendConfig(config.ProtocolAnthropic, backend.URL, anthropicKey, "claude-test"))

	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages?beta=1", requestBody, withAuth(config.ProtocolAnthropic, map[string]string{
		"Content-Type":      "application/json",
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "prompt-caching-2024-07-31",
		"X-Custom-Header":   "keep-me",
		// Present but invalid for this entrypoint: Anthropic ignores it and it
		// must never reach the upstream.
		"Authorization": "Bearer client-token",
	}))

	got := captured()
	if got.method != http.MethodPost {
		t.Errorf("backend method = %q, want %q", got.method, http.MethodPost)
	}
	if got.path != "/v1/messages" {
		t.Errorf("backend path = %q, want %q", got.path, "/v1/messages")
	}
	if got.rawQuery != "beta=1" {
		t.Errorf("backend query = %q, want %q", got.rawQuery, "beta=1")
	}
	if want := anthropicKey; got.header.Get("x-api-key") != want {
		t.Errorf("backend x-api-key = %q, want configured key %q", got.header.Get("x-api-key"), want)
	}
	if got.header.Get("x-api-key") == serviceToken {
		t.Errorf("backend received the client service token; it must never be forwarded upstream")
	}
	if got := got.header.Get("Authorization"); got != "" {
		t.Errorf("backend Authorization = %q, want empty (client credential must not be forwarded)", got)
	}
	if want := "2023-06-01"; got.header.Get("anthropic-version") != want {
		t.Errorf("backend anthropic-version = %q, want %q", got.header.Get("anthropic-version"), want)
	}
	if want := "prompt-caching-2024-07-31"; got.header.Get("anthropic-beta") != want {
		t.Errorf("backend anthropic-beta = %q, want %q", got.header.Get("anthropic-beta"), want)
	}
	if want := "application/json"; got.header.Get("Content-Type") != want {
		t.Errorf("backend Content-Type = %q, want %q", got.header.Get("Content-Type"), want)
	}
	if want := "keep-me"; got.header.Get("X-Custom-Header") != want {
		t.Errorf("backend X-Custom-Header = %q, want %q", got.header.Get("X-Custom-Header"), want)
	}
	if got.body != requestBody {
		t.Errorf("backend body = %q, want verbatim %q", got.body, requestBody)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("proxy status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if want := "anthropic-mock"; resp.Header.Get("X-Upstream-Name") != want {
		t.Errorf("proxy X-Upstream-Name = %q, want %q", resp.Header.Get("X-Upstream-Name"), want)
	}
	if body := readBody(t, resp); body != responseBody {
		t.Errorf("proxy body = %q, want verbatim %q", body, responseBody)
	}
}

func TestMissingAuthorizationReturns401(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		protocol  string
		anthropic bool
	}{
		{"openai", "/v1/chat/completions", config.ProtocolOpenAI, false},
		{"anthropic", "/v1/messages", config.ProtocolAnthropic, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
			proxyServer := newProxy(t, serviceToken, backendConfig(tc.protocol, backend.URL, "upstream-key", "test-model"))

			resp := send(t, http.MethodPost, proxyServer.URL+tc.path, `{"model":"test-model"}`, nil)

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
			}
			if tc.anthropic {
				apiErr := decodeAnthropicError(t, resp)
				if apiErr.Error.Type != "authentication_error" {
					t.Errorf("error.type = %q, want %q", apiErr.Error.Type, "authentication_error")
				}
				if want := "missing x-api-key header"; apiErr.Error.Message != want {
					t.Errorf("error.message = %q, want %q", apiErr.Error.Message, want)
				}
			} else {
				apiErr := decodeOpenAIError(t, resp)
				if apiErr.Error.Code != "missing_api_key" {
					t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "missing_api_key")
				}
			}
			if got := calls(); got != 0 {
				t.Errorf("backend calls = %d, want 0 (request must not be forwarded)", got)
			}
		})
	}
}

func TestWrongTokenReturns401(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		protocol  string
		anthropic bool
	}{
		{"openai", "/v1/chat/completions", config.ProtocolOpenAI, false},
		{"anthropic", "/v1/messages", config.ProtocolAnthropic, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
			proxyServer := newProxy(t, serviceToken, backendConfig(tc.protocol, backend.URL, "upstream-key", "test-model"))

			headers := map[string]string{"Authorization": "Bearer wrong-token"}
			if tc.anthropic {
				headers = map[string]string{"x-api-key": "wrong-token"}
			}
			resp := send(t, http.MethodPost, proxyServer.URL+tc.path, `{"model":"test-model"}`, headers)

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
			}
			if tc.anthropic {
				apiErr := decodeAnthropicError(t, resp)
				if apiErr.Error.Type != "authentication_error" {
					t.Errorf("error.type = %q, want %q", apiErr.Error.Type, "authentication_error")
				}
				if want := "invalid service access token"; apiErr.Error.Message != want {
					t.Errorf("error.message = %q, want %q", apiErr.Error.Message, want)
				}
			} else {
				apiErr := decodeOpenAIError(t, resp)
				if apiErr.Error.Code != "invalid_api_key" {
					t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "invalid_api_key")
				}
			}
			if got := calls(); got != 0 {
				t.Errorf("backend calls = %d, want 0 (request must not be forwarded)", got)
			}
		})
	}
}

func TestAnthropicRejectsAuthorizationBearer(t *testing.T) {
	backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
	proxyServer := newProxy(t, serviceToken, backendConfig(config.ProtocolAnthropic, backend.URL, anthropicKey, "test-model"))

	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", `{"model":"test-model"}`, map[string]string{
		"Authorization": "Bearer " + serviceToken,
	})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	apiErr := decodeAnthropicError(t, resp)
	if apiErr.Error.Type != "authentication_error" {
		t.Errorf("error.type = %q, want %q", apiErr.Error.Type, "authentication_error")
	}
	if want := "missing x-api-key header"; apiErr.Error.Message != want {
		t.Errorf("error.message = %q, want %q", apiErr.Error.Message, want)
	}
	if got := calls(); got != 0 {
		t.Errorf("backend calls = %d, want 0 (request must not be forwarded)", got)
	}
}

func TestOpenAIRejectsXAPIKeyOnly(t *testing.T) {
	backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
	proxyServer := newProxy(t, serviceToken, backendConfig(config.ProtocolOpenAI, backend.URL, openAIKey, "test-model"))

	resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", `{"model":"test-model"}`, map[string]string{
		"x-api-key": serviceToken,
	})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	apiErr := decodeOpenAIError(t, resp)
	if apiErr.Error.Code != "missing_api_key" {
		t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "missing_api_key")
	}
	if got := calls(); got != 0 {
		t.Errorf("backend calls = %d, want 0 (request must not be forwarded)", got)
	}
}

func TestUnknownPathNotForwarded(t *testing.T) {
	backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
	proxyServer := newProxy(t, serviceToken, backendConfig(config.ProtocolOpenAI, backend.URL, openAIKey, "gpt-test"))

	resp := send(t, http.MethodGet, proxyServer.URL+"/v1/models", "", map[string]string{
		"Authorization": "Bearer " + serviceToken,
	})

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	apiErr := decodeOpenAIError(t, resp)
	if apiErr.Error.Code != "not_found" {
		t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "not_found")
	}
	if got := calls(); got != 0 {
		t.Errorf("backend calls = %d, want 0 (request must not be forwarded)", got)
	}
}

func TestRouteByModel(t *testing.T) {
	first, _, firstCalls := newBackend(t, http.StatusOK, nil, `{"backend":"first"}`)
	second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"backend":"second"}`)
	proxyServer := newProxy(t, serviceToken,
		backendConfig(config.ProtocolOpenAI, first.URL, "first-key", "gpt-a"),
		backendConfig(config.ProtocolOpenAI, second.URL, "second-key", "gpt-b"),
	)

	cases := []struct {
		name       string
		model      string
		wantFirst  bool
		wantSecond bool
	}{
		{"matches first", "gpt-a", true, false},
		{"matches second", "gpt-b", false, true},
		{"unknown model falls back to first", "unknown-model", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			beforeFirst, beforeSecond := firstCalls(), secondCalls()
			body := `{"model":"` + tc.model + `","messages":[]}`
			resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
			}
			gotFirst := firstCalls() - beforeFirst
			gotSecond := secondCalls() - beforeSecond
			if tc.wantFirst && gotFirst != 1 {
				t.Errorf("first backend calls = %d, want 1", gotFirst)
			}
			if !tc.wantFirst && gotFirst != 0 {
				t.Errorf("first backend calls = %d, want 0", gotFirst)
			}
			if tc.wantSecond && gotSecond != 1 {
				t.Errorf("second backend calls = %d, want 1", gotSecond)
			}
			if !tc.wantSecond && gotSecond != 0 {
				t.Errorf("second backend calls = %d, want 0", gotSecond)
			}
		})
	}
}

func TestRouteFallsBackToFirst(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing model", `{"messages":[]}`},
		{"non-string model", `{"model":123,"messages":[]}`},
		{"null model", `{"model":null,"messages":[]}`},
		{"invalid json", `not-json`},
		{"top-level array", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, captured, firstCalls := newBackend(t, http.StatusOK, nil, `{"backend":"first"}`)
			second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"backend":"second"}`)
			proxyServer := newProxy(t, serviceToken,
				backendConfig(config.ProtocolOpenAI, first.URL, "first-key", "gpt-a"),
				backendConfig(config.ProtocolOpenAI, second.URL, "second-key", "gpt-b"),
			)

			resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", tc.body, withAuth(config.ProtocolOpenAI, map[string]string{
				"Content-Type": "application/json",
			}))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
			}
			if got := firstCalls(); got != 1 {
				t.Errorf("first backend calls = %d, want 1", got)
			}
			if got := secondCalls(); got != 0 {
				t.Errorf("second backend calls = %d, want 0", got)
			}
			if got := captured().body; got != tc.body {
				t.Errorf("first backend body = %q, want verbatim %q", got, tc.body)
			}
		})
	}
}

func TestRouteByMatches(t *testing.T) {
	newPair := func(t *testing.T) (first, second *httptest.Server, firstCaptured func() capturedRequest, firstCalls func() int32, secondCaptured func() capturedRequest, secondCalls func() int32) {
		t.Helper()
		first, firstCaptured, firstCalls = newBackend(t, http.StatusOK, nil, `{"backend":"first"}`)
		second, secondCaptured, secondCalls = newBackend(t, http.StatusOK, nil, `{"backend":"second"}`)
		return first, second, firstCaptured, firstCalls, secondCaptured, secondCalls
	}

	t.Run("alias match rewrites top-level model and preserves other fields", func(t *testing.T) {
		first, second, _, firstCalls, secondCaptured, secondCalls := newPair(t)
		proxyServer := newProxy(t, serviceToken,
			backendConfig(config.ProtocolOpenAI, first.URL, "first-key", "real-a", "alias-a"),
			backendConfig(config.ProtocolOpenAI, second.URL, "second-key", "real-b", " alias-b ", "", "alias-b2"),
		)

		body := `{"model":"alias-b","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := firstCalls(); got != 0 {
			t.Errorf("first backend calls = %d, want 0", got)
		}
		if got := secondCalls(); got != 1 {
			t.Errorf("second backend calls = %d, want 1", got)
		}

		forwarded := secondCaptured().body
		var original, got map[string]any
		if err := json.Unmarshal([]byte(body), &original); err != nil {
			t.Fatalf("unmarshal original body: %v", err)
		}
		if err := json.Unmarshal([]byte(forwarded), &got); err != nil {
			t.Fatalf("forwarded body is not JSON: %v (body %q)", err, forwarded)
		}
		if model, _ := got["model"].(string); model != "real-b" {
			t.Errorf("forwarded model = %q, want %q", got["model"], "real-b")
		}
		original["model"] = "real-b"
		if !reflect.DeepEqual(original, got) {
			t.Errorf("forwarded body semantics changed:\n original(with new model) = %#v\n forwarded = %#v", original, got)
		}
		if want := "hi"; got["messages"] == nil || !strings.Contains(forwarded, `"content":"hi"`) {
			t.Errorf("forwarded body lost messages: %q (want %q)", forwarded, want)
		}
		if !strings.Contains(forwarded, `"temperature":0.5`) {
			t.Errorf("forwarded body lost temperature: %q", forwarded)
		}
	})

	t.Run("second alias rewrites model", func(t *testing.T) {
		first, second, _, firstCalls, secondCaptured, secondCalls := newPair(t)
		proxyServer := newProxy(t, serviceToken,
			backendConfig(config.ProtocolOpenAI, first.URL, "first-key", "real-a", "alias-a"),
			backendConfig(config.ProtocolOpenAI, second.URL, "second-key", "real-b", "alias-b", "alias-b2"),
		)

		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", `{"model":"alias-b2"}`, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := firstCalls(); got != 0 {
			t.Errorf("first backend calls = %d, want 0", got)
		}
		if got := secondCalls(); got != 1 {
			t.Errorf("second backend calls = %d, want 1", got)
		}
		forwarded := secondCaptured().body
		if !strings.Contains(forwarded, `"model":"real-b"`) {
			t.Errorf("forwarded body = %q, want model rewritten to %q", forwarded, "real-b")
		}
	})

	t.Run("exact model match does not rewrite body", func(t *testing.T) {
		first, second, firstCaptured, firstCalls, _, secondCalls := newPair(t)
		proxyServer := newProxy(t, serviceToken,
			backendConfig(config.ProtocolOpenAI, first.URL, "first-key", "real-a", "alias-a"),
			backendConfig(config.ProtocolOpenAI, second.URL, "second-key", "real-b", "alias-b"),
		)

		body := `{"model":"real-a",  "messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := firstCalls(); got != 1 {
			t.Errorf("first backend calls = %d, want 1", got)
		}
		if got := secondCalls(); got != 0 {
			t.Errorf("second backend calls = %d, want 0", got)
		}
		if got := firstCaptured().body; got != body {
			t.Errorf("backend body = %q, want verbatim %q", got, body)
		}
	})

	t.Run("unmatched model falls back to first without rewrite", func(t *testing.T) {
		first, second, firstCaptured, firstCalls, _, secondCalls := newPair(t)
		proxyServer := newProxy(t, serviceToken,
			backendConfig(config.ProtocolOpenAI, first.URL, "first-key", "real-a", "alias-a"),
			backendConfig(config.ProtocolOpenAI, second.URL, "second-key", "real-b", "alias-b"),
		)

		body := `{"model":"no-such-model","messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := firstCalls(); got != 1 {
			t.Errorf("first backend calls = %d, want 1", got)
		}
		if got := secondCalls(); got != 0 {
			t.Errorf("second backend calls = %d, want 0", got)
		}
		if got := firstCaptured().body; got != body {
			t.Errorf("backend body = %q, want verbatim %q", got, body)
		}
	})

	t.Run("exact model wins over alias on the same backend", func(t *testing.T) {
		backend, captured, _ := newBackend(t, http.StatusOK, nil, `{"backend":"only"}`)
		proxyServer := newProxy(t, serviceToken,
			backendConfig(config.ProtocolOpenAI, backend.URL, "only-key", "same-model", "same-model"),
		)

		body := `{"model":"same-model",  "messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := captured().body; got != body {
			t.Errorf("backend body = %q, want verbatim (R4a must not rewrite) %q", got, body)
		}
	})
}

func TestRouteByRoles(t *testing.T) {
	newRoleProxy := func(t *testing.T) (*httptest.Server, func() capturedRequest, func() int32, func() capturedRequest, func() int32) {
		t.Helper()
		flash, flashCaptured, flashCalls := newBackend(t, http.StatusOK, nil, `{"backend":"flash"}`)
		pro, proCaptured, proCalls := newBackend(t, http.StatusOK, nil, `{"backend":"pro"}`)
		proxyServer := newProxy(t, serviceToken,
			withRoles(backendConfig(config.ProtocolAnthropic, flash.URL, "flash-key", "deepseek-v4-flash"), "sonnet", "haiku"),
			withRoles(backendConfig(config.ProtocolAnthropic, pro.URL, "pro-key", "deepseek-v4-pro"), "opus", "fable"),
		)
		return proxyServer, flashCaptured, flashCalls, proCaptured, proCalls
	}

	t.Run("role name and claude prefix rewrite model", func(t *testing.T) {
		cases := []struct {
			model     string
			want      string
			wantFlash bool
		}{
			{"sonnet", "deepseek-v4-flash", true},
			{"haiku", "deepseek-v4-flash", true},
			{"claude-sonnet-4-5-20250929", "deepseek-v4-flash", true},
			{"opus", "deepseek-v4-pro", false},
			{"fable", "deepseek-v4-pro", false},
			{"claude-opus-4-1-20250805", "deepseek-v4-pro", false},
		}
		for _, tc := range cases {
			t.Run(tc.model, func(t *testing.T) {
				proxyServer, flashCaptured, flashCalls, proCaptured, proCalls := newRoleProxy(t)
				body := `{"model":"` + tc.model + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
				resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
				}
				var forwarded string
				if tc.wantFlash {
					if got := flashCalls(); got != 1 {
						t.Errorf("flash backend calls = %d, want 1", got)
					}
					if got := proCalls(); got != 0 {
						t.Errorf("pro backend calls = %d, want 0", got)
					}
					forwarded = flashCaptured().body
				} else {
					if got := flashCalls(); got != 0 {
						t.Errorf("flash backend calls = %d, want 0", got)
					}
					if got := proCalls(); got != 1 {
						t.Errorf("pro backend calls = %d, want 1", got)
					}
					forwarded = proCaptured().body
				}
				assertModelRewritten(t, body, forwarded, tc.want)
			})
		}
	})

	t.Run("explicit model match wins over role on same backend", func(t *testing.T) {
		backend, captured, _ := newBackend(t, http.StatusOK, nil, `{"backend":"only"}`)
		proxyServer := newProxy(t, serviceToken,
			withRoles(backendConfig(config.ProtocolAnthropic, backend.URL, "only-key", "sonnet"), "sonnet"),
		)

		body := `{"model":"sonnet",  "messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := captured().body; got != body {
			t.Errorf("backend body = %q, want verbatim (R4a must not rewrite) %q", got, body)
		}
	})

	t.Run("first backend wins for the same role", func(t *testing.T) {
		first, firstCaptured, firstCalls := newBackend(t, http.StatusOK, nil, `{"backend":"first"}`)
		second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"backend":"second"}`)
		proxyServer := newProxy(t, serviceToken,
			withRoles(backendConfig(config.ProtocolOpenAI, first.URL, "first-key", "first-real"), "sonnet"),
			withRoles(backendConfig(config.ProtocolOpenAI, second.URL, "second-key", "second-real"), "sonnet"),
		)

		body := `{"model":"sonnet","messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := firstCalls(); got != 1 {
			t.Errorf("first backend calls = %d, want 1", got)
		}
		if got := secondCalls(); got != 0 {
			t.Errorf("second backend calls = %d, want 0", got)
		}
		assertModelRewritten(t, body, firstCaptured().body, "first-real")
	})

	t.Run("unhandled model values fall back without rewrite", func(t *testing.T) {
		for _, model := range []string{"best", "sonnet[1m]", "opus[1m]", "opusplan", "Sonnet"} {
			t.Run(model, func(t *testing.T) {
				proxyServer, flashCaptured, flashCalls, _, proCalls := newRoleProxy(t)
				body := `{"model":"` + model + `","messages":[]}`
				resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
				}
				if got := flashCalls(); got != 1 {
					t.Errorf("first backend calls = %d, want 1 (R5)", got)
				}
				if got := proCalls(); got != 0 {
					t.Errorf("second backend calls = %d, want 0", got)
				}
				if got := flashCaptured().body; got != body {
					t.Errorf("backend body = %q, want verbatim %q", got, body)
				}
			})
		}
	})

	t.Run("roles and matches coexist", func(t *testing.T) {
		backend, captured, _ := newBackend(t, http.StatusOK, nil, `{"backend":"only"}`)
		proxyServer := newProxy(t, serviceToken,
			withRoles(backendConfig(config.ProtocolAnthropic, backend.URL, "only-key", "real-model", "explicit-alias"), "sonnet"),
		)

		for _, model := range []string{"explicit-alias", "sonnet", "claude-sonnet-4-5"} {
			body := `{"model":"` + model + `","messages":[]}`
			resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
			}
			assertModelRewritten(t, body, captured().body, "real-model")
		}
	})
}

func TestRouteWildcardFallback(t *testing.T) {
	t.Run("unknown model uses first wildcard backend", func(t *testing.T) {
		primary, _, primaryCalls := newBackend(t, http.StatusOK, nil, `{"backend":"primary"}`)
		fallback, fallbackCaptured, fallbackCalls := newBackend(t, http.StatusOK, nil, `{"backend":"fallback"}`)
		proxyServer := newProxy(t, serviceToken,
			backendConfig(config.ProtocolAnthropic, primary.URL, "primary-key", "primary-model"),
			withRoles(backendConfig(config.ProtocolAnthropic, fallback.URL, "fallback-key", "fallback-model"), "*"),
		)

		body := `{"model":"some-unknown-model","max_tokens":8,"messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := primaryCalls(); got != 0 {
			t.Errorf("primary backend calls = %d, want 0", got)
		}
		if got := fallbackCalls(); got != 1 {
			t.Errorf("fallback backend calls = %d, want 1", got)
		}
		assertModelRewritten(t, body, fallbackCaptured().body, "fallback-model")
	})

	t.Run("concrete role wins over wildcard", func(t *testing.T) {
		wildcard, _, wildcardCalls := newBackend(t, http.StatusOK, nil, `{"backend":"wildcard"}`)
		roleBackend, roleCaptured, roleCalls := newBackend(t, http.StatusOK, nil, `{"backend":"role"}`)
		proxyServer := newProxy(t, serviceToken,
			withRoles(backendConfig(config.ProtocolAnthropic, wildcard.URL, "wildcard-key", "wildcard-model"), "*"),
			withRoles(backendConfig(config.ProtocolAnthropic, roleBackend.URL, "role-key", "sonnet-model"), "sonnet"),
		)

		body := `{"model":"sonnet","messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := wildcardCalls(); got != 0 {
			t.Errorf("wildcard backend calls = %d, want 0 (R4c must win over R4d)", got)
		}
		if got := roleCalls(); got != 1 {
			t.Errorf("role backend calls = %d, want 1", got)
		}
		assertModelRewritten(t, body, roleCaptured().body, "sonnet-model")
	})

	t.Run("explicit alias wins over wildcard", func(t *testing.T) {
		wildcard, _, wildcardCalls := newBackend(t, http.StatusOK, nil, `{"backend":"wildcard"}`)
		aliasBackend, aliasCaptured, aliasCalls := newBackend(t, http.StatusOK, nil, `{"backend":"alias"}`)
		proxyServer := newProxy(t, serviceToken,
			withRoles(backendConfig(config.ProtocolAnthropic, wildcard.URL, "wildcard-key", "wildcard-model"), "*"),
			backendConfig(config.ProtocolAnthropic, aliasBackend.URL, "alias-key", "alias-model", "explicit-alias"),
		)

		body := `{"model":"explicit-alias","messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := wildcardCalls(); got != 0 {
			t.Errorf("wildcard backend calls = %d, want 0", got)
		}
		if got := aliasCalls(); got != 1 {
			t.Errorf("alias backend calls = %d, want 1", got)
		}
		assertModelRewritten(t, body, aliasCaptured().body, "alias-model")
	})

	t.Run("first wildcard backend wins", func(t *testing.T) {
		first, firstCaptured, firstCalls := newBackend(t, http.StatusOK, nil, `{"backend":"first"}`)
		second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"backend":"second"}`)
		proxyServer := newProxy(t, serviceToken,
			withRoles(backendConfig(config.ProtocolAnthropic, first.URL, "first-key", "first-model"), "*"),
			withRoles(backendConfig(config.ProtocolAnthropic, second.URL, "second-key", "second-model"), "*"),
		)

		body := `{"model":"unknown-model","messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := firstCalls(); got != 1 {
			t.Errorf("first backend calls = %d, want 1", got)
		}
		if got := secondCalls(); got != 0 {
			t.Errorf("second backend calls = %d, want 0", got)
		}
		assertModelRewritten(t, body, firstCaptured().body, "first-model")
	})

	t.Run("missing or non-string model does not use wildcard", func(t *testing.T) {
		bodies := []string{
			`{}`,
			`{"model":123}`,
			`{"model":null}`,
			`{"model":true}`,
			`{"model":{"name":"sonnet"}}`,
			`not-json`,
			`[1,2,3]`,
		}
		for _, body := range bodies {
			t.Run(body, func(t *testing.T) {
				first, firstCaptured, firstCalls := newBackend(t, http.StatusOK, nil, `{"backend":"first"}`)
				wildcard, _, wildcardCalls := newBackend(t, http.StatusOK, nil, `{"backend":"wildcard"}`)
				proxyServer := newProxy(t, serviceToken,
					backendConfig(config.ProtocolAnthropic, first.URL, "first-key", "first-model"),
					withRoles(backendConfig(config.ProtocolAnthropic, wildcard.URL, "wildcard-key", "wildcard-model"), "*"),
				)

				resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
				}
				if got := firstCalls(); got != 1 {
					t.Errorf("first backend calls = %d, want 1 (R5)", got)
				}
				if got := wildcardCalls(); got != 0 {
					t.Errorf("wildcard backend calls = %d, want 0", got)
				}
				if got := firstCaptured().body; got != body {
					t.Errorf("backend body = %q, want verbatim %q", got, body)
				}
			})
		}
	})
}

func TestProtocolListsAreIndependent(t *testing.T) {
	openaiBackend, _, openaiCalls := newBackend(t, http.StatusOK, nil, `{"backend":"openai"}`)
	anthropicBackend, anthropicCaptured, anthropicCalls := newBackend(t, http.StatusOK, nil, `{"backend":"anthropic"}`)
	proxyServer := newProxy(t, serviceToken,
		backendConfig(config.ProtocolOpenAI, openaiBackend.URL, openAIKey, "shared-model"),
		backendConfig(config.ProtocolAnthropic, anthropicBackend.URL, anthropicKey, "claude-x"),
	)

	t.Run("anthropic entry ignores openai backends", func(t *testing.T) {
		beforeOpenAI, beforeAnthropic := openaiCalls(), anthropicCalls()
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", `{"model":"shared-model"}`, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := anthropicCalls() - beforeAnthropic; got != 1 {
			t.Errorf("anthropic backend calls = %d, want 1", got)
		}
		if got := openaiCalls() - beforeOpenAI; got != 0 {
			t.Errorf("openai backend calls = %d, want 0", got)
		}
		if got := anthropicCaptured().header.Get("x-api-key"); got != anthropicKey {
			t.Errorf("anthropic x-api-key = %q, want %q", got, anthropicKey)
		}
	})

	t.Run("openai entry ignores anthropic backends", func(t *testing.T) {
		beforeOpenAI, beforeAnthropic := openaiCalls(), anthropicCalls()
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", `{"model":"claude-x"}`, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := openaiCalls() - beforeOpenAI; got != 1 {
			t.Errorf("openai backend calls = %d, want 1", got)
		}
		if got := anthropicCalls() - beforeAnthropic; got != 0 {
			t.Errorf("anthropic backend calls = %d, want 0", got)
		}
	})
}

func TestNoBackendForProtocol(t *testing.T) {
	cases := []struct {
		name          string
		configured    string
		path          string
		entryProtocol string
		anthropic     bool
	}{
		{"anthropic entry without anthropic backend", config.ProtocolOpenAI, "/v1/messages", config.ProtocolAnthropic, true},
		{"openai entry without openai backend", config.ProtocolAnthropic, "/v1/chat/completions", config.ProtocolOpenAI, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
			proxyServer := newProxy(t, serviceToken, backendConfig(tc.configured, backend.URL, "upstream-key", "test-model"))

			resp := send(t, http.MethodPost, proxyServer.URL+tc.path, `{"model":"test-model"}`, withAuth(tc.entryProtocol, nil))

			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
			}
			if tc.anthropic {
				apiErr := decodeAnthropicError(t, resp)
				if apiErr.Error.Type != "api_error" {
					t.Errorf("error.type = %q, want %q", apiErr.Error.Type, "api_error")
				}
			} else {
				apiErr := decodeOpenAIError(t, resp)
				if apiErr.Error.Code != "upstream_error" {
					t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "upstream_error")
				}
			}
			if got := calls(); got != 0 {
				t.Errorf("backend calls = %d, want 0 (request must not be forwarded)", got)
			}
		})
	}
}

func TestRequestTooLarge(t *testing.T) {
	largeBody := strings.Repeat("x", proxy.MaxRequestBodyBytes+1)

	cases := []struct {
		name          string
		configured    string
		path          string
		entryProtocol string
		anthropic     bool
	}{
		{"openai entry", config.ProtocolOpenAI, "/v1/chat/completions", config.ProtocolOpenAI, false},
		{"anthropic entry", config.ProtocolAnthropic, "/v1/messages", config.ProtocolAnthropic, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
			proxyServer := newProxy(t, serviceToken, backendConfig(tc.configured, backend.URL, "upstream-key", "test-model"))

			resp := send(t, http.MethodPost, proxyServer.URL+tc.path, largeBody, withAuth(tc.entryProtocol, nil))

			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
			}
			if tc.anthropic {
				apiErr := decodeAnthropicError(t, resp)
				if apiErr.Error.Type != "request_too_large" {
					t.Errorf("error.type = %q, want %q", apiErr.Error.Type, "request_too_large")
				}
			} else {
				apiErr := decodeOpenAIError(t, resp)
				if apiErr.Error.Code != "request_too_large" {
					t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "request_too_large")
				}
			}
			if got := calls(); got != 0 {
				t.Errorf("backend calls = %d, want 0 (request must not be forwarded)", got)
			}
		})
	}
}

func TestUpstreamFailureErrorFormat(t *testing.T) {
	cases := []struct {
		name          string
		configured    string
		path          string
		entryProtocol string
		anthropic     bool
	}{
		{"openai entry", config.ProtocolOpenAI, "/v1/chat/completions", config.ProtocolOpenAI, false},
		{"anthropic entry", config.ProtocolAnthropic, "/v1/messages", config.ProtocolAnthropic, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			backendURL := backend.URL
			backend.Close()

			proxyServer := newProxy(t, serviceToken, backendConfig(tc.configured, backendURL, "upstream-key", "test-model"))
			resp := send(t, http.MethodPost, proxyServer.URL+tc.path, `{"model":"test-model"}`, withAuth(tc.entryProtocol, nil))

			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
			}
			if tc.anthropic {
				apiErr := decodeAnthropicError(t, resp)
				if apiErr.Error.Type != "api_error" {
					t.Errorf("error.type = %q, want %q", apiErr.Error.Type, "api_error")
				}
			} else {
				apiErr := decodeOpenAIError(t, resp)
				if apiErr.Error.Code != "upstream_error" {
					t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "upstream_error")
				}
			}
		})
	}
}
