package proxy_test

import (
	"encoding/json"
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

const (
	serviceToken = "service-token"
	upstreamKey  = "config-key"
)

type capturedRequest struct {
	method   string
	path     string
	rawQuery string
	header   http.Header
	body     string
}

// newBackend starts a mock upstream that records the request it receives and
// replies with the given status, headers and body. It also reports how many
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

func newProxy(t *testing.T, backendURL, apiKey, authToken string) *httptest.Server {
	t.Helper()

	cfg := &config.Config{
		Listen:    "unused:0",
		AuthToken: authToken,
		Backend: config.Backend{
			BaseURL: backendURL,
			APIKey:  apiKey,
			Model:   "gpt-test",
			Timeout: config.Duration(5 * time.Second),
		},
	}
	handler, err := proxy.New(cfg)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

type openAIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeError(t *testing.T, resp *http.Response) openAIError {
	t.Helper()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("error Content-Type = %q, want application/json", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read error response body: %v", err)
	}
	var apiErr openAIError
	if err := json.Unmarshal(body, &apiErr); err != nil {
		t.Fatalf("error body is not JSON: %v (body %q)", err, body)
	}
	if apiErr.Error.Message == "" {
		t.Errorf("error.message is empty (body %q)", body)
	}
	if apiErr.Error.Type == "" {
		t.Errorf("error.type is empty (body %q)", body)
	}
	if apiErr.Error.Code == "" {
		t.Errorf("error.code is empty (body %q)", body)
	}
	return apiErr
}

func TestPassThroughVerbatim(t *testing.T) {
	const (
		requestBody  = `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}],"stream":false}`
		responseBody = `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`
	)

	backend, captured, _ := newBackend(t, http.StatusCreated, map[string]string{
		"Content-Type":    "application/json",
		"X-Upstream-Name": "mock-backend",
	}, responseBody)
	proxyServer := newProxy(t, backend.URL, upstreamKey, serviceToken)

	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/chat/completions?trace=abc", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+serviceToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom-Header", "keep-me")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call proxy: %v", err)
	}
	defer resp.Body.Close()
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read proxy response body: %v", err)
	}

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
	if want := "Bearer " + upstreamKey; got.header.Get("Authorization") != want {
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
	if want := "mock-backend"; resp.Header.Get("X-Upstream-Name") != want {
		t.Errorf("proxy X-Upstream-Name = %q, want %q", resp.Header.Get("X-Upstream-Name"), want)
	}
	if string(gotBody) != responseBody {
		t.Errorf("proxy body = %q, want verbatim %q", gotBody, responseBody)
	}
}

func TestMissingAuthorizationReturns401(t *testing.T) {
	backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
	proxyServer := newProxy(t, backend.URL, upstreamKey, serviceToken)

	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-test"}`))
	if err != nil {
		t.Fatalf("call proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	apiErr := decodeError(t, resp)
	if apiErr.Error.Code != "missing_api_key" {
		t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "missing_api_key")
	}
	if got := calls(); got != 0 {
		t.Errorf("backend calls = %d, want 0 (request must not be forwarded)", got)
	}
}

func TestWrongTokenReturns401(t *testing.T) {
	backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
	proxyServer := newProxy(t, backend.URL, upstreamKey, serviceToken)

	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-test"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	apiErr := decodeError(t, resp)
	if apiErr.Error.Code != "invalid_api_key" {
		t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "invalid_api_key")
	}
	if got := calls(); got != 0 {
		t.Errorf("backend calls = %d, want 0 (request must not be forwarded)", got)
	}
}

func TestUnknownPathNotForwarded(t *testing.T) {
	backend, _, calls := newBackend(t, http.StatusOK, nil, "should not be reached")
	proxyServer := newProxy(t, backend.URL, upstreamKey, serviceToken)

	req, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+serviceToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("proxy status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	apiErr := decodeError(t, resp)
	if apiErr.Error.Code != "not_found" {
		t.Errorf("error.code = %q, want %q", apiErr.Error.Code, "not_found")
	}
	if got := calls(); got != 0 {
		t.Errorf("backend calls = %d, want 0 (request must not be forwarded)", got)
	}
}
