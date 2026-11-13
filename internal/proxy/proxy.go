// Package proxy implements the OpenAI/Anthropic passthrough proxy.
package proxy

import (
	"bytes"
	"context"
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
	"sync/atomic"
	"time"

	"model-router/internal/config"
	"model-router/internal/providers"
	"model-router/internal/web"
)

// API paths served by this proxy.
const (
	PathOpenAI    = "/v1/chat/completions"
	PathAnthropic = "/v1/messages"
	PathModels    = "/v1/models"
)

// Admin API paths added in M3.1.
const (
	PathConfigValidate = "/api/config/validate"
	PathProbeTest      = "/api/probe/test"
	PathProbeModels    = "/api/probe/models"
	PathProviders      = "/api/providers"
)

// MaxRequestBodyBytes caps how much of a request body is buffered for
// model-based routing. Larger requests are rejected with 413.
const MaxRequestBodyBytes = 10 << 20 // 10 MiB

// probeResponseLimit caps how much of an upstream model list we parse.
const probeResponseLimit = 4 << 20 // 4 MiB

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

// roleOrder is the deterministic expansion order for anthropic role maps.
// JSON objects have no preserved order, and entries of one instance share the
// same upstream, so the order only affects the published /v1/models sequence.
var roleOrder = []string{
	config.RoleSonnet,
	config.RoleOpus,
	config.RoleFable,
	config.RoleHaiku,
	config.RoleWildcard,
}

type protocol string

const (
	protocolOpenAI    protocol = config.ProtocolOpenAI
	protocolAnthropic protocol = config.ProtocolAnthropic
)

// route is one flattened model mapping from an instance.
type route struct {
	baseURL *url.URL
	apiKey  string
	model   string
	role    string // anthropic role (including "*"); empty for openai
	client  *http.Client
}

// Handler routes OpenAI and Anthropic requests to their configured instances
// without translating the protocol.
type Handler struct {
	authToken  string
	byProtocol map[protocol][]*route
}

// New builds a Handler from the configured instances.
func New(cfg *config.Config) (*Handler, error) {
	if cfg.AuthToken == "" {
		return nil, errors.New("auth_token is required")
	}
	if len(cfg.Instances) == 0 {
		return nil, errors.New("instances must contain at least one instance")
	}

	h := &Handler{
		authToken:  cfg.AuthToken,
		byProtocol: make(map[protocol][]*route),
	}
	for i, inst := range cfg.Instances {
		proto := protocol(strings.ToLower(strings.TrimSpace(inst.Protocol)))
		if proto != protocolOpenAI && proto != protocolAnthropic {
			return nil, fmt.Errorf("instances[%d]: unsupported protocol %q", i, inst.Protocol)
		}
		if strings.TrimSpace(inst.APIKey) == "" {
			return nil, fmt.Errorf("instances[%d]: api_key is required", i)
		}
		base, err := url.Parse(inst.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("instances[%d]: parse base_url: %w", i, err)
		}
		if base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
			return nil, fmt.Errorf("instances[%d]: base_url must be an absolute http(s) URL, got %q", i, inst.BaseURL)
		}
		client := newClient(inst.Timeout.Duration())

		switch proto {
		case protocolOpenAI:
			for _, model := range inst.Models {
				model = strings.TrimSpace(model)
				if model == "" {
					continue
				}
				h.byProtocol[proto] = append(h.byProtocol[proto], &route{
					baseURL: base,
					apiKey:  inst.APIKey,
					model:   model,
					client:  client,
				})
			}
		case protocolAnthropic:
			roles := make(map[string]string, len(inst.Roles))
			for rawRole, rawModel := range inst.Roles {
				role := strings.ToLower(strings.TrimSpace(rawRole))
				if !config.IsValidRole(role) {
					return nil, fmt.Errorf("instances[%d]: invalid role %q", i, rawRole)
				}
				if model := strings.TrimSpace(rawModel); model != "" {
					roles[role] = model
				}
			}
			for _, role := range roleOrder {
				model, ok := roles[role]
				if !ok {
					continue
				}
				h.byProtocol[proto] = append(h.byProtocol[proto], &route{
					baseURL: base,
					apiKey:  inst.APIKey,
					model:   model,
					role:    role,
					client:  client,
				})
			}
		}
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
	proto, known := protocolForPath(r)
	if !h.authorized(proto, r) {
		h.writeAuthError(w, proto, r)
		return
	}
	if !known {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "not_found",
			"only "+PathOpenAI+", "+PathAnthropic+" and "+PathModels+" are served")
		return
	}

	if r.URL.Path == PathModels {
		h.serveModels(w, r, proto)
		return
	}

	routes := h.byProtocol[proto]
	if len(routes) == 0 {
		writeProtocolError(w, proto, http.StatusBadGateway, "no instance configured for protocol "+string(proto))
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

	selected, forwardBody := selectRoute(routes, body)

	start := time.Now()
	status, err := h.forward(w, r, proto, selected, forwardBody)
	if err != nil {
		// The response may already be partially written; just record the failure.
		log.Printf("%s %s failed: %v", r.Method, r.URL.Path, err)
		return
	}
	log.Printf("%s %s -> model=%q (%s) %d in %s",
		r.Method, r.URL.Path, selected.model, proto, status, time.Since(start).Round(time.Millisecond))
}

// protocolForPath returns the protocol of the entrypoint. /v1/models has no
// inherent protocol, so its credential header decides; unknown paths are
// reported as OpenAI so their 404 keeps the M1 OpenAI error format.
func protocolForPath(r *http.Request) (protocol, bool) {
	switch r.URL.Path {
	case PathOpenAI:
		return protocolOpenAI, true
	case PathAnthropic:
		return protocolAnthropic, true
	case PathModels:
		return modelsProtocol(r), true
	default:
		return protocolOpenAI, false
	}
}

// modelsProtocol treats /v1/models as Anthropic only when the caller presents
// x-api-key without Authorization; otherwise it is served as OpenAI.
func modelsProtocol(r *http.Request) protocol {
	if r.Header.Get("x-api-key") != "" && r.Header.Get("Authorization") == "" {
		return protocolAnthropic
	}
	return protocolOpenAI
}

// serveModels answers /v1/models locally from the configured instances.
func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request, proto protocol) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeProtocolError(w, proto, http.StatusMethodNotAllowed, "only GET is allowed")
		return
	}

	models := h.modelList(proto)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if proto == protocolAnthropic {
		_ = json.NewEncoder(w).Encode(newAnthropicModelList(models))
		return
	}
	_ = json.NewEncoder(w).Encode(newOpenAIModelList(models))
}

// modelList aggregates declared upstream models per protocol in instance order,
// de-duplicated by first appearance. "*" role values are excluded.
func (h *Handler) modelList(proto protocol) []string {
	seen := make(map[string]bool)
	models := make([]string, 0)
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		models = append(models, name)
	}
	for _, rt := range h.byProtocol[proto] {
		if proto == protocolAnthropic && rt.role == config.RoleWildcard {
			continue
		}
		add(rt.model)
	}
	return models
}

type openAIModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type openAIModelList struct {
	Object string            `json:"object"`
	Data   []openAIModelEntry `json:"data"`
}

func newOpenAIModelList(models []string) openAIModelList {
	now := time.Now().Unix()
	data := make([]openAIModelEntry, 0, len(models))
	for _, id := range models {
		data = append(data, openAIModelEntry{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "model-router",
		})
	}
	return openAIModelList{Object: "list", Data: data}
}

type anthropicModelEntry struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

type anthropicModelList struct {
	Data    []anthropicModelEntry `json:"data"`
	HasMore bool                  `json:"has_more"`
	FirstID string                `json:"first_id"`
	LastID  string                `json:"last_id"`
}

func newAnthropicModelList(models []string) anthropicModelList {
	createdAt := time.Now().UTC().Format(time.RFC3339)
	data := make([]anthropicModelEntry, 0, len(models))
	for _, id := range models {
		data = append(data, anthropicModelEntry{
			ID:          id,
			Type:        "model",
			DisplayName: id,
			CreatedAt:   createdAt,
		})
	}
	list := anthropicModelList{Data: data, HasMore: false}
	if len(models) > 0 {
		list.FirstID = models[0]
		list.LastID = models[len(models)-1]
	}
	return list
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

// selectRoute picks a flattened route by the request body's top-level model:
// exact model match wins without rewriting; a concrete role match (role name or
// claude-<role>- prefix) rewrites; then the first "*" route rewrites; anything
// unusable or unmatched falls back to the first route untouched.
func selectRoute(routes []*route, body []byte) (*route, []byte) {
	model, ok := requestModel(body)
	if !ok {
		return routes[0], body
	}
	for _, rt := range routes {
		if rt.model == model {
			return rt, body
		}
		if rt.role != "" && rt.role != config.RoleWildcard && matchesRole(model, rt.role) {
			return rt, rewriteModel(body, rt.model)
		}
	}
	for _, rt := range routes {
		if rt.role == config.RoleWildcard {
			return rt, rewriteModel(body, rt.model)
		}
	}
	return routes[0], body
}

// matchesRole reports whether model names role exactly ("sonnet") or as an
// official-style id ("claude-sonnet-*").
func matchesRole(model, role string) bool {
	return model == role || strings.HasPrefix(model, "claude-"+role+"-")
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

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, proto protocol, rt *route, body []byte) (int, error) {
	target, err := url.JoinPath(rt.baseURL.String(), r.URL.Path)
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
		outReq.Header.Set("x-api-key", rt.apiKey)
	} else {
		outReq.Header.Set("Authorization", "Bearer "+rt.apiKey)
	}

	resp, err := rt.client.Do(outReq)
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
		case http.StatusMethodNotAllowed:
			errType = "invalid_request_error"
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
	case http.StatusMethodNotAllowed:
		errType, code = "invalid_request_error", "method_not_allowed"
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

// Server routes model traffic plus the admin surface and owns the atomically
// swappable runtime state. A reload replaces the whole state without restarting
// the process; in-flight requests keep using the handler they loaded.
type Server struct {
	configPath string
	state      atomic.Pointer[serverState]
}

type serverState struct {
	cfg     *config.Config
	handler *Handler
}

// NewServer builds a Server for cfg, which must have been loaded from
// configPath so that reload reads the same file.
func NewServer(configPath string, cfg *config.Config) (*Server, error) {
	st, err := buildState(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{configPath: configPath}
	s.state.Store(st)
	return s, nil
}

func buildState(cfg *config.Config) (*serverState, error) {
	handler, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return &serverState{cfg: cfg, handler: handler}, nil
}

func (s *Server) reload() error {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		return err
	}
	st, err := buildState(cfg)
	if err != nil {
		return err
	}
	s.state.Store(st)
	return nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	if isAdminPath(r.URL.Path) {
		if st.cfg.AdminPassword == "" {
			writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "not_found", "admin surface is disabled")
			return
		}
		if !adminAuthorized(st.cfg, r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="model-router"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.serveAdmin(w, r, st)
		return
	}
	st.handler.ServeHTTP(w, r)
}

func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/") ||
		path == "/api" || strings.HasPrefix(path, "/api/")
}

func adminAuthorized(cfg *config.Config, r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(cfg.AdminUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.AdminPassword)) == 1
	return userOK && passOK
}

func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request, st *serverState) {
	switch r.URL.Path {
	case "/admin", "/admin/":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(web.IndexHTML)
	case "/api/config":
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(st.cfg)
		case http.MethodPut:
			s.putConfig(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case PathConfigValidate:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.validateConfig(w, r)
	case PathProviders:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSONError(w, http.StatusOK, providers.List())
	case "/api/reload":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := s.reload(); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAdminOK(w)
	case PathProbeTest, PathProbeModels:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.serveProbe(w, r, r.URL.Path == PathProbeModels)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "cannot read request body")
		return
	}
	cfg, err := config.Parse(data)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.Save(s.configPath, cfg); err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAdminOK(w)
}

type validateResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) validateConfig(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, validateResponse{OK: false, Error: "cannot read request body"})
		return
	}
	if _, err := config.Parse(data); err != nil {
		writeJSONError(w, http.StatusBadRequest, validateResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSONError(w, http.StatusOK, validateResponse{OK: true})
}

type probeRequest struct {
	Protocol string          `json:"protocol"`
	BaseURL  string          `json:"base_url"`
	APIKey   string          `json:"api_key"`
	Provider string          `json:"provider"`
	Timeout  config.Duration `json:"timeout"`
}

type probeTestResponse struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Message   string `json:"message"`
}

type probeModelsResponse struct {
	Models []string `json:"models"`
}

// anthropicProbeBody is the minimal messages request used to probe Anthropic
// upstreams (some, e.g. DeepSeek, do not implement GET /v1/models).
const anthropicProbeBody = `{"model":"ping","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`

func (s *Server) serveProbe(w http.ResponseWriter, r *http.Request, wantModels bool) {
	req, err := decodeProbeRequest(r)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !wantModels {
		start := time.Now()
		status, err := req.probeConnect(r.Context())
		result := probeTestResponse{LatencyMs: time.Since(start).Milliseconds()}
		switch {
		case err != nil:
			result.OK = false
			result.Message = err.Error()
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			result.OK = false
			result.Message = "api_key 无效（认证失败）"
		case status == http.StatusNotFound:
			result.OK = false
			result.Message = "base_url 路径无效"
		case status >= 200 && status < 300:
			result.OK = true
			result.Message = "upstream reachable"
		default:
			// 400/422/429 etc. mean the endpoint exists and auth passed; the
			// placeholder probe model being rejected is expected.
			result.OK = true
			result.Message = "上游可达（探测 model 为占位名，非 2xx 属预期）"
		}
		writeJSONError(w, http.StatusOK, result)
		return
	}

	if req.Provider != "" {
		if _, ok := providers.Get(req.Provider); !ok {
			writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("unknown provider %q", req.Provider))
			return
		}
	}

	status, body, err := req.fetchModels(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "probe failed: "+err.Error())
		return
	}
	if status == http.StatusNotFound {
		writeAdminError(w, http.StatusBadGateway, "该上游不支持模型列表查询（未实现 /v1/models），请手动输入模型名")
		return
	}
	if status < 200 || status >= 300 {
		writeAdminError(w, http.StatusBadGateway, fmt.Sprintf("upstream returned HTTP %d", status))
		return
	}
	models, err := parseModelIDs(body)
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSONError(w, http.StatusOK, probeModelsResponse{Models: models})
}

func decodeProbeRequest(r *http.Request) (*probeRequest, error) {
	var req probeRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	if err := dec.Decode(new(struct{})); err != io.EOF {
		return nil, errors.New("invalid request body: unexpected trailing data")
	}

	req.Protocol = strings.ToLower(strings.TrimSpace(req.Protocol))
	if req.Protocol != config.ProtocolOpenAI && req.Protocol != config.ProtocolAnthropic {
		return nil, fmt.Errorf("protocol must be %q or %q", config.ProtocolOpenAI, config.ProtocolAnthropic)
	}
	req.Provider = strings.ToLower(strings.TrimSpace(req.Provider))
	req.APIKey = strings.TrimSpace(req.APIKey)
	if req.APIKey == "" {
		return nil, errors.New("api_key is required")
	}
	req.BaseURL = strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if req.BaseURL == "" {
		return nil, errors.New("base_url is required")
	}
	u, err := url.Parse(req.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("base_url must be an absolute http(s) URL, got %q", req.BaseURL)
	}
	return &req, nil
}

// probeConnect probes the upstream with the protocol-appropriate endpoint:
// OpenAI uses GET /v1/models, Anthropic uses a minimal POST /v1/messages.
func (req *probeRequest) probeConnect(ctx context.Context) (int, error) {
	if req.Protocol == config.ProtocolAnthropic {
		return req.probeAnthropic(ctx)
	}
	status, _, err := req.fetchModels(ctx)
	return status, err
}

// probeAnthropic sends a minimal messages request so upstreams without a
// /v1/models endpoint can still be reachability-tested.
func (req *probeRequest) probeAnthropic(ctx context.Context) (int, error) {
	target, err := url.JoinPath(req.BaseURL, PathAnthropic)
	if err != nil {
		return 0, fmt.Errorf("build probe URL: %w", err)
	}

	timeout := req.Timeout.Duration()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(anthropicProbeBody))
	if err != nil {
		return 0, fmt.Errorf("build probe request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", req.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := newClient(timeout).Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("probe request failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeResponseLimit))
	return resp.StatusCode, nil
}

// fetchModels probes {base_url}/v1/models and returns the status code and raw
// body. A provider models_override can redirect the base URL and auth scheme.
func (req *probeRequest) fetchModels(ctx context.Context) (int, []byte, error) {
	auth := providers.AuthBearer
	if req.Protocol == config.ProtocolAnthropic {
		auth = providers.AuthXAPIKey
	}
	baseURL, auth := providers.ModelsSource(req.Provider, req.Protocol, req.BaseURL, auth)

	target, err := url.JoinPath(baseURL, PathModels)
	if err != nil {
		return 0, nil, fmt.Errorf("build probe URL: %w", err)
	}

	timeout := req.Timeout.Duration()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("build probe request: %w", err)
	}
	if auth == providers.AuthXAPIKey {
		httpReq.Header.Set("x-api-key", req.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}

	resp, err := newClient(timeout).Do(httpReq)
	if err != nil {
		return 0, nil, fmt.Errorf("probe request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, probeResponseLimit))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read probe response: %w", err)
	}
	return resp.StatusCode, body, nil
}

func parseModelIDs(body []byte) ([]string, error) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errors.New("upstream response is not a model list; please enter models manually")
	}
	if payload.Data == nil {
		return nil, errors.New("upstream response has no model data; please enter models manually")
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if id := strings.TrimSpace(item.ID); id != "" {
			models = append(models, id)
		}
	}
	return models, nil
}

type adminMessage struct {
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

func writeAdminOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(adminMessage{Status: "ok"})
}

func writeAdminError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(adminMessage{Error: message})
}
