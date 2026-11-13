// Package proxy implements the M2 OpenAI/Anthropic passthrough proxy.
package proxy

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"model-router/internal/config"
)

// API paths served by this proxy.
const (
	PathOpenAI    = "/v1/chat/completions"
	PathAnthropic = "/v1/messages"
)

// MaxRequestBodyBytes caps how much of a request body is buffered for
// model-based routing. Larger requests are rejected with 413.
const MaxRequestBodyBytes = 10 << 20 // 10 MiB

var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

type protocol string

const (
	protocolOpenAI    protocol = config.ProtocolOpenAI
	protocolAnthropic protocol = config.ProtocolAnthropic
)

type backend struct {
	baseURL  *url.URL
	apiKey   string
	model    string
	matches  []string
	roles    []string
	wildcard bool
	client   *http.Client
}

// Handler routes OpenAI and Anthropic requests to their configured backends
// without translating the protocol.
type Handler struct {
	authToken  string
	byProtocol map[protocol][]*backend
}

// New builds a Handler from the configured backends.
func New(cfg *config.Config) (*Handler, error) {
	if cfg.AuthToken == "" {
		return nil, errors.New("auth_token is required")
	}
	if len(cfg.Backends) == 0 {
		return nil, errors.New("backends must contain at least one backend")
	}

	h := &Handler{
		authToken:  cfg.AuthToken,
		byProtocol: make(map[protocol][]*backend),
	}
	for i, bc := range cfg.Backends {
		proto := protocol(strings.ToLower(strings.TrimSpace(bc.Protocol)))
		if proto != protocolOpenAI && proto != protocolAnthropic {
			return nil, fmt.Errorf("backends[%d]: unsupported protocol %q", i, bc.Protocol)
		}
		if bc.APIKey == "" {
			return nil, fmt.Errorf("backends[%d]: api_key is required", i)
		}
		base, err := url.Parse(bc.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("backends[%d]: parse base_url: %w", i, err)
		}
		if base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
			return nil, fmt.Errorf("backends[%d]: base_url must be an absolute http(s) URL, got %q", i, bc.BaseURL)
		}
		matches := make([]string, 0, len(bc.Matches))
		for _, alias := range bc.Matches {
			if alias = strings.TrimSpace(alias); alias != "" {
				matches = append(matches, alias)
			}
		}
		roles := make([]string, 0, len(bc.Roles))
		wildcard := false
		for _, raw := range bc.Roles {
			role := strings.ToLower(strings.TrimSpace(raw))
			if !config.IsValidRole(role) {
				return nil, fmt.Errorf("backends[%d]: invalid role %q", i, raw)
			}
			if role == config.RoleWildcard {
				wildcard = true
				continue
			}
			roles = append(roles, role)
		}
		h.byProtocol[proto] = append(h.byProtocol[proto], &backend{
			baseURL:  base,
			apiKey:   bc.APIKey,
			model:    bc.Model,
			matches:  matches,
			roles:    roles,
			wildcard: wildcard,
			client:   newClient(bc.Timeout.Duration()),
		})
	}
	return h, nil
}

func newClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: transport}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	proto, known := protocolForPath(r.URL.Path)
	if !h.authorized(proto, r) {
		h.writeAuthError(w, proto, r)
		return
	}
	if !known {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "not_found",
			"only "+PathOpenAI+" and "+PathAnthropic+" are served")
		return
	}

	backends := h.byProtocol[proto]
	if len(backends) == 0 {
		writeProtocolError(w, proto, http.StatusBadGateway, "no backend configured for protocol "+string(proto))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProtocolError(w, proto, http.StatusRequestEntityTooLarge, "request body exceeds the 10 MiB limit")
			return
		}
		writeProtocolError(w, proto, http.StatusBadRequest, "cannot read request body")
		return
	}

	selected, forwardBody := selectBackend(backends, body)

	start := time.Now()
	status, err := h.forward(w, r, proto, selected, forwardBody)
	if err != nil {
		// The response may already be partially written; just record the failure.
		log.Printf("%s %s failed: %v", r.Method, r.URL.Path, err)
		return
	}
	log.Printf("%s %s -> backend model=%q (%s) %d in %s",
		r.Method, r.URL.Path, selected.model, proto, status, time.Since(start).Round(time.Millisecond))
}

// protocolForPath returns the protocol of the entrypoint. Unknown paths are
// reported as OpenAI so their 404 keeps the M1 OpenAI error format.
func protocolForPath(path string) (protocol, bool) {
	switch path {
	case PathOpenAI:
		return protocolOpenAI, true
	case PathAnthropic:
		return protocolAnthropic, true
	default:
		return protocolOpenAI, false
	}
}

// authorized reports whether the request carries the configured service token:
// OpenAI entrypoints use Authorization: Bearer, Anthropic entrypoints use
// x-api-key. Comparisons are constant-time to avoid timing side channels.
func (h *Handler) authorized(proto protocol, r *http.Request) bool {
	if proto == protocolAnthropic {
		token := r.Header.Get("x-api-key")
		if token == "" {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(token), []byte(h.authToken)) == 1
	}

	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return false
	}
	token := value[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.authToken)) == 1
}

func (h *Handler) writeAuthError(w http.ResponseWriter, proto protocol, r *http.Request) {
	if proto == protocolAnthropic {
		message := "invalid service access token"
		if r.Header.Get("x-api-key") == "" {
			message = "missing x-api-key header"
		}
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", message)
		return
	}

	missing := r.Header.Get("Authorization") == ""
	code := "invalid_api_key"
	message := "invalid service access token"
	if missing {
		code = "missing_api_key"
		message = "missing Authorization header"
	}
	writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", code, message)
}

// selectBackend picks a backend by the request body's top-level model string.
// Per backend, in list order: exact model match (no rewrite), then matches
// alias (rewrite), then concrete role (rewrite). Only if no backend matches
// concretely does the first backend declaring the "*" role serve as fallback
// (rewrite). Anything unusable or unmatched falls back to the first backend
// with the body untouched.
func selectBackend(backends []*backend, body []byte) (*backend, []byte) {
	model, ok := requestModel(body)
	if !ok {
		return backends[0], body
	}
	for _, b := range backends {
		if b.model == model {
			return b, body
		}
		if contains(b.matches, model) {
			return b, rewriteModel(body, b.model)
		}
		if b.matchesRole(model) {
			return b, rewriteModel(body, b.model)
		}
	}
	for _, b := range backends {
		if b.wildcard {
			return b, rewriteModel(body, b.model)
		}
	}
	return backends[0], body
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// matchesRole reports whether model names one of this backend's concrete roles,
// either exactly ("sonnet") or as an official-style id ("claude-sonnet-*").
func (b *backend) matchesRole(model string) bool {
	for _, role := range b.roles {
		if model == role || strings.HasPrefix(model, "claude-"+role+"-") {
			return true
		}
	}
	return false
}

// rewriteModel replaces only the top-level "model" value. Other fields are
// re-serialized from their raw JSON, so their semantics are unchanged.
func rewriteModel(body []byte, model string) []byte {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	encodedModel, err := json.Marshal(model)
	if err != nil {
		return body
	}
	payload["model"] = encodedModel
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return rewritten
}

// requestModel extracts a top-level string "model" field. Missing, null and
// non-string values yield ok=false (they must not trigger the "*" fallback).
func requestModel(body []byte) (string, bool) {
	var payload struct {
		Model json.RawMessage `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false
	}
	if len(payload.Model) == 0 {
		return "", false
	}
	var value any
	if err := json.Unmarshal(payload.Model, &value); err != nil {
		return "", false
	}
	model, ok := value.(string)
	if !ok {
		return "", false
	}
	return model, true
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, proto protocol, b *backend, body []byte) (int, error) {
	target, err := url.JoinPath(b.baseURL.String(), r.URL.Path)
	if err != nil {
		writeProtocolError(w, proto, http.StatusBadGateway, "cannot build upstream URL")
		return 0, fmt.Errorf("join upstream URL: %w", err)
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeProtocolError(w, proto, http.StatusBadGateway, "cannot build upstream request")
		return 0, fmt.Errorf("build upstream request: %w", err)
	}
	outReq.ContentLength = int64(len(body))

	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	// ContentLength field is authoritative; a stale header would conflict with it.
	outReq.Header.Del("Content-Length")
	// Client credentials are only for this proxy and never reach upstream.
	outReq.Header.Del("Authorization")
	if proto == protocolAnthropic {
		outReq.Header.Del("x-api-key")
		outReq.Header.Set("x-api-key", b.apiKey)
	} else {
		outReq.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(outReq)
	if err != nil {
		writeProtocolError(w, proto, http.StatusBadGateway, "upstream request failed")
		return 0, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	removeHopByHop(w.Header())
	w.WriteHeader(resp.StatusCode)

	if err := copyBody(w, resp.Body); err != nil {
		return resp.StatusCode, fmt.Errorf("copy response body: %w", err)
	}
	return resp.StatusCode, nil
}

// copyBody streams the upstream body to the client, flushing after each chunk
// so SSE responses reach the caller without buffering.
func copyBody(w http.ResponseWriter, src io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func removeHopByHop(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				header.Del(token)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		header.Del(name)
	}
}

func writeProtocolError(w http.ResponseWriter, proto protocol, status int, message string) {
	if proto == protocolAnthropic {
		errType := "api_error"
		switch status {
		case http.StatusBadRequest:
			errType = "invalid_request_error"
		case http.StatusUnauthorized:
			errType = "authentication_error"
		case http.StatusNotFound:
			errType = "not_found_error"
		case http.StatusRequestEntityTooLarge:
			errType = "request_too_large"
		}
		writeAnthropicError(w, status, errType, message)
		return
	}

	errType, code := "server_error", "upstream_error"
	switch status {
	case http.StatusBadRequest:
		errType, code = "invalid_request_error", "invalid_request"
	case http.StatusUnauthorized:
		errType, code = "invalid_request_error", "invalid_api_key"
	case http.StatusNotFound:
		errType, code = "invalid_request_error", "not_found"
	case http.StatusRequestEntityTooLarge:
		errType, code = "invalid_request_error", "request_too_large"
	}
	writeOpenAIError(w, status, errType, code, message)
}

type openAIErrorBody struct {
	Error openAIErrorDetail `json:"error"`
}

type openAIErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func writeOpenAIError(w http.ResponseWriter, status int, errType, code, message string) {
	writeJSONError(w, status, openAIErrorBody{Error: openAIErrorDetail{
		Message: message,
		Type:    errType,
		Code:    code,
	}})
}

type anthropicErrorBody struct {
	Type  string               `json:"type"`
	Error anthropicErrorDetail `json:"error"`
}

type anthropicErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSONError(w, status, anthropicErrorBody{
		Type: "error",
		Error: anthropicErrorDetail{
			Type:    errType,
			Message: message,
		},
	})
}

func writeJSONError(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
