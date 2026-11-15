// Package proxy implements the OpenAI/Anthropic passthrough proxy.
package proxy

import (
	"bytes"
	"compress/gzip"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"model-router/internal/config"
	"model-router/internal/providers"
	"model-router/internal/web"
)

// API paths served by this proxy. PathAnthropic and PathModels are also the
// upstream paths used by probe requests.
const (
	PathOpenAI    = "/v1/chat/completions"
	PathAnthropic = "/v1/messages"
	PathModels    = "/v1/models"

	// Protocol-separated Anthropic entrypoints (§22.3). /anthropic/v1/messages
	// behaves exactly like /v1/messages; /anthropic/v1/models always answers in
	// the Anthropic schema.
	PathAnthropicMessages = "/anthropic/v1/messages"
	PathAnthropicModels   = "/anthropic/v1/models"
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

// concreteRoles is the canonical order for concrete anthropic roles.
var concreteRoles = []string{
	config.RoleSonnet,
	config.RoleOpus,
	config.RoleFable,
	config.RoleHaiku,
}

type protocol string

const (
	protocolOpenAI    protocol = config.ProtocolOpenAI
	protocolAnthropic protocol = config.ProtocolAnthropic
)

// instanceRuntime is one configured instance plus its in-memory fallback state.
type instanceRuntime struct {
	name         string
	baseURL      *url.URL
	apiKey       string
	models       []string
	roles        map[string]string
	roleNames    map[string]string
	capabilities map[string]config.ModelCapability
	client       *http.Client

	mu                  sync.Mutex
	consecutiveFailures int
	degradedUntil       time.Time
}

// Fallback defaults (kept for compatibility; the effective values come from
// config.Fallback, §12.2).
const (
	DefaultFailureThreshold   = config.DefaultFailureThreshold
	DefaultDegradeDuration    = config.DefaultDegradeDuration
	DefaultMinTokensPerSecond = config.DefaultMinTokensPerSecond
)

// Options tunes the fallback state machine. Zero values select defaults.
type Options struct {
	Now                func() time.Time
	FailureThreshold   int
	DegradeDuration    time.Duration
	MinTokensPerSecond float64
	IdleTimeout        time.Duration
	FirstTokenTimeout  time.Duration
}

// Handler routes OpenAI and Anthropic requests to their configured instances
// without translating the protocol.
type Handler struct {
	authToken          string
	byProtocol         map[protocol][]*instanceRuntime
	now                func() time.Time
	failureThreshold   int
	degradeDuration    time.Duration
	minTokensPerSecond float64
	idleTimeout        time.Duration
	firstTokenTimeout  time.Duration
}

// New builds a Handler with the default fallback parameters.
func New(cfg *config.Config) (*Handler, error) {
	return NewWithOptions(cfg, Options{})
}

// NewWithOptions builds a Handler from the configured instances.
func NewWithOptions(cfg *config.Config, opts Options) (*Handler, error) {
	if cfg.AuthToken == "" {
		return nil, errors.New("auth_token is required")
	}
	if len(cfg.Instances) == 0 {
		return nil, errors.New("instances must contain at least one instance")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	fallback := cfg.Fallback.Values()
	threshold := fallback.FailureThreshold
	degradeDuration := fallback.DegradeDuration
	minTokensPerSecond := fallback.MinTokensPerSecond
	idleTimeout := fallback.IdleTimeout
	firstTokenTimeout := fallback.FirstTokenTimeout
	if opts.FailureThreshold > 0 {
		threshold = opts.FailureThreshold
	}
	if opts.DegradeDuration > 0 {
		degradeDuration = opts.DegradeDuration
	}
	if opts.MinTokensPerSecond != 0 {
		minTokensPerSecond = opts.MinTokensPerSecond
	}
	if opts.IdleTimeout > 0 {
		idleTimeout = opts.IdleTimeout
	}
	if opts.FirstTokenTimeout != 0 {
		firstTokenTimeout = opts.FirstTokenTimeout
	}

	h := &Handler{
		authToken:          cfg.AuthToken,
		byProtocol:         make(map[protocol][]*instanceRuntime),
		now:                opts.Now,
		failureThreshold:   threshold,
		degradeDuration:    degradeDuration,
		minTokensPerSecond: minTokensPerSecond,
		idleTimeout:        idleTimeout,
		firstTokenTimeout:  firstTokenTimeout,
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

		name := strings.TrimSpace(inst.Name)
		if name == "" {
			name = inst.BaseURL
		}
		ir := &instanceRuntime{
			name:    name,
			baseURL: base,
			apiKey:  inst.APIKey,
			client:  newClient(inst.Timeout.Duration()),
		}
		switch proto {
		case protocolOpenAI:
			for _, model := range inst.Models {
				if model = strings.TrimSpace(model); model != "" {
					ir.models = append(ir.models, model)
				}
			}
		case protocolAnthropic:
			ir.roles = make(map[string]string, len(inst.Roles))
			for rawRole, rawModel := range inst.Roles {
				role := strings.ToLower(strings.TrimSpace(rawRole))
				if !config.IsValidRole(role) {
					return nil, fmt.Errorf("instances[%d]: invalid role %q", i, rawRole)
				}
				if model := strings.TrimSpace(rawModel); model != "" {
					ir.roles[role] = model
				}
			}
			ir.roleNames = make(map[string]string, len(inst.RoleNames))
			for rawRole, rawName := range inst.RoleNames {
				role := strings.ToLower(strings.TrimSpace(rawRole))
				if !config.IsConcreteRole(role) {
					continue
				}
				if name := strings.TrimSpace(rawName); name != "" {
					ir.roleNames[role] = name
				}
			}
		}
		// Capabilities feed the models listings of both protocols (§21.5, §22.4).
		ir.capabilities = make(map[string]config.ModelCapability, len(inst.ModelCapabilities))
		for rawModel, capability := range inst.ModelCapabilities {
			if model := strings.TrimSpace(rawModel); model != "" {
				ir.capabilities[model] = capability
			}
		}
		h.byProtocol[proto] = append(h.byProtocol[proto], ir)
	}
	return h, nil
}

func (inst *instanceRuntime) isDegraded(now time.Time) bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return now.Before(inst.degradedUntil)
}

func (inst *instanceRuntime) recordSuccess() {
	inst.mu.Lock()
	inst.consecutiveFailures = 0
	inst.degradedUntil = time.Time{}
	inst.mu.Unlock()
}

func (inst *instanceRuntime) recordFailure(now time.Time, threshold int, duration time.Duration) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.consecutiveFailures++
	if !inst.degradedUntil.IsZero() && !now.Before(inst.degradedUntil) {
		// Recovery probe after the degradation window failed: re-time without
		// waiting for the failure count again.
		inst.degradedUntil = now.Add(duration)
		return
	}
	if inst.consecutiveFailures >= threshold {
		inst.degradedUntil = now.Add(duration)
	}
}

func (h *Handler) recordFailure(inst *instanceRuntime) {
	inst.recordFailure(h.now(), h.failureThreshold, h.degradeDuration)
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
	start := time.Now()
	proto, known := protocolForPath(r)
	if !h.authorized(proto, r) {
		h.writeAuthError(w, proto, r)
		// §17.6: rejected calls are logged too, without any credential values.
		logRequest(r, attemptMetrics{}, time.Since(start))
		return
	}
	if !known {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "not_found",
			"only "+PathOpenAI+", "+PathAnthropic+", "+PathAnthropicMessages+", "+
				PathModels+" and "+PathAnthropicModels+" are served")
		return
	}

	if isModelsPath(r.URL.Path) {
		h.serveModels(w, r, proto)
		return
	}

	insts := h.byProtocol[proto]
	if len(insts) == 0 {
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

	h.serveChain(w, r, proto, insts, body)
}

// protocolForPath returns the protocol of the entrypoint. The path alone
// decides the protocol (§22.3); unknown paths are reported as OpenAI so their
// 404 keeps the M1 OpenAI error format.
func protocolForPath(r *http.Request) (protocol, bool) {
	switch r.URL.Path {
	case PathOpenAI, PathModels:
		return protocolOpenAI, true
	case PathAnthropic, PathAnthropicMessages, PathAnthropicModels:
		return protocolAnthropic, true
	default:
		return protocolOpenAI, false
	}
}

func isModelsPath(path string) bool {
	return path == PathModels || path == PathAnthropicModels
}

// upstreamPath maps a public entry path to the upstream path. The /anthropic
// prefix exists only locally; upstreams still receive /v1/messages (§22.3).
func upstreamPath(entry string) string {
	if entry == PathAnthropicMessages {
		return PathAnthropic
	}
	return entry
}

// serveModels answers the two models entrypoints locally from the configured
// instances. The path, not the credential header, selects the response schema.
func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request, proto protocol) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeProtocolError(w, proto, http.StatusMethodNotAllowed, "only GET is allowed")
		return
	}

	if r.URL.Path == PathAnthropicModels {
		list, err := h.anthropicModelList(r)
		if err != nil {
			writeProtocolError(w, proto, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(list)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(h.openAIModelList())
}

// openAIModelList aggregates declared upstream models in instance order,
// de-duplicated by first appearance, with the first declared capability of
// each model (§22.4).
func (h *Handler) openAIModelList() openAIModelList {
	instances := h.byProtocol[protocolOpenAI]
	seen := make(map[string]bool)
	now := time.Now().Unix()
	data := make([]openAIModelEntry, 0)
	for _, inst := range instances {
		for _, model := range inst.models {
			if model == "" || seen[model] {
				continue
			}
			seen[model] = true
			entry := openAIModelEntry{
				ID:      model,
				Object:  "model",
				Created: now,
				OwnedBy: "model-router",
			}
			if capability, ok := openAIModelCapability(instances, model); ok {
				applyOpenAIModelCapability(&entry, capability)
			}
			data = append(data, entry)
		}
	}
	return openAIModelList{Object: "list", Data: data}
}

// openAIModelCapability returns the first declaration of the upstream model in
// instance order; a later instance supplies it when the first one is silent
// (§22.4, mirroring §21.5).
func openAIModelCapability(instances []*instanceRuntime, model string) (config.ModelCapability, bool) {
	for _, inst := range instances {
		if capability, ok := inst.capabilities[model]; ok {
			return capability, true
		}
	}
	return config.ModelCapability{}, false
}

func applyOpenAIModelCapability(entry *openAIModelEntry, capability config.ModelCapability) {
	if capability.MaxInputTokens > 0 {
		value := capability.MaxInputTokens
		entry.ContextWindow = &value
		entry.MaxInputTokens = &value
	}
	if capability.MaxTokens > 0 {
		value := capability.MaxTokens
		entry.MaxOutputTokens = &value
		entry.MaxTokens = &value
	}

	modalities := []string{"text"}
	if capability.ImageInput != nil && *capability.ImageInput {
		modalities = append(modalities, "image")
	}
	if capability.PDFInput != nil && *capability.PDFInput {
		modalities = append(modalities, "pdf")
	}
	if len(modalities) > 1 {
		entry.InputModalities = modalities
	}
}

type openAIModelEntry struct {
	ID              string   `json:"id"`
	Object          string   `json:"object"`
	Created         int64    `json:"created"`
	OwnedBy         string   `json:"owned_by"`
	ContextWindow   *int     `json:"context_window,omitempty"`
	MaxInputTokens  *int     `json:"max_input_tokens,omitempty"`
	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`
	MaxTokens       *int     `json:"max_tokens,omitempty"`
	InputModalities []string `json:"input_modalities,omitempty"`
}

type openAIModelList struct {
	Object string             `json:"object"`
	Data   []openAIModelEntry `json:"data"`
}

// Lifecycle values accepted by ?lifecycle (§21.3). This proxy only publishes
// active models.
const (
	lifecycleActive     = "active"
	lifecycleDeprecated = "deprecated"
	lifecycleRetired    = "retired"

	defaultModelsLimit = 20
	maxModelsLimit     = 1000
)

// roleNameDefault is the built-in public identity of a concrete role (§21.4).
type roleNameDefault struct {
	ID          string
	DisplayName string
	CreatedAt   string
}

var builtinRoleNames = map[string]roleNameDefault{
	config.RoleSonnet: {ID: "claude-sonnet-5", DisplayName: "Claude Sonnet 5", CreatedAt: "2026-01-01T00:00:00Z"},
	config.RoleOpus:   {ID: "claude-opus-5", DisplayName: "Claude Opus 5", CreatedAt: "2026-01-01T00:00:00Z"},
	config.RoleFable:  {ID: "claude-fable-5", DisplayName: "Claude Fable 5", CreatedAt: "2026-01-01T00:00:00Z"},
	config.RoleHaiku:  {ID: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5", CreatedAt: "2025-10-01T00:00:00Z"},
}

var builtinRoleNamesByID = func() map[string]roleNameDefault {
	byID := make(map[string]roleNameDefault, len(builtinRoleNames))
	for _, name := range builtinRoleNames {
		byID[name.ID] = name
	}
	return byID
}()

type anthropicCapabilitySupport struct {
	Supported bool `json:"supported"`
}

type anthropicCapabilities struct {
	ImageInput        *anthropicCapabilitySupport `json:"image_input,omitempty"`
	PDFInput          *anthropicCapabilitySupport `json:"pdf_input,omitempty"`
	Thinking          *anthropicCapabilitySupport `json:"thinking,omitempty"`
	StructuredOutputs *anthropicCapabilitySupport `json:"structured_outputs,omitempty"`
}

type anthropicModelEntry struct {
	Type           string                 `json:"type"`
	ID             string                 `json:"id"`
	DisplayName    string                 `json:"display_name"`
	CreatedAt      string                 `json:"created_at"`
	MaxInputTokens *int                   `json:"max_input_tokens"`
	MaxTokens      *int                   `json:"max_tokens"`
	Lifecycle      string                 `json:"lifecycle"`
	DeprecatedAt   *string                `json:"deprecated_at"`
	RetiresAt      *string                `json:"retires_at"`
	Line           string                 `json:"line"`
	Capabilities   *anthropicCapabilities `json:"capabilities"`
}

type anthropicModelList struct {
	Data    []anthropicModelEntry `json:"data"`
	HasMore bool                  `json:"has_more"`
	FirstID *string               `json:"first_id"`
	LastID  *string               `json:"last_id"`
}

type anthropicModelsQuery struct {
	limit      int
	afterID    string
	beforeID   string
	lifecycles map[string]bool
}

func (h *Handler) anthropicModelList(r *http.Request) (anthropicModelList, error) {
	query, err := parseAnthropicModelsQuery(r)
	if err != nil {
		return anthropicModelList{}, err
	}
	entries := filterAnthropicModels(h.anthropicModelEntries(), query.lifecycles)
	return paginateAnthropicModels(entries, query), nil
}

// anthropicModelEntries lists one entry per concrete role declared by at least
// one anthropic instance, in canonical role order.
func (h *Handler) anthropicModelEntries() []anthropicModelEntry {
	instances := h.byProtocol[protocolAnthropic]
	now := h.now().UTC().Format(time.RFC3339)
	entries := make([]anthropicModelEntry, 0, len(concreteRoles))
	for _, role := range concreteRoles {
		upstreamModel, ok := roleUpstreamModel(instances, role)
		if !ok {
			continue
		}
		id := publicRoleModelID(instances, role)
		displayName, createdAt := roleModelIdentity(id, now)
		entry := anthropicModelEntry{
			Type:        "model",
			ID:          id,
			DisplayName: displayName,
			CreatedAt:   createdAt,
			Lifecycle:   lifecycleActive,
			Line:        role,
		}
		if capability, ok := roleModelCapability(instances, upstreamModel); ok {
			applyModelCapability(&entry, capability)
		}
		entries = append(entries, entry)
	}
	return entries
}

// roleUpstreamModel returns the upstream model of the first anthropic instance
// that maps role, matching the routing order.
func roleUpstreamModel(instances []*instanceRuntime, role string) (string, bool) {
	for _, inst := range instances {
		if model, ok := inst.roles[role]; ok {
			return model, true
		}
	}
	return "", false
}

// publicRoleModelID is the first role_names override in instance order, or the
// built-in official model name (§21.4).
func publicRoleModelID(instances []*instanceRuntime, role string) string {
	for _, inst := range instances {
		if id := inst.roleNames[role]; id != "" {
			return id
		}
	}
	if builtin, ok := builtinRoleNames[role]; ok {
		return builtin.ID
	}
	return role
}

// roleModelIdentity follows the built-in table by public id; ids outside the
// table fall back to id/current time (§21.4).
func roleModelIdentity(id, now string) (displayName, createdAt string) {
	if builtin, ok := builtinRoleNamesByID[id]; ok {
		return builtin.DisplayName, builtin.CreatedAt
	}
	return id, now
}

// roleModelCapability returns the first declaration of the upstream model in
// anthropic instance order; absence means unknown (§21.5).
func roleModelCapability(instances []*instanceRuntime, upstreamModel string) (config.ModelCapability, bool) {
	for _, inst := range instances {
		if capability, ok := inst.capabilities[upstreamModel]; ok {
			return capability, true
		}
	}
	return config.ModelCapability{}, false
}

func applyModelCapability(entry *anthropicModelEntry, capability config.ModelCapability) {
	if capability.MaxInputTokens > 0 {
		value := capability.MaxInputTokens
		entry.MaxInputTokens = &value
	}
	if capability.MaxTokens > 0 {
		value := capability.MaxTokens
		entry.MaxTokens = &value
	}

	declared := false
	capabilities := &anthropicCapabilities{}
	if capability.ImageInput != nil {
		capabilities.ImageInput = &anthropicCapabilitySupport{Supported: *capability.ImageInput}
		declared = true
	}
	if capability.PDFInput != nil {
		capabilities.PDFInput = &anthropicCapabilitySupport{Supported: *capability.PDFInput}
		declared = true
	}
	if capability.Thinking != nil {
		capabilities.Thinking = &anthropicCapabilitySupport{Supported: *capability.Thinking}
		declared = true
	}
	if capability.StructuredOutputs != nil {
		capabilities.StructuredOutputs = &anthropicCapabilitySupport{Supported: *capability.StructuredOutputs}
		declared = true
	}
	if declared {
		entry.Capabilities = capabilities
	}
}

func parseAnthropicModelsQuery(r *http.Request) (anthropicModelsQuery, error) {
	values := r.URL.Query()
	query := anthropicModelsQuery{
		limit:      defaultModelsLimit,
		lifecycles: map[string]bool{lifecycleActive: true, lifecycleDeprecated: true},
	}

	if limits, ok := values["limit"]; ok {
		raw := ""
		if len(limits) > 0 {
			raw = limits[0]
		}
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxModelsLimit {
			return anthropicModelsQuery{}, fmt.Errorf("limit must be an integer between 1 and %d", maxModelsLimit)
		}
		query.limit = limit
	}

	before, hasBefore := values["before_id"]
	after, hasAfter := values["after_id"]
	if hasBefore && hasAfter {
		return anthropicModelsQuery{}, errors.New("before_id and after_id are mutually exclusive")
	}
	if hasBefore && len(before) > 0 {
		query.beforeID = before[0]
	}
	if hasAfter && len(after) > 0 {
		query.afterID = after[0]
	}

	if raw, ok := values["lifecycle"]; ok {
		lifecycles := make([]string, 0, len(raw))
		for _, item := range raw {
			for _, value := range strings.Split(item, ",") {
				lifecycles = append(lifecycles, strings.TrimSpace(value))
			}
		}
		if len(lifecycles) > 3 {
			return anthropicModelsQuery{}, errors.New("lifecycle accepts at most 3 values")
		}
		query.lifecycles = make(map[string]bool, len(lifecycles))
		for _, value := range lifecycles {
			switch value {
			case lifecycleActive, lifecycleDeprecated, lifecycleRetired:
				query.lifecycles[value] = true
			default:
				return anthropicModelsQuery{}, fmt.Errorf("invalid lifecycle %q: must be one of active, deprecated, retired", value)
			}
		}
	}

	return query, nil
}

func filterAnthropicModels(entries []anthropicModelEntry, lifecycles map[string]bool) []anthropicModelEntry {
	filtered := make([]anthropicModelEntry, 0, len(entries))
	for _, entry := range entries {
		if lifecycles[entry.Lifecycle] {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func paginateAnthropicModels(entries []anthropicModelEntry, query anthropicModelsQuery) anthropicModelList {
	start, end := 0, len(entries)
	hasMore := false
	switch {
	case query.afterID != "":
		index := anthropicModelIndex(entries, query.afterID)
		if index < 0 {
			return newAnthropicModelPage(nil, false)
		}
		start = index + 1
		end = min(start+query.limit, len(entries))
		hasMore = end < len(entries)
	case query.beforeID != "":
		index := anthropicModelIndex(entries, query.beforeID)
		if index < 0 {
			return newAnthropicModelPage(nil, false)
		}
		end = index
		start = max(0, end-query.limit)
		hasMore = start > 0
	default:
		end = min(query.limit, len(entries))
		hasMore = end < len(entries)
	}
	return newAnthropicModelPage(entries[start:end], hasMore)
}

func anthropicModelIndex(entries []anthropicModelEntry, id string) int {
	for i, entry := range entries {
		if entry.ID == id {
			return i
		}
	}
	return -1
}

func newAnthropicModelPage(entries []anthropicModelEntry, hasMore bool) anthropicModelList {
	data := make([]anthropicModelEntry, len(entries))
	copy(data, entries)
	list := anthropicModelList{Data: data, HasMore: hasMore}
	if len(data) > 0 {
		firstID, lastID := data[0].ID, data[len(data)-1].ID
		list.FirstID = &firstID
		list.LastID = &lastID
	}
	return list
}

const bearerPrefix = "Bearer "

// hasBearerPrefix reports whether value starts with "Bearer " case-insensitively.
func hasBearerPrefix(value string) bool {
	return len(value) >= len(bearerPrefix) && strings.EqualFold(value[:len(bearerPrefix)], bearerPrefix)
}

// bearerToken extracts a non-empty Bearer credential from an Authorization value.
func bearerToken(value string) (string, bool) {
	if !hasBearerPrefix(value) || len(value) == len(bearerPrefix) {
		return "", false
	}
	return value[len(bearerPrefix):], true
}

// tokenMatches compares a presented token to the service token in constant time.
func (h *Handler) tokenMatches(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(h.authToken)) == 1
}

// authorized reports whether the request carries the configured service token:
// Anthropic entrypoints and both models entrypoints accept x-api-key first and
// fall back to Authorization: Bearer (§17.2, §22.3); other OpenAI entrypoints
// use Bearer only. Comparisons are constant-time to avoid timing side channels.
func (h *Handler) authorized(proto protocol, r *http.Request) bool {
	if (proto == protocolAnthropic || isModelsPath(r.URL.Path)) && h.tokenMatches(r.Header.Get("x-api-key")) {
		return true
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	return ok && h.tokenMatches(token)
}

func (h *Handler) writeAuthError(w http.ResponseWriter, proto protocol, r *http.Request) {
	if proto == protocolAnthropic {
		message := "invalid service access token"
		if r.Header.Get("x-api-key") == "" && !hasBearerPrefix(r.Header.Get("Authorization")) {
			message = "missing x-api-key or Authorization header"
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

type matchKind int

const (
	matchExact matchKind = iota
	matchRole
	matchWildcard
	matchFallback
)

// candidate is one instance that can serve the request, plus the upstream model
// to use and whether the request body's "model" must be rewritten.
type candidate struct {
	inst    *instanceRuntime
	model   string
	rewrite bool
}

// serveChain runs the fallback state machine: skip degraded instances, try
// candidates in order, retry on pre-response failures, and record outcomes.
func (h *Handler) serveChain(w http.ResponseWriter, r *http.Request, proto protocol, insts []*instanceRuntime, body []byte) {
	start := time.Now()
	reqModel, _ := requestModel(body)
	chain := buildChain(proto, insts, body)
	now := h.now()

	attempted := false
	// last is the candidate that failed before any response was written. Its
	// model information goes into the final log line so that a failed routed
	// call still shows which instance/model was tried (§23 only drops the
	// segment for traffic that never reached a route).
	var last *candidate
	for _, cand := range chain {
		if cand.inst.isDegraded(now) {
			continue
		}
		attempted = true
		committed, metrics, err := h.tryCandidate(w, r, proto, cand, body)
		if committed {
			if err != nil {
				log.Printf("%s %s response committed with error: %v", r.Method, r.URL.Path, err)
			}
			metrics.reqModel = reqModel
			logRequest(r, metrics, time.Since(start))
			return
		}
		log.Printf("%s %s candidate %s failed: %v", r.Method, r.URL.Path, cand.inst.name, err)
		last = cand
	}

	if !attempted && len(chain) > 0 {
		// All candidates are in sustained degradation: force the chain head once.
		cand := chain[0]
		committed, metrics, err := h.tryCandidate(w, r, proto, cand, body)
		if committed {
			if err != nil {
				log.Printf("%s %s forced response committed with error: %v", r.Method, r.URL.Path, err)
			}
			metrics.reqModel = reqModel
			logRequest(r, metrics, time.Since(start))
			return
		}
		log.Printf("%s %s forced candidate %s failed: %v", r.Method, r.URL.Path, cand.inst.name, err)
		last = cand
	}

	if last != nil {
		logRequest(r, attemptMetrics{
			instName: last.inst.name,
			model:    last.model,
			reqModel: reqModel,
			hasModel: true,
		}, time.Since(start))
	}
	log.Printf("%s %s -> (%s) total=%ss status=502 all candidates failed",
		r.Method, r.URL.Path, proto, formatSeconds(time.Since(start)))
	writeProtocolError(w, proto, http.StatusBadGateway, "upstream request failed")
}

// buildChain determines the base instance per §9.2 and collects the fallback
// candidates from it, each with its upstream model and rewrite flag (§11.2).
func buildChain(proto protocol, insts []*instanceRuntime, body []byte) []*candidate {
	model, usable := requestModel(body)
	if !usable {
		return fallbackChain(proto, insts)
	}
	base, kind := findBase(proto, insts, model)
	from := insts[base:]
	switch kind {
	case matchExact:
		return exactChain(proto, from, model)
	case matchRole:
		role, _ := requestRole(model)
		return roleChain(from, role)
	case matchWildcard:
		return wildcardChain(from)
	default:
		return fallbackChain(proto, insts)
	}
}

// findBase returns the first instance that can handle the request (R4a → R4c →
// R4d), mirroring the M3 routing rules.
func findBase(proto protocol, insts []*instanceRuntime, model string) (int, matchKind) {
	role, hasRole := "", false
	if proto == protocolAnthropic {
		role, hasRole = requestRole(model)
	}
	for i, inst := range insts {
		if inst.hasModel(model) {
			return i, matchExact
		}
		if hasRole && inst.hasRole(role) {
			return i, matchRole
		}
	}
	if proto == protocolAnthropic {
		for i, inst := range insts {
			if inst.hasWildcard() {
				return i, matchWildcard
			}
		}
	}
	return 0, matchFallback
}

// exactChain: the request model matched, so instances that declare it keep the
// body untouched; the rest rewrite to their first model (openai) or wildcard
// (anthropic), or are skipped when they cannot map the request.
func exactChain(proto protocol, insts []*instanceRuntime, model string) []*candidate {
	out := make([]*candidate, 0, len(insts))
	for _, inst := range insts {
		if proto == protocolAnthropic {
			if inst.hasModel(model) {
				out = append(out, &candidate{inst: inst, model: model})
			} else if wildcard, ok := inst.roles[config.RoleWildcard]; ok {
				out = append(out, &candidate{inst: inst, model: wildcard, rewrite: true})
			}
			continue
		}
		if len(inst.models) == 0 {
			continue
		}
		if contains(inst.models, model) {
			out = append(out, &candidate{inst: inst, model: model})
		} else {
			out = append(out, &candidate{inst: inst, model: inst.models[0], rewrite: true})
		}
	}
	return out
}

// roleChain: every instance declaring the role (or a wildcard) can serve it.
func roleChain(insts []*instanceRuntime, role string) []*candidate {
	out := make([]*candidate, 0, len(insts))
	for _, inst := range insts {
		if model, ok := inst.roles[role]; ok {
			out = append(out, &candidate{inst: inst, model: model, rewrite: true})
		} else if wildcard, ok := inst.roles[config.RoleWildcard]; ok {
			out = append(out, &candidate{inst: inst, model: wildcard, rewrite: true})
		}
	}
	return out
}

func wildcardChain(insts []*instanceRuntime) []*candidate {
	out := make([]*candidate, 0, len(insts))
	for _, inst := range insts {
		if model, ok := inst.roles[config.RoleWildcard]; ok {
			out = append(out, &candidate{inst: inst, model: model, rewrite: true})
		}
	}
	return out
}

// fallbackChain (R5): all instances are candidates, but the body stays verbatim.
func fallbackChain(proto protocol, insts []*instanceRuntime) []*candidate {
	out := make([]*candidate, 0, len(insts))
	for _, inst := range insts {
		if proto == protocolAnthropic {
			if model, ok := inst.roles[config.RoleWildcard]; ok {
				out = append(out, &candidate{inst: inst, model: model})
			}
			continue
		}
		if len(inst.models) > 0 {
			out = append(out, &candidate{inst: inst, model: inst.models[0]})
		}
	}
	if len(out) == 0 && len(insts) > 0 {
		// Safety net for R5 when no anthropic instance declares "*": keep M3
		// behavior and still attempt the first instance with the body untouched.
		out = append(out, &candidate{inst: insts[0]})
	}
	return out
}

func (inst *instanceRuntime) hasModel(model string) bool {
	for _, declared := range inst.models {
		if declared == model {
			return true
		}
	}
	for _, mapped := range inst.roles {
		if mapped == model {
			return true
		}
	}
	return false
}

func (inst *instanceRuntime) hasRole(role string) bool {
	_, ok := inst.roles[role]
	return ok
}

func (inst *instanceRuntime) hasWildcard() bool {
	return inst.hasRole(config.RoleWildcard)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// longContextSuffix is the long-context variant marker Claude Code appends to a
// role or model id (e.g. "opus[1m]"); it is not part of the routing name (§17.3).
const longContextSuffix = "[1m]"

// trimLongContextSuffix drops one trailing "[1m]" marker, case-insensitively.
func trimLongContextSuffix(model string) string {
	if len(model) >= len(longContextSuffix) &&
		strings.EqualFold(model[len(model)-len(longContextSuffix):], longContextSuffix) {
		return model[:len(model)-len(longContextSuffix)]
	}
	return model
}

// requestRole maps a role name or claude-<role>- id to the concrete role,
// ignoring a trailing long-context marker first (§17.3).
func requestRole(model string) (string, bool) {
	model = trimLongContextSuffix(model)
	for _, role := range concreteRoles {
		if model == role {
			return role, true
		}
	}
	for _, role := range concreteRoles {
		if strings.HasPrefix(model, "claude-"+role+"-") {
			return role, true
		}
	}
	return "", false
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

// attemptMetrics carries per-attempt measurements for logging (§17.6) and the
// generation-speed judgement (§17.4).
type attemptMetrics struct {
	instName string
	reqModel string
	model    string
	// hasModel records that some candidate supplied a model name, i.e. the
	// request actually reached a route. It stays false for non-AI calls.
	hasModel    bool
	inTokens    int
	hasIn       bool
	outTokens   int
	hasOut      bool
	genDuration time.Duration
	hasGen      bool
	genTPS      float64
	hasTPS      bool
}

const (
	minThroughputSampleTokens   = 100
	minThroughputSampleDuration = 2 * time.Second
)

// judgeThroughput computes generation speed over the generation window only and
// applies the minimum-sample guard. It returns slow=true when the sample is
// large enough and below min_tokens_per_second. Skipped samples are healthy.
func (h *Handler) judgeThroughput(m *attemptMetrics) (slow bool) {
	if h.minTokensPerSecond <= 0 || !m.hasOut || !m.hasGen {
		return false
	}
	if m.outTokens < minThroughputSampleTokens || m.genDuration < minThroughputSampleDuration {
		return false
	}
	tps := float64(m.outTokens) / m.genDuration.Seconds()
	m.genTPS, m.hasTPS = tps, true
	return tps < h.minTokensPerSecond
}

func formatSeconds(d time.Duration) string {
	return fmt.Sprintf("%.3f", d.Seconds())
}

func formatOptionalInt(value int, ok bool) string {
	if !ok {
		return "n/a"
	}
	return strconv.Itoa(value)
}

func formatTPS(tps float64) string {
	return fmt.Sprintf("%.0f", tps)
}

// clientMarkers maps User-Agent substrings to client identifiers, in the
// precedence order of §16.2. Matching is case-insensitive.
var clientMarkers = []struct {
	needles []string
	id      string
}{
	{[]string{"openai-python", "openai/python"}, "openai-python"},
	{[]string{"openai-node", "openai/node"}, "openai-node"},
	{[]string{"anthropic-sdk-typescript", "anthropic/js"}, "anthropic-js"},
	{[]string{"anthropic-sdk-python", "anthropic/python"}, "anthropic-python"},
	{[]string{"dsh"}, "dsh"},
}

// clientID derives a client identifier from request headers only (§16.2).
func clientID(r *http.Request) string {
	ua := strings.TrimSpace(r.Header.Get("User-Agent"))
	lower := strings.ToLower(ua)
	if version, ok := strings.CutPrefix(lower, "claude-cli/"); ok {
		if idx := strings.IndexByte(version, ' '); idx >= 0 {
			version = version[:idx]
		}
		if version == "" {
			return "claude-code"
		}
		return "claude-code/" + version
	}
	if r.Header.Get("X-Claude-Code-Session-Id") != "" {
		return "claude-code"
	}
	if strings.Contains(strings.ToLower(r.Header.Get("anthropic-beta")), "claude-code") {
		return "claude-code"
	}
	for _, marker := range clientMarkers {
		for _, needle := range marker.needles {
			if strings.Contains(lower, needle) {
				return marker.id
			}
		}
	}
	if strings.HasPrefix(lower, "curl/") {
		return "curl"
	}
	if ua == "" {
		return "unknown"
	}
	short := ua
	if len(short) > 32 {
		short = short[:32]
	}
	if idx := strings.IndexByte(short, ' '); idx >= 0 {
		short = short[:idx]
	}
	return short
}

// clientIP returns the caller address without its port (§16.3). Under a docker
// bridge only the gateway address is visible unless X-Forwarded-For is set.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		first = strings.TrimSpace(first)
		if first != "" && !strings.EqualFold(first, "unknown") {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// logRequest emits the §18.3 one-line request log in one of §23's three shapes:
// non-AI traffic carries neither segment, a routed call without usage keeps the
// model segment only, and a call with usage gets the full line. The gen segment
// appears only in the full shape, and only when the §17.4 sample passed.
func logRequest(r *http.Request, m attemptMetrics, total time.Duration) {
	// D1: a call that never reached a route (favicon, rejected credentials,
	// unknown path) prints neither "-> name/model (req_model)" nor "↑in ↓out".
	if !m.hasModel {
		log.Printf("%s %s in %ss from %s %s",
			r.Method, r.URL.Path,
			formatSeconds(total),
			clientIP(r), clientID(r))
		return
	}
	name, model, reqModel := m.instName, m.model, m.reqModel
	if name == "" {
		name = "n/a"
	}
	if model == "" {
		model = "n/a"
	}
	if reqModel == "" {
		reqModel = "n/a"
	}
	// D2: a routed call that produced no usage omits the "↑<in> ↓<out>" segment
	// instead of printing "↑n/a ↓n/a".
	if !m.hasIn && !m.hasOut {
		log.Printf("%s %s -> %s/%s (%s) in %ss from %s %s",
			r.Method, r.URL.Path, name, model, reqModel,
			formatSeconds(total),
			clientIP(r), clientID(r))
		return
	}
	// D3: full format. A response with only one side of the usage pair still
	// prints the segment, with "n/a" for the missing side (R1).
	gen := ""
	if m.hasTPS {
		gen = formatTPS(m.genTPS) + "t/s "
	}
	log.Printf("%s %s -> %s/%s (%s) in %ss ↑%s ↓%s %sfrom %s %s",
		r.Method, r.URL.Path, name, model, reqModel,
		formatSeconds(total),
		formatOptionalInt(m.inTokens, m.hasIn),
		formatOptionalInt(m.outTokens, m.hasOut),
		gen,
		clientIP(r), clientID(r))
}

// tryCandidate attempts one candidate. committed reports that a response was
// (at least partially) written to the client, so no further candidate may be
// tried. A non-nil error with committed=false is a retryable pre-response
// failure (connection/dial/response-header/first-token-timeout errors).
func (h *Handler) tryCandidate(w http.ResponseWriter, r *http.Request, proto protocol, cand *candidate, body []byte) (committed bool, m attemptMetrics, err error) {
	m.instName = cand.inst.name
	m.model = cand.model
	m.hasModel = true
	attemptBody := body
	if cand.rewrite {
		attemptBody = rewriteModel(body, cand.model)
	}

	target, err := url.JoinPath(cand.inst.baseURL.String(), upstreamPath(r.URL.Path))
	if err != nil {
		h.recordFailure(cand.inst)
		return false, m, fmt.Errorf("join upstream URL: %w", err)
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(attemptBody))
	if err != nil {
		h.recordFailure(cand.inst)
		return false, m, fmt.Errorf("build upstream request: %w", err)
	}
	outReq.ContentLength = int64(len(attemptBody))

	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	// ContentLength field is authoritative; a stale header would conflict with it.
	outReq.Header.Del("Content-Length")
	// Never forward the client's Accept-Encoding (§20.2): with the header absent
	// the Go transport negotiates gzip itself and transparently decompresses the
	// response, so the SSE parser and byte-rate window see plaintext.
	outReq.Header.Del("Accept-Encoding")
	// Client credentials are only for this proxy and never reach upstream.
	outReq.Header.Del("Authorization")
	if proto == protocolAnthropic {
		outReq.Header.Del("x-api-key")
		outReq.Header.Set("x-api-key", cand.inst.apiKey)
	} else {
		outReq.Header.Set("Authorization", "Bearer "+cand.inst.apiKey)
	}

	resp, err := cand.inst.client.Do(outReq)
	if err != nil {
		h.recordFailure(cand.inst)
		return false, m, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()
	headersAt := time.Now()

	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	decompressGzip := encoding == "gzip" || encoding == "x-gzip"
	parseable := true
	streaming := isEventStream(resp.Header)
	var respBody io.ReadCloser = resp.Body
	switch {
	case encoding == "" || encoding == "identity":
	case decompressGzip:
		// The transport was not asked for compression, so this only happens when
		// the upstream compresses regardless (§20.2). Decompress before any
		// parsing; streams defer the gzip header read to the first chunk so
		// first_token_timeout still covers a silent upstream.
		gz, gzErr := newGzipBody(resp.Body, streaming)
		if gzErr != nil {
			h.recordFailure(cand.inst)
			return false, m, fmt.Errorf("decompress upstream gzip body: %w", gzErr)
		}
		respBody = gz
	default:
		// br/zstd/etc: forward verbatim, but usage and the byte rate cannot be
		// parsed behind an encoding the proxy does not decode.
		parseable = false
		log.Printf("instance %s: upstream content-encoding %q is not decodable; usage and byte rate are unavailable", cand.inst.name, encoding)
	}

	writeResponseHead := func() {
		copyHeader(w.Header(), resp.Header)
		removeHopByHop(w.Header())
		if decompressGzip {
			// The client receives plaintext (§20.2).
			w.Header().Del("Content-Encoding")
			w.Header().Del("Content-Length")
		}
		if streaming {
			// Appending platform-generated tail events requires chunked framing.
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(resp.StatusCode)
	}

	if streaming {
		// For streams, headers are committed together with the first data chunk
		// so a first-token timeout can still fail over to another instance.
		outcome, streamErr := h.copySSE(w, respBody, h.idleTimeout, h.firstTokenTimeout,
			h.minTokensPerSecond, proto == protocolAnthropic, parseable, writeResponseHead)
		if outcome.firstTokenTimeout {
			h.recordFailure(cand.inst)
			return false, m, fmt.Errorf("first token timeout after %s", h.firstTokenTimeout)
		}
		if !outcome.committed {
			if streamErr != nil {
				h.recordFailure(cand.inst)
				return false, m, fmt.Errorf("read stream: %w", streamErr)
			}
			// Empty stream: commit the (empty) response and treat as success.
			writeResponseHead()
			cand.inst.recordSuccess()
			return true, m, nil
		}

		if outcome.chunks > 0 {
			m.genDuration = outcome.lastBlockAt.Sub(outcome.firstBlockAt)
			m.hasGen = outcome.chunks > 1
		}
		m.inTokens, m.hasIn = outcome.state.inTokens, outcome.state.hasIn
		m.outTokens, m.hasOut = outcome.state.tokens, outcome.state.hasTokens

		if outcome.idle {
			if proto == protocolAnthropic && !outcome.state.finished {
				if err := writePauseTurnTail(w, &outcome.state); err != nil {
					h.recordFailure(cand.inst)
					return true, m, fmt.Errorf("write pause_turn tail: %w", err)
				}
			} else if proto == protocolOpenAI {
				truncateClientConnection(w)
			}
			h.recordFailure(cand.inst)
			log.Printf("instance %s idle for %s; stream terminated", cand.inst.name, h.idleTimeout)
			return true, m, nil
		}
		if outcome.slow {
			// §19 real-time low throughput: the verdict already implies a failed
			// attempt, so the post-hoc throughput judgement is skipped.
			h.recordFailure(cand.inst)
			log.Printf("instance %s slow stream (realtime): %.0fB/s", cand.inst.name, outcome.slowBytesPerSecond)
			if proto == protocolAnthropic && !outcome.state.finished {
				if err := writePauseTurnTail(w, &outcome.state); err != nil {
					return true, m, fmt.Errorf("write pause_turn tail: %w", err)
				}
			}
			return true, m, nil
		}
		if streamErr != nil {
			h.recordFailure(cand.inst)
			return true, m, fmt.Errorf("copy response body: %w", streamErr)
		}
		if h.judgeThroughput(&m) {
			h.recordFailure(cand.inst)
			log.Printf("instance %s slow stream: %st/s", cand.inst.name, formatTPS(m.genTPS))
		} else {
			cand.inst.recordSuccess()
		}
		return true, m, nil
	}

	writeResponseHead()
	var copyErr error
	if parseable {
		var usage usageTokens
		capture := &cappedBuffer{limit: tokenCaptureLimit}
		copyErr = copyBodyTee(w, respBody, capture)
		readDone := time.Now()
		if !capture.truncated {
			usage = parseUsage(proto, capture.bytes())
		}
		m.inTokens, m.hasIn = usage.in, usage.hasIn
		m.outTokens, m.hasOut = usage.out, usage.hasOut
		m.genDuration = readDone.Sub(headersAt)
		m.hasGen = true
	} else {
		copyErr = copyBody(w, respBody)
	}

	if copyErr != nil {
		h.recordFailure(cand.inst)
		return true, m, fmt.Errorf("copy response body: %w", copyErr)
	}
	if h.judgeThroughput(&m) {
		h.recordFailure(cand.inst)
		log.Printf("instance %s slow: %st/s", cand.inst.name, formatTPS(m.genTPS))
	} else {
		cand.inst.recordSuccess()
	}
	return true, m, nil
}

func isEventStream(header http.Header) bool {
	return strings.HasPrefix(strings.ToLower(header.Get("Content-Type")), "text/event-stream")
}

const tokenCaptureLimit = 4 << 20 // 4 MiB

// cappedBuffer captures a response body up to limit; truncated bodies are not
// parsed for usage to avoid mis-measuring throughput.
type cappedBuffer struct {
	buf       []byte
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.truncated {
		return len(p), nil
	}
	remaining := b.limit - len(b.buf)
	if len(p) > remaining {
		b.buf = append(b.buf, p[:remaining]...)
		b.truncated = true
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *cappedBuffer) bytes() []byte { return b.buf }

// usageTokens is the usage subset mirrored from OpenAI or Anthropic responses.
type usageTokens struct {
	in     int
	hasIn  bool
	out    int
	hasOut bool
}

// parseUsage extracts prompt/input and completion/output token counts. Missing
// or unparseable usage yields hasIn/hasOut=false.
func parseUsage(proto protocol, body []byte) usageTokens {
	var payload struct {
		Usage struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			InputTokens      *int `json:"input_tokens"`
			OutputTokens     *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return usageTokens{}
	}
	var usage usageTokens
	if payload.Usage.PromptTokens != nil {
		usage.in, usage.hasIn = *payload.Usage.PromptTokens, true
	}
	if payload.Usage.InputTokens != nil {
		usage.in, usage.hasIn = *payload.Usage.InputTokens, true
	}
	if proto == protocolAnthropic {
		if payload.Usage.OutputTokens != nil {
			usage.out, usage.hasOut = *payload.Usage.OutputTokens, true
		}
		return usage
	}
	if payload.Usage.CompletionTokens != nil {
		usage.out, usage.hasOut = *payload.Usage.CompletionTokens, true
	}
	return usage
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

// copyBodyTee streams like copyBody while capturing the bytes for usage parsing.
func copyBodyTee(w http.ResponseWriter, src io.Reader, capture io.Writer) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if _, cerr := capture.Write(buf[:n]); cerr != nil {
				return cerr
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

// gzipBody decompresses a gzip-encoded upstream body. Close closes the
// underlying body so the stream timers can still abort a stalled upstream.
// A lazy body defers gzip.NewReader to the first Read; that keeps the stream's
// first-token timer in charge of how long the upstream may stay silent.
type gzipBody struct {
	src    io.ReadCloser
	lazy   bool
	reader *gzip.Reader
	err    error
}

func newGzipBody(src io.ReadCloser, lazy bool) (*gzipBody, error) {
	b := &gzipBody{src: src, lazy: lazy}
	if !lazy {
		if err := b.init(); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (b *gzipBody) init() error {
	if b.reader != nil || b.err != nil {
		return b.err
	}
	reader, err := gzip.NewReader(b.src)
	if err != nil {
		b.err = err
		return err
	}
	b.reader = reader
	return nil
}

func (b *gzipBody) Read(p []byte) (int, error) {
	if err := b.init(); err != nil {
		return 0, err
	}
	return b.reader.Read(p)
}

func (b *gzipBody) Close() error {
	return b.src.Close()
}

// sseState tracks the minimal Anthropic/OpenAI stream state needed for the
// pause_turn tail and streaming throughput judgement.
type sseState struct {
	pending    []byte
	inBlock    bool
	blockIndex int
	tokens     int
	hasTokens  bool
	inTokens   int
	hasIn      bool
	finished   bool
}

func (s *sseState) feed(chunk []byte) {
	s.pending = append(s.pending, chunk...)
	for {
		idx := bytes.IndexByte(s.pending, '\n')
		if idx < 0 {
			return
		}
		line := strings.TrimRight(string(s.pending[:idx]), "\r")
		s.pending = append([]byte(nil), s.pending[idx+1:]...)
		s.processLine(line)
	}
}

func (s *sseState) processLine(line string) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" {
		return
	}
	if payload == "[DONE]" {
		s.finished = true
		return
	}
	var event struct {
		Type  string `json:"type"`
		Index *int   `json:"index"`
		Usage struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			InputTokens      *int `json:"input_tokens"`
			OutputTokens     *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return
	}
	if event.Usage.PromptTokens != nil {
		s.inTokens = *event.Usage.PromptTokens
		s.hasIn = true
	}
	if event.Usage.InputTokens != nil {
		s.inTokens = *event.Usage.InputTokens
		s.hasIn = true
	}
	if event.Usage.CompletionTokens != nil {
		s.tokens = *event.Usage.CompletionTokens
		s.hasTokens = true
	}
	if event.Usage.OutputTokens != nil {
		s.tokens = *event.Usage.OutputTokens
		s.hasTokens = true
	}
	switch event.Type {
	case "content_block_start":
		s.inBlock = true
		if event.Index != nil {
			s.blockIndex = *event.Index
		}
	case "content_block_stop":
		s.inBlock = false
	case "message_stop":
		s.finished = true
	}
}

type sseOutcome struct {
	idle               bool
	firstTokenTimeout  bool
	slow               bool
	slowBytesPerSecond float64
	committed          bool
	chunks             int
	firstBlockAt       time.Time
	lastBlockAt        time.Time
	state              sseState
}

// §19 real-time throughput detection: token counts are unavailable mid-stream,
// so byte throughput is measured and converted with a rough ratio.
const (
	realtimeSlowWindow = 10 * time.Second
	bytesPerToken      = 3
)

// byteSample is one upstream chunk in the real-time throughput window.
type byteSample struct {
	ts time.Time
	n  int
}

// copySSE streams an SSE body. It arms a first-token timer until the first
// chunk (commit is invoked just before that chunk is written) and then switches
// to an idle timer reset on every chunk. When a timer fires the upstream body is
// closed and the outcome reports which phase timed out.
//
// It also tracks byte throughput over a sliding 10s window (§19). Once the
// window is full and the rate is below minTokensPerSecond*bytesPerToken the
// outcome reports slow; with slowInterrupt (Anthropic) the upstream body is
// closed immediately, otherwise (OpenAI) the stream keeps flowing.
//
// parse is false for bodies in an encoding the proxy cannot decode (§20.2);
// such streams are forwarded but neither usage nor the byte window is tracked.
func (h *Handler) copySSE(w http.ResponseWriter, body io.ReadCloser, idle, firstToken time.Duration, minTokensPerSecond float64, slowInterrupt, parse bool, commit func()) (outcome sseOutcome, err error) {
	var firstHit, idleHit, slowHit atomic.Bool
	var timerMu sync.Mutex
	var timer *time.Timer

	var window []byteSample
	var windowBytes int
	var firstChunkAt time.Time
	var slowBytes float64

	defer func() {
		outcome.slow = slowHit.Load()
		outcome.slowBytesPerSecond = slowBytes
	}()

	arm := func(d time.Duration, idleStage bool) {
		timerMu.Lock()
		defer timerMu.Unlock()
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		if d <= 0 {
			return
		}
		timer = time.AfterFunc(d, func() {
			if idleStage {
				idleHit.Store(true)
			} else {
				firstHit.Store(true)
			}
			_ = body.Close()
		})
	}
	defer func() {
		timerMu.Lock()
		if timer != nil {
			timer.Stop()
		}
		timerMu.Unlock()
	}()
	arm(firstToken, false)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			now := time.Now()
			if !outcome.committed {
				commit()
				outcome.committed = true
			}
			if outcome.chunks == 0 {
				outcome.firstBlockAt = now
			}
			outcome.lastBlockAt = now
			outcome.chunks++
			if parse {
				outcome.state.feed(buf[:n])
				if minTokensPerSecond > 0 {
					if firstChunkAt.IsZero() {
						firstChunkAt = now
					}
					window = append(window, byteSample{ts: now, n: n})
					windowBytes += n
					for len(window) > 0 && window[0].ts.Before(now.Add(-realtimeSlowWindow)) {
						windowBytes -= window[0].n
						window = window[1:]
					}
					if now.Sub(firstChunkAt) >= realtimeSlowWindow {
						rate := float64(windowBytes) / realtimeSlowWindow.Seconds()
						if rate < minTokensPerSecond*bytesPerToken && !slowHit.Swap(true) {
							slowBytes = rate
							if slowInterrupt {
								_ = body.Close()
							}
						}
					}
				}
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return outcome, werr
			}
			if flusher != nil {
				flusher.Flush()
			}
			arm(idle, true)
		}
		if err == io.EOF {
			return outcome, nil
		}
		if err != nil {
			if firstHit.Load() && outcome.chunks == 0 {
				outcome.firstTokenTimeout = true
				return outcome, nil
			}
			if idleHit.Load() && outcome.committed {
				outcome.idle = true
				return outcome, nil
			}
			if slowHit.Load() {
				// The body was closed by the slow-stream detector; report the
				// declared slow outcome instead of a spurious read error.
				return outcome, nil
			}
			return outcome, err
		}
	}
}

// writePauseTurnTail emits Anthropic's graceful stop for an idle stream so the
// client SDK can continue with pause_turn.
func writePauseTurnTail(w http.ResponseWriter, state *sseState) error {
	var builder strings.Builder
	if state.inBlock {
		data, err := json.Marshal(map[string]any{
			"type":  "content_block_stop",
			"index": state.blockIndex,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(&builder, "event: content_block_stop\ndata: %s\n\n", data)
	}

	tokens := 0
	if state.hasTokens {
		tokens = state.tokens
	}
	data, err := json.Marshal(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   "pause_turn",
			"stop_sequence": nil,
		},
		"usage": map[string]any{"output_tokens": tokens},
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(&builder, "event: message_delta\ndata: %s\n\n", data)
	builder.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

	if _, err := io.WriteString(w, builder.String()); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// truncateClientConnection aborts the client connection without a terminating
// chunk, so an OpenAI SSE consumer sees a truncated stream and decides on retry.
func truncateClientConnection(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
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
	// Capabilities carries optional metadata declared by the upstream model
	// list (Anthropic official schema). Models without declared metadata are
	// omitted; the field itself is omitted when nothing was declared.
	Capabilities map[string]config.ModelCapability `json:"capabilities,omitempty"`
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
	writeJSONError(w, http.StatusOK, probeModelsResponse{
		Models:       models,
		Capabilities: parseModelCapabilities(body),
	})
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

// upstreamModelEntry is one entry of a /v1/models response, kept as raw JSON so
// every metadata field can be decoded independently: one malformed value must
// not discard the rest of the entry.
type upstreamModelEntry map[string]json.RawMessage

// parseModelCapabilities extracts optional capability metadata from a
// /v1/models response. It understands the Anthropic official schema as well as
// OpenAI/DeepSeek-style aliases (context_window, max_output_tokens,
// input_modalities, effort, ...); Anthropic official fields win when both
// spellings are present. It is best-effort: absent or malformed metadata yields
// a nil map and never fails the model-name probe.
func parseModelCapabilities(body []byte) map[string]config.ModelCapability {
	var payload struct {
		Data []upstreamModelEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	capabilities := make(map[string]config.ModelCapability)
	for _, item := range payload.Data {
		id := strings.TrimSpace(rawString(item["id"]))
		if id == "" {
			continue
		}
		capability := modelCapabilityFromEntry(item)
		if capability == (config.ModelCapability{}) {
			continue
		}
		capabilities[id] = capability
	}
	if len(capabilities) == 0 {
		return nil
	}
	return capabilities
}

func modelCapabilityFromEntry(item upstreamModelEntry) config.ModelCapability {
	limit := rawObject(item["limit"])
	topProvider := rawObject(item["top_provider"])
	return config.ModelCapability{
		MaxInputTokens: firstPositiveInt(
			item["max_input_tokens"],
			item["context_window"],
			item["context_length"],
			item["contextWindow"],
			limit["context"],
		),
		MaxTokens: firstPositiveInt(
			item["max_tokens"],
			item["max_output_tokens"],
			item["maxTokens"],
			item["maxOutputTokens"],
			item["max_completion_tokens"],
			topProvider["max_completion_tokens"],
			limit["output"],
		),
		ImageInput: officialOrInferredBool(
			upstreamCapabilitySupported(item, "image_input"),
			inputModalitySupported(item, "image"),
		),
		PDFInput: officialOrInferredBool(
			upstreamCapabilitySupported(item, "pdf_input"),
			inputModalitySupported(item, "pdf"),
		),
		Thinking: officialOrInferredBool(
			upstreamCapabilitySupported(item, "thinking"),
			thinkingIndicator(item),
		),
		StructuredOutputs: upstreamCapabilitySupported(item, "structured_outputs"),
	}
}

// upstreamCapabilitySupported reads the Anthropic official
// capabilities.<name>.supported flag.
func upstreamCapabilitySupported(item upstreamModelEntry, name string) *bool {
	capabilities := rawObject(item["capabilities"])
	flag := rawObject(capabilities[name])
	return rawBool(flag["supported"])
}

// officialOrInferredBool gives the Anthropic official flag priority over the
// OpenAI-style inference when both spellings are declared.
func officialOrInferredBool(official, inferred *bool) *bool {
	if official != nil {
		return official
	}
	return inferred
}

// inputModalitySupported reports whether input_modalities declares modality.
// A present array is an explicit declaration: a missing modality yields false
// rather than "undeclared".
func inputModalitySupported(item upstreamModelEntry, modality string) *bool {
	raw, ok := item["input_modalities"]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var modalities []string
	if err := json.Unmarshal(raw, &modalities); err != nil {
		return nil
	}
	supported := slices.Contains(modalities, modality)
	return &supported
}

// thinkingIndicator treats a declared effort (object or non-empty string) or a
// present reasoning field as evidence that the model supports thinking.
func thinkingIndicator(item upstreamModelEntry) *bool {
	if raw, ok := item["effort"]; ok && effortDeclared(raw) {
		return declaredTrue()
	}
	if _, ok := item["reasoning"]; ok {
		return declaredTrue()
	}
	return nil
}

func effortDeclared(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		return false
	}
	switch trimmed[0] {
	case '{':
		return true
	case '"':
		var level string
		return json.Unmarshal(trimmed, &level) == nil && level != ""
	default:
		return false
	}
}

func declaredTrue() *bool {
	value := true
	return &value
}

func firstPositiveInt(values ...json.RawMessage) int {
	for _, raw := range values {
		if value := positiveInt(raw); value > 0 {
			return value
		}
	}
	return 0
}

// positiveInt accepts only a JSON integer literal above zero; 0, negatives,
// floats, exponents, strings and other types count as undeclared.
func positiveInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func rawString(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

func rawObject(raw json.RawMessage) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	return object
}

func rawBool(raw json.RawMessage) *bool {
	if isJSONNull(raw) {
		return nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return &value
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
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
