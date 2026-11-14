package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-router/internal/config"
	"model-router/internal/providers"
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

func openAIInstance(backendURL, apiKey string, models ...string) config.Instance {
	return config.Instance{
		Protocol: config.ProtocolOpenAI,
		BaseURL:  backendURL,
		APIKey:   apiKey,
		Timeout:  config.Duration(5 * time.Second),
		Models:   models,
	}
}

func anthropicInstance(backendURL, apiKey string, roles map[string]string) config.Instance {
	return config.Instance{
		Protocol: config.ProtocolAnthropic,
		BaseURL:  backendURL,
		APIKey:   apiKey,
		Timeout:  config.Duration(5 * time.Second),
		Roles:    roles,
	}
}

func newProxy(t *testing.T, authToken string, instances ...config.Instance) *httptest.Server {
	t.Helper()

	handler, err := proxy.New(&config.Config{
		Listen:    "unused:0",
		AuthToken: authToken,
		Instances: instances,
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

func sendBasic(t *testing.T, method, url, body, user, pass string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call server: %v", err)
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
	proxyServer := newProxy(t, serviceToken, openAIInstance(backend.URL, openAIKey, "gpt-test"))

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
	proxyServer := newProxy(t, serviceToken, anthropicInstance(backend.URL, anthropicKey, map[string]string{
		"sonnet": "claude-test",
	}))

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
		instance  config.Instance
		anthropic bool
	}{
		{"openai", "/v1/chat/completions", openAIInstance("https://openai.example", "k", "m"), false},
		{"anthropic", "/v1/messages", anthropicInstance("https://anthropic.example", "k", map[string]string{"sonnet": "m"}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyServer := newProxy(t, serviceToken, tc.instance)

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
		})
	}
}

func TestWrongTokenReturns401(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		instance  config.Instance
		anthropic bool
	}{
		{"openai", "/v1/chat/completions", openAIInstance("https://openai.example", "k", "m"), false},
		{"anthropic", "/v1/messages", anthropicInstance("https://anthropic.example", "k", map[string]string{"sonnet": "m"}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyServer := newProxy(t, serviceToken, tc.instance)

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
			} else {
				apiErr := decodeOpenAIError(t, resp)
				if apiErr.Error.Code != "invalid_api_key" {
					t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "invalid_api_key")
				}
			}
		})
	}
}

func TestAnthropicRejectsAuthorizationBearer(t *testing.T) {
	proxyServer := newProxy(t, serviceToken, anthropicInstance("https://anthropic.example", anthropicKey, map[string]string{"sonnet": "m"}))

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
}

func TestOpenAIRejectsXAPIKeyOnly(t *testing.T) {
	proxyServer := newProxy(t, serviceToken, openAIInstance("https://openai.example", openAIKey, "m"))

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
}

func TestUnknownPathNotForwarded(t *testing.T) {
	backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
	proxyServer := newProxy(t, serviceToken, openAIInstance(backend.URL, openAIKey, "gpt-test"))

	resp := send(t, http.MethodGet, proxyServer.URL+"/v1/unknown", "", map[string]string{
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

func TestRouteByExactModel(t *testing.T) {
	first, firstCaptured, firstCalls := newBackend(t, http.StatusOK, nil, `{"backend":"first"}`)
	second, secondCaptured, secondCalls := newBackend(t, http.StatusOK, nil, `{"backend":"second"}`)
	proxyServer := newProxy(t, serviceToken,
		openAIInstance(first.URL, "first-key", "gpt-a"),
		openAIInstance(second.URL, "second-key", "gpt-b"),
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
			if got := firstCalls() - beforeFirst; (got == 1) != tc.wantFirst {
				t.Errorf("first backend calls = %d, want first=%v", got, tc.wantFirst)
			}
			if got := secondCalls() - beforeSecond; (got == 1) != tc.wantSecond {
				t.Errorf("second backend calls = %d, want second=%v", got, tc.wantSecond)
			}
			var forwarded string
			if tc.wantFirst {
				forwarded = firstCaptured().body
			}
			if tc.wantSecond {
				forwarded = secondCaptured().body
			}
			if forwarded != body {
				t.Errorf("forwarded body = %q, want verbatim %q (R4a/R5 must not rewrite)", forwarded, body)
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
				openAIInstance(first.URL, "first-key", "gpt-a"),
				openAIInstance(second.URL, "second-key", "gpt-b"),
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

func TestRouteByRoles(t *testing.T) {
	newRoleProxy := func(t *testing.T) (*httptest.Server, func() capturedRequest, func() int32, func() capturedRequest, func() int32) {
		t.Helper()
		flash, flashCaptured, flashCalls := newBackend(t, http.StatusOK, nil, `{"backend":"flash"}`)
		pro, proCaptured, proCalls := newBackend(t, http.StatusOK, nil, `{"backend":"pro"}`)
		proxyServer := newProxy(t, serviceToken,
			anthropicInstance(flash.URL, "flash-key", map[string]string{
				"sonnet": "deepseek-v4-flash",
				"haiku":  "deepseek-v4-flash",
			}),
			anthropicInstance(pro.URL, "pro-key", map[string]string{
				"opus":  "deepseek-v4-pro",
				"fable": "deepseek-v4-pro",
			}),
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

	t.Run("exact model match wins over role on same instance", func(t *testing.T) {
		backend, captured, _ := newBackend(t, http.StatusOK, nil, `{"backend":"only"}`)
		proxyServer := newProxy(t, serviceToken,
			anthropicInstance(backend.URL, "only-key", map[string]string{"sonnet": "sonnet"}),
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

	t.Run("first instance wins for the same role", func(t *testing.T) {
		first, firstCaptured, firstCalls := newBackend(t, http.StatusOK, nil, `{"backend":"first"}`)
		second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"backend":"second"}`)
		proxyServer := newProxy(t, serviceToken,
			anthropicInstance(first.URL, "first-key", map[string]string{"sonnet": "first-real"}),
			anthropicInstance(second.URL, "second-key", map[string]string{"sonnet": "second-real"}),
		)

		body := `{"model":"sonnet","messages":[]}`
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
}

func TestRouteWildcardFallback(t *testing.T) {
	t.Run("unknown model uses first wildcard instance", func(t *testing.T) {
		primary, _, primaryCalls := newBackend(t, http.StatusOK, nil, `{"backend":"primary"}`)
		fallback, fallbackCaptured, fallbackCalls := newBackend(t, http.StatusOK, nil, `{"backend":"fallback"}`)
		proxyServer := newProxy(t, serviceToken,
			anthropicInstance(primary.URL, "primary-key", map[string]string{"sonnet": "primary-model"}),
			anthropicInstance(fallback.URL, "fallback-key", map[string]string{"*": "fallback-model"}),
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
			anthropicInstance(wildcard.URL, "wildcard-key", map[string]string{"*": "wildcard-model"}),
			anthropicInstance(roleBackend.URL, "role-key", map[string]string{"sonnet": "sonnet-model"}),
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

	t.Run("exact model wins over wildcard", func(t *testing.T) {
		wildcard, _, wildcardCalls := newBackend(t, http.StatusOK, nil, `{"backend":"wildcard"}`)
		exact, exactCaptured, exactCalls := newBackend(t, http.StatusOK, nil, `{"backend":"exact"}`)
		proxyServer := newProxy(t, serviceToken,
			anthropicInstance(wildcard.URL, "wildcard-key", map[string]string{"*": "wildcard-model"}),
			anthropicInstance(exact.URL, "exact-key", map[string]string{"opus": "explicit-model"}),
		)

		body := `{"model":"explicit-model","messages":[]}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := wildcardCalls(); got != 0 {
			t.Errorf("wildcard backend calls = %d, want 0", got)
		}
		if got := exactCalls(); got != 1 {
			t.Errorf("exact backend calls = %d, want 1", got)
		}
		if got := exactCaptured().body; got != body {
			t.Errorf("backend body = %q, want verbatim (R4a must not rewrite) %q", got, body)
		}
	})

	t.Run("first wildcard instance wins", func(t *testing.T) {
		first, firstCaptured, firstCalls := newBackend(t, http.StatusOK, nil, `{"backend":"first"}`)
		second, _, secondCalls := newBackend(t, http.StatusOK, nil, `{"backend":"second"}`)
		proxyServer := newProxy(t, serviceToken,
			anthropicInstance(first.URL, "first-key", map[string]string{"*": "first-model"}),
			anthropicInstance(second.URL, "second-key", map[string]string{"*": "second-model"}),
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

	t.Run("R5 chain uses wildcard candidates and keeps the body untouched", func(t *testing.T) {
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
				first, _, firstCalls := newBackend(t, http.StatusOK, nil, `{"backend":"first"}`)
				wildcard, wildcardCaptured, wildcardCalls := newBackend(t, http.StatusOK, nil, `{"backend":"wildcard"}`)
				proxyServer := newProxy(t, serviceToken,
					anthropicInstance(first.URL, "first-key", map[string]string{"sonnet": "first-model"}),
					anthropicInstance(wildcard.URL, "wildcard-key", map[string]string{"*": "wildcard-model"}),
				)

				resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
				}
				// §11.2 R5: instances without "*" are skipped; the wildcard
				// instance is the candidate and the body is not rewritten.
				if got := firstCalls(); got != 0 {
					t.Errorf("first backend calls = %d, want 0 (no wildcard)", got)
				}
				if got := wildcardCalls(); got != 1 {
					t.Errorf("wildcard backend calls = %d, want 1", got)
				}
				if got := wildcardCaptured().body; got != body {
					t.Errorf("backend body = %q, want verbatim %q", got, body)
				}
			})
		}
	})
}

func TestProtocolListsAreIndependent(t *testing.T) {
	openaiBackend, openaiCaptured, openaiCalls := newBackend(t, http.StatusOK, nil, `{"backend":"openai"}`)
	anthropicBackend, anthropicCaptured, anthropicCalls := newBackend(t, http.StatusOK, nil, `{"backend":"anthropic"}`)
	proxyServer := newProxy(t, serviceToken,
		openAIInstance(openaiBackend.URL, openAIKey, "shared-model"),
		anthropicInstance(anthropicBackend.URL, anthropicKey, map[string]string{"sonnet": "claude-x"}),
	)

	t.Run("anthropic entry ignores openai instances", func(t *testing.T) {
		beforeOpenAI, beforeAnthropic := openaiCalls(), anthropicCalls()
		body := `{"model":"shared-model"}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/messages", body, withAuth(config.ProtocolAnthropic, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := anthropicCalls() - beforeAnthropic; got != 1 {
			t.Errorf("anthropic backend calls = %d, want 1", got)
		}
		if got := openaiCalls() - beforeOpenAI; got != 0 {
			t.Errorf("openai backend calls = %d, want 0", got)
		}
		if got := anthropicCaptured().body; got != body {
			t.Errorf("anthropic body = %q, want R5 verbatim %q", got, body)
		}
	})

	t.Run("openai entry ignores anthropic instances", func(t *testing.T) {
		beforeOpenAI, beforeAnthropic := openaiCalls(), anthropicCalls()
		body := `{"model":"claude-x"}`
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/chat/completions", body, withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
		}
		if got := openaiCalls() - beforeOpenAI; got != 1 {
			t.Errorf("openai backend calls = %d, want 1", got)
		}
		if got := anthropicCalls() - beforeAnthropic; got != 0 {
			t.Errorf("anthropic backend calls = %d, want 0", got)
		}
		if got := openaiCaptured().body; got != body {
			t.Errorf("openai body = %q, want R5 verbatim %q", got, body)
		}
	})
}

func TestNoBackendForProtocol(t *testing.T) {
	cases := []struct {
		name          string
		instance      config.Instance
		path          string
		entryProtocol string
		anthropic     bool
	}{
		{"anthropic entry without anthropic instance", openAIInstance("https://openai.example", "k", "m"), "/v1/messages", config.ProtocolAnthropic, true},
		{"openai entry without openai instance", anthropicInstance("https://anthropic.example", "k", map[string]string{"sonnet": "m"}), "/v1/chat/completions", config.ProtocolOpenAI, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyServer := newProxy(t, serviceToken, tc.instance)

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

func TestRequestTooLarge(t *testing.T) {
	largeBody := strings.Repeat("x", proxy.MaxRequestBodyBytes+1)

	cases := []struct {
		name          string
		instance      config.Instance
		path          string
		entryProtocol string
		anthropic     bool
	}{
		{"openai entry", openAIInstance("https://openai.example", "k", "m"), "/v1/chat/completions", config.ProtocolOpenAI, false},
		{"anthropic entry", anthropicInstance("https://anthropic.example", "k", map[string]string{"sonnet": "m"}), "/v1/messages", config.ProtocolAnthropic, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyServer := newProxy(t, serviceToken, tc.instance)

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
		})
	}
}

func TestUpstreamFailureErrorFormat(t *testing.T) {
	cases := []struct {
		name          string
		instance      func(string) config.Instance
		path          string
		entryProtocol string
		anthropic     bool
	}{
		{"openai entry", func(u string) config.Instance { return openAIInstance(u, "k", "m") }, "/v1/chat/completions", config.ProtocolOpenAI, false},
		{"anthropic entry", func(u string) config.Instance {
			return anthropicInstance(u, "k", map[string]string{"sonnet": "m"})
		}, "/v1/messages", config.ProtocolAnthropic, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			backendURL := backend.URL
			backend.Close()

			proxyServer := newProxy(t, serviceToken, tc.instance(backendURL))
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

func TestModelsOpenAI(t *testing.T) {
	first, _, firstCalls := newBackend(t, http.StatusOK, nil, "unused")
	second, _, secondCalls := newBackend(t, http.StatusOK, nil, "unused")
	proxyServer := newProxy(t, serviceToken,
		openAIInstance(first.URL, "first-key", "gpt-a", "gpt-a", "gpt-b"),
		openAIInstance(second.URL, "second-key", "gpt-c"),
		anthropicInstance("https://anthropic.example", "anthropic-key", map[string]string{
			"sonnet": "claude-only",
			"*":      "wildcard-model",
		}),
	)

	resp := send(t, http.MethodGet, proxyServer.URL+"/v1/models", "", withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body := readBody(t, resp)
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("models response is not JSON: %v (body %q)", err, body)
	}
	if list.Object != "list" {
		t.Errorf("object = %q, want %q", list.Object, "list")
	}
	ids := make([]string, 0, len(list.Data))
	for _, item := range list.Data {
		ids = append(ids, item.ID)
		if item.Object != "model" || item.OwnedBy != "model-router" || item.Created <= 0 {
			t.Errorf("model entry has wrong shape: %+v", item)
		}
	}
	want := []string{"gpt-a", "gpt-b", "gpt-c"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("model ids = %v, want %v (instance order, dedupe)", ids, want)
	}
	if firstCalls() != 0 || secondCalls() != 0 {
		t.Errorf("upstream calls = %d/%d, want 0 (models are generated locally)", firstCalls(), secondCalls())
	}
}

func TestModelsAnthropic(t *testing.T) {
	proxyServer := newProxy(t, serviceToken,
		anthropicInstance("https://anthropic.example", "anthropic-key", map[string]string{
			"sonnet": "claude-sonnet-real",
			"opus":   "claude-opus-real",
			"haiku":  "claude-sonnet-real",
			"*":      "fallback-model",
		}),
		openAIInstance("https://openai.example", "openai-key", "gpt-only"),
	)

	resp := send(t, http.MethodGet, proxyServer.URL+"/v1/models", "", withAuth(config.ProtocolAnthropic, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body := readBody(t, resp)
	var list struct {
		Data []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		FirstID string `json:"first_id"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("models response is not JSON: %v (body %q)", err, body)
	}
	ids := make([]string, 0, len(list.Data))
	for _, item := range list.Data {
		ids = append(ids, item.ID)
		if item.Type != "model" || item.DisplayName != item.ID {
			t.Errorf("model entry has wrong shape: %+v", item)
		}
		if _, err := time.Parse(time.RFC3339, item.CreatedAt); err != nil {
			t.Errorf("model %q created_at = %q, want RFC3339: %v", item.ID, item.CreatedAt, err)
		}
	}
	want := []string{"claude-sonnet-real", "claude-opus-real"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("model ids = %v, want %v (concrete roles only, dedupe)", ids, want)
	}
	if list.HasMore {
		t.Error("has_more = true, want false")
	}
	if list.FirstID != "claude-sonnet-real" || list.LastID != "claude-opus-real" {
		t.Errorf("first_id/last_id = %q/%q, want first/last ids", list.FirstID, list.LastID)
	}
}

func TestModelsAuthAndMethod(t *testing.T) {
	proxyServer := newProxy(t, serviceToken,
		openAIInstance("https://openai.example", "openai-key", "gpt-a"),
		anthropicInstance("https://anthropic.example", "anthropic-key", map[string]string{"sonnet": "claude-a"}),
	)

	t.Run("openai requires bearer", func(t *testing.T) {
		resp := send(t, http.MethodGet, proxyServer.URL+"/v1/models", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if apiErr := decodeOpenAIError(t, resp); apiErr.Error.Code != "missing_api_key" {
			t.Errorf("error.code = %q, want missing_api_key", apiErr.Error.Code)
		}
	})

	t.Run("anthropic rejects wrong x-api-key", func(t *testing.T) {
		resp := send(t, http.MethodGet, proxyServer.URL+"/v1/models", "", map[string]string{
			"x-api-key": "wrong-token",
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if apiErr := decodeAnthropicError(t, resp); apiErr.Error.Type != "authentication_error" {
			t.Errorf("error.type = %q, want authentication_error", apiErr.Error.Type)
		}
	})

	t.Run("post is rejected with 405", func(t *testing.T) {
		resp := send(t, http.MethodPost, proxyServer.URL+"/v1/models", "", withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != http.MethodGet {
			t.Errorf("Allow = %q, want GET", got)
		}
		if apiErr := decodeOpenAIError(t, resp); apiErr.Error.Code != "method_not_allowed" {
			t.Errorf("error.code = %q, want method_not_allowed", apiErr.Error.Code)
		}
	})
}

const adminTestConfig = `{
	"listen": "127.0.0.1:18080",
	"auth_token": "tok",
	"admin_user": "admin",
	"admin_password": "secret",
	"fallback": {"failure_threshold": 3, "degrade_duration": "5m", "min_tokens_per_second": 30},
	"instances": {
		"openai": [{
			"base_url": "https://api.example",
			"api_key": "sk-real",
			"models": ["gpt-real"],
			"timeout": "15s"
		}]
	}
}`

const adminNewConfig = `{
	"listen": "127.0.0.1:18080",
	"auth_token": "new-token",
	"admin_user": "admin",
	"admin_password": "secret",
	"instances": {
		"openai": [{
			"base_url": "https://api.new",
			"api_key": "new-key",
			"models": ["new-model"]
		}]
	}
}`

func newAdminServer(t *testing.T, configJSON string) (*httptest.Server, string) {
	t.Helper()

	cfg, err := config.Parse([]byte(configJSON))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	server, err := proxy.NewServer(path, cfg)
	if err != nil {
		t.Fatalf("proxy.NewServer: %v", err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return ts, path
}

func decodeConfig(t *testing.T, resp *http.Response) config.Config {
	t.Helper()

	body := readBody(t, resp)
	var cfg config.Config
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("config response is not JSON: %v (body %q)", err, body)
	}
	return cfg
}

func modelIDs(t *testing.T, resp *http.Response) []string {
	t.Helper()

	body := readBody(t, resp)
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("models response is not JSON: %v (body %q)", err, body)
	}
	ids := make([]string, 0, len(list.Data))
	for _, item := range list.Data {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestAdminDisabled(t *testing.T) {
	server, _ := newAdminServer(t, `{
		"auth_token": "tok",
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
	}`)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin"},
		{http.MethodGet, "/admin/"},
		{http.MethodGet, "/api/config"},
		{http.MethodPost, "/api/reload"},
		{http.MethodPost, "/api/probe/test"},
		{http.MethodPost, "/api/probe/models"},
		{http.MethodPost, "/api/config/validate"},
		{http.MethodGet, "/api/providers"},
		{http.MethodGet, "/api/unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp := send(t, tc.method, server.URL+tc.path, "", nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s status = %d, want 404", tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}

func TestAdminAuth(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	t.Run("admin page requires basic auth", func(t *testing.T) {
		resp := send(t, http.MethodGet, server.URL+"/admin", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, `Basic realm="model-router"`) {
			t.Errorf("WWW-Authenticate = %q, want Basic realm", got)
		}
	})

	t.Run("wrong password rejected", func(t *testing.T) {
		resp := sendBasic(t, http.MethodGet, server.URL+"/admin", "", "admin", "wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("authorized admin page", func(t *testing.T) {
		resp := sendBasic(t, http.MethodGet, server.URL+"/admin", "", "admin", "secret")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", got)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, "<!DOCTYPE html>") || !strings.Contains(body, "model-router") {
			t.Errorf("admin page body does not look like the embedded UI")
		}
	})

	t.Run("authorized config api", func(t *testing.T) {
		resp := send(t, http.MethodGet, server.URL+"/api/config", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status without auth = %d, want 401", resp.StatusCode)
		}
		resp = sendBasic(t, http.MethodGet, server.URL+"/api/config", "", "admin", "secret")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		cfg := decodeConfig(t, resp)
		if cfg.AuthToken != "tok" || cfg.AdminUser != "admin" || cfg.AdminPassword != "secret" {
			t.Errorf("config credentials mismatch: %+v", cfg)
		}
		if len(cfg.Instances) != 1 || !reflect.DeepEqual(cfg.Instances[0].Models, []string{"gpt-real"}) {
			t.Errorf("config instances mismatch: %+v", cfg.Instances)
		}
	})
}

func TestAPIConfigPut(t *testing.T) {
	server, path := newAdminServer(t, adminTestConfig)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}

	t.Run("unauthorized put does not write", func(t *testing.T) {
		resp := send(t, http.MethodPut, server.URL+"/api/config", adminNewConfig, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read config file: %v", err)
		}
		if string(after) != string(before) {
			t.Error("config file changed after unauthorized PUT")
		}
	})

	t.Run("invalid put does not write", func(t *testing.T) {
		resp := sendBasic(t, http.MethodPut, server.URL+"/api/config", `{"auth_token":"x"}`, "admin", "secret")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read config file: %v", err)
		}
		if string(after) != string(before) {
			t.Error("config file changed after invalid PUT")
		}
	})

	t.Run("valid put writes file without taking effect", func(t *testing.T) {
		resp := sendBasic(t, http.MethodPut, server.URL+"/api/config", adminNewConfig, "admin", "secret")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		loaded, err := config.Load(path)
		if err != nil {
			t.Fatalf("reload written config: %v", err)
		}
		if loaded.AuthToken != "new-token" {
			t.Errorf("written auth_token = %q, want new-token", loaded.AuthToken)
		}
		if len(loaded.Instances) != 1 || !reflect.DeepEqual(loaded.Instances[0].Models, []string{"new-model"}) {
			t.Errorf("written instances = %+v, want new-model", loaded.Instances)
		}

		resp = sendBasic(t, http.MethodGet, server.URL+"/api/config", "", "admin", "secret")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET config status = %d, want 200", resp.StatusCode)
		}
		effective := decodeConfig(t, resp)
		if effective.AuthToken != "tok" || !reflect.DeepEqual(effective.Instances[0].Models, []string{"gpt-real"}) {
			t.Errorf("effective config changed without reload: %+v", effective)
		}
	})
}

func TestAPIReload(t *testing.T) {
	server, path := newAdminServer(t, adminTestConfig)

	resp := sendBasic(t, http.MethodPut, server.URL+"/api/config", adminNewConfig, "admin", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT config status = %d, want 200", resp.StatusCode)
	}
	resp = sendBasic(t, http.MethodPost, server.URL+"/api/reload", "", "admin", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload status = %d, want 200", resp.StatusCode)
	}

	t.Run("old token rejected after reload", func(t *testing.T) {
		resp := send(t, http.MethodGet, server.URL+"/v1/models", "", map[string]string{
			"Authorization": "Bearer tok",
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("new token sees new models", func(t *testing.T) {
		resp := send(t, http.MethodGet, server.URL+"/v1/models", "", map[string]string{
			"Authorization": "Bearer new-token",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		ids := modelIDs(t, resp)
		if !reflect.DeepEqual(ids, []string{"new-model"}) {
			t.Errorf("model ids = %v, want [new-model]", ids)
		}
	})

	t.Run("failed reload keeps old config", func(t *testing.T) {
		if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
			t.Fatalf("corrupt config file: %v", err)
		}
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/reload", "", "admin", "secret")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("reload status = %d, want 400", resp.StatusCode)
		}

		resp = send(t, http.MethodGet, server.URL+"/v1/models", "", map[string]string{
			"Authorization": "Bearer new-token",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status after failed reload = %d, want 200 (old config kept)", resp.StatusCode)
		}
		ids := modelIDs(t, resp)
		if !reflect.DeepEqual(ids, []string{"new-model"}) {
			t.Errorf("model ids after failed reload = %v, want [new-model]", ids)
		}
	})
}

type validateResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func TestValidateConfig(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	t.Run("requires admin auth", func(t *testing.T) {
		resp := send(t, http.MethodPost, server.URL+"/api/config/validate", adminTestConfig, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("valid config", func(t *testing.T) {
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/config/validate", adminTestConfig, "admin", "secret")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var result validateResult
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("validate response is not JSON: %v", err)
		}
		if !result.OK {
			t.Errorf("ok = false, want true (%s)", result.Error)
		}
	})

	t.Run("legacy backends rejected", func(t *testing.T) {
		legacy := `{"auth_token":"tok","backends":[{"protocol":"openai","base_url":"https://example.com","api_key":"k","model":"m"}]}`
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/config/validate", legacy, "admin", "secret")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		var result validateResult
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("validate response is not JSON: %v", err)
		}
		if result.OK || !strings.Contains(result.Error, "instances") {
			t.Errorf("result = %+v, want ok=false with migration hint", result)
		}
	})

	t.Run("invalid instance rejected", func(t *testing.T) {
		invalid := `{"auth_token":"tok","instances":{"openai":[{"base_url":"https://example.com","api_key":"k"}]}}`
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/config/validate", invalid, "admin", "secret")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("legacy instances array rejected", func(t *testing.T) {
		legacy := `{"auth_token":"tok","instances":[{"protocol":"openai","base_url":"https://example.com","api_key":"k","models":["m"]}]}`
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/config/validate", legacy, "admin", "secret")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		var result validateResult
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("validate response is not JSON: %v", err)
		}
		if result.OK || !strings.Contains(result.Error, "分组结构") {
			t.Errorf("result = %+v, want grouped-instances migration hint", result)
		}
	})
}

type probeTestResult struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Message   string `json:"message"`
}

func probeBody(proto, baseURL, apiKey string) string {
	return probeBodyWithProvider(proto, baseURL, apiKey, "")
}

func probeBodyWithProvider(proto, baseURL, apiKey, provider string) string {
	return `{"protocol":"` + proto + `","base_url":"` + baseURL + `","api_key":"` + apiKey + `","provider":"` + provider + `","timeout":"5s"}`
}

func TestProbeTest(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	t.Run("openai reachable", func(t *testing.T) {
		upstream, captured, calls := newBackend(t, http.StatusOK, nil, `{"data":[{"id":"m1"}]}`)
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/test", probeBody("openai", upstream.URL, "probe-key"), "admin", "secret")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var result probeTestResult
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("probe response is not JSON: %v", err)
		}
		if !result.OK {
			t.Errorf("ok = false, want true (%s)", result.Message)
		}
		if result.LatencyMs < 0 {
			t.Errorf("latency_ms = %d, want >= 0", result.LatencyMs)
		}
		if got := calls(); got != 1 {
			t.Errorf("upstream calls = %d, want 1", got)
		}
		req := captured()
		if req.path != "/v1/models" {
			t.Errorf("probe path = %q, want /v1/models", req.path)
		}
		if want := "Bearer probe-key"; req.header.Get("Authorization") != want {
			t.Errorf("probe Authorization = %q, want %q", req.header.Get("Authorization"), want)
		}
	})

	t.Run("anthropic reachable uses POST /v1/messages", func(t *testing.T) {
		upstream, captured, _ := newBackend(t, http.StatusOK, nil, `{"id":"msg_1"}`)
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/test", probeBody("anthropic", upstream.URL, "probe-key"), "admin", "secret")
		var result probeTestResult
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("probe response is not JSON: %v", err)
		}
		if !result.OK {
			t.Errorf("ok = false, want true (%s)", result.Message)
		}
		req := captured()
		if req.method != http.MethodPost {
			t.Errorf("probe method = %q, want POST", req.method)
		}
		if req.path != "/v1/messages" {
			t.Errorf("probe path = %q, want /v1/messages", req.path)
		}
		if req.header.Get("x-api-key") != "probe-key" {
			t.Errorf("probe x-api-key = %q, want probe-key", req.header.Get("x-api-key"))
		}
		if got := req.header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q, want 2023-06-01", got)
		}
		if got := req.header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := req.header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		wantBody := `{"model":"ping","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`
		if req.body != wantBody {
			t.Errorf("probe body = %q, want %q", req.body, wantBody)
		}
	})

	t.Run("anthropic 400 counts as reachable", func(t *testing.T) {
		upstream, _, _ := newBackend(t, http.StatusBadRequest, nil, `{"type":"error"}`)
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/test", probeBody("anthropic", upstream.URL, "probe-key"), "admin", "secret")
		var result probeTestResult
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("probe response is not JSON: %v", err)
		}
		if !result.OK {
			t.Errorf("ok = false, want true for 400 (auth passed, endpoint exists): %s", result.Message)
		}
		if !strings.Contains(result.Message, "预期") {
			t.Errorf("message = %q, want an explanation about the placeholder model", result.Message)
		}
	})

	t.Run("anthropic 401 fails with api key message", func(t *testing.T) {
		upstream, _, _ := newBackend(t, http.StatusUnauthorized, nil, `{"type":"error"}`)
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/test", probeBody("anthropic", upstream.URL, "bad-key"), "admin", "secret")
		var result probeTestResult
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("probe response is not JSON: %v", err)
		}
		if result.OK {
			t.Error("ok = true, want false for 401")
		}
		if !strings.Contains(result.Message, "api_key") {
			t.Errorf("message = %q, want mention of api_key", result.Message)
		}
	})

	t.Run("anthropic 404 fails with base_url message", func(t *testing.T) {
		upstream, _, _ := newBackend(t, http.StatusNotFound, nil, `{"type":"error"}`)
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/test", probeBody("anthropic", upstream.URL, "probe-key"), "admin", "secret")
		var result probeTestResult
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("probe response is not JSON: %v", err)
		}
		if result.OK {
			t.Error("ok = true, want false for 404")
		}
		if !strings.Contains(result.Message, "base_url") {
			t.Errorf("message = %q, want mention of base_url", result.Message)
		}
	})

	t.Run("non-2xx reports failure", func(t *testing.T) {
		upstream, _, _ := newBackend(t, http.StatusUnauthorized, nil, "nope")
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/test", probeBody("openai", upstream.URL, "bad-key"), "admin", "secret")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (probe result)", resp.StatusCode)
		}
		var result probeTestResult
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("probe response is not JSON: %v", err)
		}
		if result.OK {
			t.Error("ok = true, want false for 401")
		}
		if !strings.Contains(result.Message, "api_key") {
			t.Errorf("message = %q, want mention of api_key", result.Message)
		}
	})

	t.Run("network error reports failure", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		backendURL := backend.URL
		backend.Close()

		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/test", probeBody("openai", backendURL, "k"), "admin", "secret")
		var result probeTestResult
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("probe response is not JSON: %v", err)
		}
		if result.OK || result.Message == "" {
			t.Errorf("result = %+v, want ok=false with message", result)
		}
	})

	t.Run("invalid request rejected", func(t *testing.T) {
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/test", `{"protocol":"gemini"}`, "admin", "secret")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestProvidersAPI(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	t.Run("requires admin auth", func(t *testing.T) {
		resp := send(t, http.MethodGet, server.URL+"/api/providers", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("returns built-in providers", func(t *testing.T) {
		resp := sendBasic(t, http.MethodGet, server.URL+"/api/providers", "", "admin", "secret")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var list []providers.Provider
		if err := json.Unmarshal([]byte(readBody(t, resp)), &list); err != nil {
			t.Fatalf("providers response is not JSON array: %v", err)
		}
		if len(list) != 11 {
			t.Fatalf("len(providers) = %d, want 11", len(list))
		}
		var deepseek *providers.Provider
		for i := range list {
			if list[i].ID == "deepseek" {
				deepseek = &list[i]
			}
		}
		if deepseek == nil {
			t.Fatal("deepseek provider missing")
		}
		override, ok := deepseek.ModelsOverride["anthropic"]
		if !ok {
			t.Fatal("deepseek anthropic models_override missing")
		}
		if want := "https://api.deepseek.com"; override.BaseURL != want {
			t.Errorf("override base_url = %q, want %q", override.BaseURL, want)
		}
		if want := providers.AuthBearer; override.Auth != want {
			t.Errorf("override auth = %q, want %q", override.Auth, want)
		}
	})

	t.Run("post is rejected", func(t *testing.T) {
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/providers", "", "admin", "secret")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})
}

func TestProbeModelsWithProvider(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	t.Run("provider without protocol override uses request base_url", func(t *testing.T) {
		upstream, captured, calls := newBackend(t, http.StatusOK, nil, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/models",
			probeBodyWithProvider("openai", upstream.URL, "probe-key", "deepseek"), "admin", "secret")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var result struct {
			Models []string `json:"models"`
		}
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("response is not JSON: %v", err)
		}
		if want := []string{"m1", "m2"}; !reflect.DeepEqual(result.Models, want) {
			t.Errorf("models = %v, want %v", result.Models, want)
		}
		if got := calls(); got != 1 {
			t.Errorf("upstream calls = %d, want 1", got)
		}
		req := captured()
		if req.path != "/v1/models" {
			t.Errorf("probe path = %q, want /v1/models", req.path)
		}
		if want := "Bearer probe-key"; req.header.Get("Authorization") != want {
			t.Errorf("Authorization = %q, want %q", req.header.Get("Authorization"), want)
		}
	})

	t.Run("unknown provider is rejected", func(t *testing.T) {
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/models",
			probeBodyWithProvider("anthropic", "https://example.com", "k", "nope"), "admin", "secret")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestProbeModels(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	t.Run("parses model ids", func(t *testing.T) {
		upstream, captured, _ := newBackend(t, http.StatusOK, nil, `{"data":[{"id":"m1"},{"id":"m2"},{"id":""}]}`)
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/models", probeBody("openai", upstream.URL, "probe-key"), "admin", "secret")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var result struct {
			Models []string `json:"models"`
		}
		if err := json.Unmarshal([]byte(readBody(t, resp)), &result); err != nil {
			t.Fatalf("models response is not JSON: %v", err)
		}
		if want := []string{"m1", "m2"}; !reflect.DeepEqual(result.Models, want) {
			t.Errorf("models = %v, want %v", result.Models, want)
		}
		if req := captured(); req.path != "/v1/models" {
			t.Errorf("probe path = %q, want /v1/models", req.path)
		}
	})

	t.Run("upstream 404 gives manual-input guidance", func(t *testing.T) {
		upstream, _, _ := newBackend(t, http.StatusNotFound, nil, "not found")
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/models", probeBody("anthropic", upstream.URL, "k"), "admin", "secret")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, "不支持模型列表查询") || !strings.Contains(body, "手动输入") {
			t.Errorf("body = %q, want a friendly unsupported-models error", body)
		}
	})

	t.Run("missing data reports error", func(t *testing.T) {
		upstream, _, _ := newBackend(t, http.StatusOK, nil, `{"object":"list"}`)
		resp := sendBasic(t, http.MethodPost, server.URL+"/api/probe/models", probeBody("openai", upstream.URL, "k"), "admin", "secret")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		if body := readBody(t, resp); !strings.Contains(body, "manual") {
			t.Errorf("body = %q, want a manual-input hint", body)
		}
	})

	t.Run("requires admin auth", func(t *testing.T) {
		resp := send(t, http.MethodPost, server.URL+"/api/probe/models", probeBody("openai", "https://example.com", "k"), nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})
}