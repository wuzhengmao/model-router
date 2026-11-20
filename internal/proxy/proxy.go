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
	"net/http/httptrace"
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

// API paths served by this proxy.
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

	// §24.15.3 per-instance breaker controls.
	PathInstancesLock   = "/api/instances/lock"
	PathInstancesUnlock = "/api/instances/unlock"

	// §24.31 M159: manual balance refresh of one or all instances.
	PathInstancesBalance = "/api/instances/balance"
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

// errLockedHandoff marks a non-locked stream attempt that was aborted before it
// produced content while the protocol is pinned (§24.25 M94). serveChain uses it
// to route the request back to the locked instance.
var errLockedHandoff = errors.New("handoff to locked instance")

// errRequestCancelledByOperator marks a request terminated from the monitor
// page (§24.29). It is never treated as an upstream failure.
var errRequestCancelledByOperator = errors.New("request cancelled by operator")

// errAllQuotasExhausted is the client facing error of a request whose whole
// candidate chain ran out of quota (§24.31 M162).
const errAllQuotasExhausted = "all instance quotas exhausted"

// instanceRuntime is one configured instance plus its in-memory fallback state.
type instanceRuntime struct {
	name         string
	proto        protocol
	index        int
	baseURL      *url.URL
	apiKey       string
	timeout      time.Duration
	models       []string
	roles        map[string]string
	roleNames    map[string]string
	capabilities map[string]config.ModelCapability
	client       *http.Client
	// lockedClient is used instead of client while this instance is pinned
	// (§24.25 M95): all three connection/header timeouts are zero, so the
	// locked instance is never cut off by its own timeout. It stays separate
	// from client so unlocking restores the configured timeout immediately.
	lockedClient *http.Client

	// now is the clock the breaker compares against degradedUntil (§24.17 M35).
	// A nil clock means time.Now, so literal instances in tests stay usable.
	now func() time.Time

	mu                  sync.Mutex
	consecutiveFailures int
	degradedUntil       time.Time
	degradedSince       time.Time
	degradeReason       string
	// singleInstanceProtocol disables the breaker when the protocol has exactly
	// one configured instance (§24.16 M32): there is no alternative instance to
	// fail over to, so opening would only pin a red state without diverting
	// traffic. Set from len(protocolRuntime.insts) at construction.
	singleInstanceProtocol bool

	// §24.31 M153: the optional balance/quota check of this instance. An
	// empty balanceKind means the instance never takes part in a balance
	// check. The fields below are guarded by mu.
	balanceKind     string
	balanceURL      string
	balanceInterval time.Duration
	balanceInfo     balanceInfo
	// quotaExhausted marks an instance whose quota/balance is used up
	// (§24.31 M160). It is independent of the breaker counters.
	quotaExhausted      bool
	quotaExhaustedSince time.Time
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
	protocols          map[protocol]*protocolRuntime
	monitor            *monitorHub
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
			proto:   proto,
			index:   len(h.byProtocol[proto]),
			baseURL: base,
			apiKey:  inst.APIKey,
			timeout: inst.Timeout.Duration(),
			now:     opts.Now,
			client:  newClient(inst.Timeout.Duration()),
			// §24.25 M95: the pinned instance is exempt from every network
			// timeout. Keeping a second client makes lock/unlock take effect on
			// the next attempt without mutating the unlocked client.
			lockedClient: newClient(0),
		}
		// §24.31 M153: the balance check is opt-in and never inferred from the
		// provider.
		if inst.Balance != nil {
			kind := strings.ToLower(strings.TrimSpace(inst.Balance.Kind))
			switch kind {
			case config.BalanceKindMiniMaxPlan, config.BalanceKindDeepSeek:
			default:
				return nil, fmt.Errorf("instances[%d]: balance.kind must be %q or %q, got %q",
					i, config.BalanceKindMiniMaxPlan, config.BalanceKindDeepSeek, inst.Balance.Kind)
			}
			ir.balanceKind = kind
			ir.balanceURL = strings.TrimSpace(inst.Balance.URL)
			ir.balanceInterval = inst.Balance.PollInterval()
			ir.balanceInfo = balanceInfo{
				Kind:              kind,
				Interval5hPercent: unknownPercent,
				Weekly7dPercent:   unknownPercent,
			}
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
	h.protocols = map[protocol]*protocolRuntime{
		protocolOpenAI:    newProtocolRuntime(h.byProtocol[protocolOpenAI]),
		protocolAnthropic: newProtocolRuntime(h.byProtocol[protocolAnthropic]),
	}
	// §24.16 M32: the judge is the configured instance count of the protocol, not
	// the current number of selectable instances. A multi-instance protocol keeps
	// the breaker even if only one instance is healthy right now.
	for _, ps := range h.protocols {
		if len(ps.insts) == 1 {
			ps.insts[0].singleInstanceProtocol = true
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
			// §24.10 A2: owned_by names the first instance that declares the
			// model, unless the instance has no display name.
			owner := inst.name
			if owner == "" {
				owner = "model-router"
			}
			entry := openAIModelEntry{
				ID:      model,
				Object:  "model",
				Created: now,
				OwnedBy: owner,
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

// §24.20 M49/M50: a status outside 2xx received before anything was written to
// the client is a retryable candidate failure. The buffered response is kept so
// the last real rejection can be forwarded verbatim once every candidate has
// failed (§24.21 M52/M60).
const (
	upstreamErrorBodyLimit   = 4 << 20 // bytes buffered for verbatim forwarding
	upstreamBodyExcerptLimit = 512     // bytes kept for logs and recentErrors
)

type upstreamResponseError struct {
	status int
	header http.Header
	body   []byte
}

func (e *upstreamResponseError) Error() string {
	if excerpt := e.excerpt(); excerpt != "" {
		return fmt.Sprintf("upstream status %d: %s", e.status, excerpt)
	}
	return fmt.Sprintf("upstream status %d", e.status)
}

// excerpt returns a readable prefix of the response body. A gzip-encoded body
// (preserved for verbatim forwarding) is decoded best-effort for the log.
func (e *upstreamResponseError) excerpt() string {
	body := e.body
	switch strings.ToLower(strings.TrimSpace(e.header.Get("Content-Encoding"))) {
	case "gzip", "x-gzip":
		if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			decoded, readErr := io.ReadAll(io.LimitReader(zr, upstreamBodyExcerptLimit))
			_ = zr.Close()
			if readErr == nil {
				body = decoded
			}
		}
	}
	if len(body) > upstreamBodyExcerptLimit {
		body = body[:upstreamBodyExcerptLimit]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(body), "�"))
}

// forwardUpstreamError relays the buffered non-2xx response unchanged
// (§24.21 M52/M60): same status, headers and body; only the hop-by-hop headers
// and Content-Length (the body may have been buffered/truncated) are adjusted.
func forwardUpstreamError(w http.ResponseWriter, e *upstreamResponseError) {
	copyHeader(w.Header(), e.header)
	removeHopByHop(w.Header())
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Length", strconv.Itoa(len(e.body)))
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}

// serveChain runs the fallback state machine: build the candidate chain for the
// request, try the first closed instance, retry pre-response failures on the
// next closed candidate, and record outcomes. §24.17 M36/M37: there is no
// current pointer — every request starts at the first closed instance on the
// chain. §24.19 M45–M48: an all-open chain tries every candidate in chain order,
// starting at index 0. A locked protocol sends everything to the pinned
// instance (§24.15 M10).
//
// §24.21 adds a fallback round after the first round fails: every protocol
// instance the first round never called is tried in configuration order, with
// the instance's own mapped model (M56–M58). Each instance is attempted at most
// once per request (M61/M15bl).
func (h *Handler) serveChain(w http.ResponseWriter, r *http.Request, proto protocol, insts []*instanceRuntime, body []byte) {
	start := time.Now()
	reqModel, _ := requestModel(body)

	locked := -1
	if ps := h.protocolRuntime(proto); ps != nil {
		locked = ps.lockedIndex()
	}
	var chain []*candidate
	lockedMode := locked >= 0 && locked < len(insts)
	var lockedCand *candidate
	if lockedMode {
		// §24.25 M93: the pinned instance is always first, then every other
		// instance in configuration order, each with its own mapped model
		// (§24.25 M99/M100).
		lockedCand = lockedCandidate(proto, insts, body, locked)
		if lockedCand != nil {
			chain = append(chain, lockedCand)
		}
		for i, inst := range insts {
			if i == locked {
				continue
			}
			chain = append(chain, mappedCandidate(proto, inst, body))
		}
	} else {
		chain = buildChainFrom(proto, insts, body)
	}

	// §24.17 M36/M39: closed candidates are tried in chain order; open ones are
	// skipped. §24.19 M45/M46: when every candidate is open the whole chain is
	// tried from index 0, so a failing head falls through to the next instance.
	// §24.25 M93: the pinned candidate is never skipped, even when open.
	// §24.31 M161: exhausted instances never enter the attempts and never take
	// part in the all-open fallback.
	var attempts []*candidate
	var exhaustedCands []*candidate
	if lockedMode {
		if lockedCand != nil {
			attempts = append(attempts, lockedCand)
		}
		var closed, rest []*candidate
		for _, cand := range chain {
			if cand == lockedCand {
				// §24.31 M163: an operator pin wins over the quota state.
				continue
			}
			if cand.inst.isExhausted() {
				continue
			}
			rest = append(rest, cand)
			if !cand.inst.isOpen() {
				closed = append(closed, cand)
			}
		}
		if len(closed) > 0 {
			attempts = append(attempts, closed...)
		} else {
			attempts = append(attempts, rest...)
		}
	} else {
		for _, cand := range chain {
			if cand.inst.isExhausted() {
				exhaustedCands = append(exhaustedCands, cand)
				continue
			}
			if !cand.inst.isOpen() {
				attempts = append(attempts, cand)
			}
		}
		if len(attempts) == 0 {
			for _, cand := range chain {
				if cand.inst.isExhausted() {
					continue
				}
				attempts = append(attempts, cand)
			}
		}
	}

	// plan is the first round plus the §24.21 fallback round. fallbackStart
	// separates the rounds so the monitor reports the right retry reason.
	plan := attempts
	fallbackStart := len(attempts)
	// §24.31 M162: every candidate of the chain is out of quota, so the
	// request cannot be served at all.
	allExhausted := len(attempts) == 0 && len(exhaustedCands) > 0
	if !lockedMode && !allExhausted {
		seen := make(map[*instanceRuntime]bool, len(insts))
		for _, cand := range attempts {
			seen[cand.inst] = true
		}
		byInst := make(map[*instanceRuntime]*candidate, len(chain))
		for _, cand := range chain {
			byInst[cand.inst] = cand
		}
		// M56–M58/M61: every protocol instance the first round never called is
		// topped up in configuration order. A chain candidate kept its own
		// mapped model; a chain-external instance is mapped from its own roles
		// or models, never from the client's original model name.
		for _, inst := range insts {
			if seen[inst] {
				continue
			}
			seen[inst] = true
			// §24.31 M161: an instance without quota is never called.
			if inst.isExhausted() {
				continue
			}
			cand := byInst[inst]
			if cand == nil {
				cand = fallbackCandidate(proto, inst, body)
				if cand == nil {
					continue
				}
			}
			plan = append(plan, cand)
		}
	}

	tracker := h.monitor.trackRequest(r, proto, reqModel)
	// §24.29 M137: derive a request-scoped cancellable context. Every upstream
	// attempt inherits it, so an operator cancel interrupts in-flight header
	// waits and body reads without touching the shared transport connection.
	ctx, cancelRequest := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	tracker.setCancel(cancelRequest)
	defer cancelRequest()

	finalStatus := 0
	var finalMetrics attemptMetrics
	var finalErr error
	// §24.29 M139–M144: cancellation stops the chain, skips breaker accounting
	// and maps to 502 (pre-header) or a truncated connection (committed).
	finishCancelled := func(committed bool, status int, m attemptMetrics) {
		m.reqModel = reqModel
		finalMetrics = m
		finalErr = errRequestCancelledByOperator
		inst := m.instName
		if inst == "" {
			inst = "-"
		}
		if committed {
			finalStatus = status
			// Log before closing the client connection: the client observes the
			// truncation immediately, and the reason line must not race it.
			log.Printf("request %s cancelled by operator inst=%s phase=streaming", tracker.id, inst)
			truncateClientConnection(w)
		} else {
			finalStatus = http.StatusBadGateway
			log.Printf("request %s cancelled by operator inst=%s phase=pre-header", tracker.id, inst)
			writeProtocolError(w, proto, http.StatusBadGateway, errRequestCancelledByOperator.Error())
		}
		logRequest(r, m, time.Since(start))
	}
	// §24.22 M63/M69 and §24.26 M115/M116: the summary is computed for failed
	// requests and for requests that carry tool_use. A successful request only
	// gets the log line; reqShape is written to stats on failure alone (M119).
	defer func() {
		failed := finalStatus >= http.StatusBadRequest || finalStatus == 0
		if failed || requestMayHaveToolUse(body) {
			stats := computeReqShapeStats(proto, body)
			if stats.ToolUseMsgs > 0 {
				log.Printf("reqshape msgs=%d tool_use_msgs=%d paired_msgs=%d unpaired_idx=%s thinking=%s last=%s session=%s",
					stats.Messages, stats.ToolUseMsgs, stats.PairedMsgs, formatUnpaired(stats),
					stats.Declared, stats.Last, stats.Session)
			}
			if failed {
				finalMetrics.reqShape = buildReqShapeWithStats(proto, body, stats)
			}
		}
		tracker.end(finalStatus, finalErr, finalMetrics)
	}()

	// §24.31 M162: no candidate left because every one of them ran out of
	// quota. The client gets a 502 naming the exhausted instances.
	if allExhausted {
		names := make([]string, 0, len(exhaustedCands))
		for _, cand := range exhaustedCands {
			names = append(names, cand.inst.name)
		}
		finalStatus = http.StatusBadGateway
		finalErr = errors.New(errAllQuotasExhausted)
		finalMetrics = attemptMetrics{instName: names[0], reqModel: reqModel}
		tracker.begin(exhaustedCands[0])
		writeProtocolError(w, proto, http.StatusBadGateway,
			errAllQuotasExhausted+": "+strings.Join(names, ", "))
		log.Printf("%s %s -> (%s) total=%ss status=502 %s", r.Method, r.URL.Path, proto,
			formatSeconds(time.Since(start)), errAllQuotasExhausted)
		logRequest(r, finalMetrics, time.Since(start))
		return
	}

	// last is the candidate that failed before any response was written. Its
	// model information goes into the final log line so that a failed routed
	// call still shows which instance/model was tried (§23 only drops the
	// segment for traffic that never reached a route).
	var last *candidate
	// lastUpstream keeps the last real non-2xx upstream response so it can be
	// forwarded instead of a synthetic 502 (§24.21 M60).
	var lastUpstream *upstreamResponseError
	var lastUpstreamCand *candidate
	var lastUpstreamMetrics attemptMetrics
	handoffUsed := false
	for i := 0; i < len(plan); i++ {
		cand := plan[i]
		lockedExempt := lockedMode && cand == lockedCand
		handoffBeforeContent := lockedMode && !lockedExempt
		tracker.begin(cand)
		committed, status, metrics, err := h.tryCandidate(w, r, proto, cand, body, tracker, lockedExempt, handoffBeforeContent)
		// §24.29 M139: an operator cancel terminates the request immediately;
		// no later candidate or §24.21 fallback round may start.
		if tracker.operatorCancelled() {
			finishCancelled(committed, status, metrics)
			return
		}
		if committed {
			if err != nil {
				log.Printf("%s %s response committed with error: %v", r.Method, r.URL.Path, err)
			}
			metrics.reqModel = reqModel
			finalStatus, finalMetrics, finalErr = status, metrics, err
			logRequest(r, metrics, time.Since(start))
			return
		}
		log.Printf("%s %s candidate %s failed: %v", r.Method, r.URL.Path, cand.inst.name, err)
		last = cand
		finalMetrics = metrics
		var upstream *upstreamResponseError
		if errors.As(err, &upstream) {
			lastUpstream, lastUpstreamCand, lastUpstreamMetrics = upstream, cand, metrics
		}
		// §24.25 M94: while pinned, a non-locked stream that has not produced
		// content is aborted and handed back to the locked instance at once.
		if lockedMode && !handoffUsed && lockedCand != nil && cand != lockedCand &&
			errors.Is(err, errLockedHandoff) {
			handoffUsed = true
			plan = append(plan[:i+1], lockedCand)
			tracker.retry(lockedCand, retryReasonFallbackRetry)
			continue
		}
		if i+1 < len(plan) {
			reason := retryReason(err)
			if i+1 >= fallbackStart {
				reason = retryReasonFallbackRetry
			}
			tracker.retry(plan[i+1], reason)
		}
	}

	// §24.25 M94d/M15de: after every non-locked candidate failed before the
	// client saw a byte, the pinned instance gets one final try. The handoff is
	// used at most once; afterwards routing never returns to non-locked peers.
	if lockedMode && !handoffUsed && lockedCand != nil && last != nil && last != lockedCand {
		handoffUsed = true
		tracker.begin(lockedCand)
		committed, status, metrics, err := h.tryCandidate(w, r, proto, lockedCand, body, tracker, true, false)
		if tracker.operatorCancelled() {
			finishCancelled(committed, status, metrics)
			return
		}
		if committed {
			if err != nil {
				log.Printf("%s %s response committed with error: %v", r.Method, r.URL.Path, err)
			}
			metrics.reqModel = reqModel
			finalStatus, finalMetrics, finalErr = status, metrics, err
			logRequest(r, metrics, time.Since(start))
			return
		}
		log.Printf("%s %s candidate %s failed: %v", r.Method, r.URL.Path, lockedCand.inst.name, err)
		last = lockedCand
		finalMetrics = metrics
		var upstream *upstreamResponseError
		if errors.As(err, &upstream) {
			lastUpstream, lastUpstreamCand, lastUpstreamMetrics = upstream, lockedCand, metrics
		}
	}

	// §24.29 M139: a cancel between the last attempt and the final response
	// must still terminate without forwarding a candidate response.
	if tracker.operatorCancelled() {
		finishCancelled(false, 0, finalMetrics)
		return
	}

	if lastUpstream != nil && lastUpstreamCand != nil {
		// M52/M60: with no candidate left, relay the last upstream rejection
		// unchanged instead of hiding it behind a 502.
		tracker.begin(lastUpstreamCand)
		log.Printf("%s %s -> (%s) total=%ss status=%d all candidates failed (forwarding upstream response)",
			r.Method, r.URL.Path, proto, formatSeconds(time.Since(start)), lastUpstream.status)
		forwardUpstreamError(w, lastUpstream)
		lastUpstreamMetrics.reqModel = reqModel
		finalStatus = lastUpstream.status
		finalErr = lastUpstream
		finalMetrics = lastUpstreamMetrics
		logRequest(r, finalMetrics, time.Since(start))
		return
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
	finalStatus = http.StatusBadGateway
	finalErr = errors.New("upstream request failed")
	writeProtocolError(w, proto, http.StatusBadGateway, "upstream request failed")
}

// buildChainFrom determines the base instance per §9.2 and collects the fallback
// candidates from there, each with its upstream model and rewrite flag (§11.2).
// The base is purely the model-capability match; routing no longer truncates
// the chain at a current pointer (§24.17 M37).
func buildChainFrom(proto protocol, insts []*instanceRuntime, body []byte) []*candidate {
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
		return fallbackChain(proto, from)
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

// fallbackCandidate maps one protocol instance the first round never called
// onto the request with that instance's own upstream model (§24.21 M58): the
// requested role or wildcard for Anthropic, the declared model or the first
// model for OpenAI. It returns nil when the instance has no mapping, so the
// fallback round never falls back to the client's original model name.
func fallbackCandidate(proto protocol, inst *instanceRuntime, body []byte) *candidate {
	model, rewrite, ok := resolveInstanceModel(proto, inst, body)
	if !ok {
		return nil
	}
	return &candidate{inst: inst, model: model, rewrite: rewrite}
}

// resolveInstanceModel returns the upstream model this instance maps the
// request onto and whether the body must be rewritten (§24.25 M99/M100).
// ok=false means the instance declares no usable mapping; callers decide how to
// fall back (lockedCandidate keeps the client model untouched, fallbackCandidate
// skips the instance).
func resolveInstanceModel(proto protocol, inst *instanceRuntime, body []byte) (model string, rewrite, ok bool) {
	requested, usable := requestModel(body)
	switch proto {
	case protocolAnthropic:
		if usable {
			if inst.hasModel(requested) {
				return requested, false, true
			}
			if role, ok := requestRole(requested); ok {
				if mapped, ok := inst.roles[role]; ok {
					return mapped, true, true
				}
			}
		}
		if mapped, ok := inst.roles[config.RoleWildcard]; ok {
			return mapped, true, true
		}
		if mapped, ok := firstRoleMapping(inst); ok {
			return mapped, true, true
		}
		return "", false, false
	default:
		if len(inst.models) == 0 {
			return "", false, false
		}
		if usable && contains(inst.models, requested) {
			return requested, false, true
		}
		return inst.models[0], true, true
	}
}

// mappedCandidate builds a candidate with the instance's own mapping. Unlike
// fallbackCandidate it never returns nil: when no mapping resolves, the client
// model is kept verbatim (§24.25 M99).
func mappedCandidate(proto protocol, inst *instanceRuntime, body []byte) *candidate {
	model, rewrite, ok := resolveInstanceModel(proto, inst, body)
	if ok {
		return &candidate{inst: inst, model: model, rewrite: rewrite}
	}
	requested, _ := requestModel(body)
	return &candidate{inst: inst, model: requested}
}

// firstRoleMapping returns the instance's mapping in canonical role order
// (wildcard is handled by fallbackCandidate), so an Anthropic instance that
// maps no requested role still contributes a deterministic own model.
func firstRoleMapping(inst *instanceRuntime) (string, bool) {
	for _, role := range concreteRoles {
		if model, ok := inst.roles[role]; ok {
			return model, true
		}
	}
	return "", false
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
	// upstreamBody is the excerpt of a non-2xx upstream body (§24.20 M53).
	upstreamBody string
	// reqShape is the §24.22 M63 structural summary of the client request body,
	// filled only when the request ends in failure.
	reqShape string
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
func (h *Handler) tryCandidate(w http.ResponseWriter, r *http.Request, proto protocol, cand *candidate, body []byte, tracker *requestMonitor, lockedExempt, handoffBeforeContent bool) (committed bool, status int, m attemptMetrics, err error) {
	attemptStart := time.Now()
	m.instName = cand.inst.name
	m.model = cand.model
	m.hasModel = true
	// §24.29 M142: an operator cancel is not an upstream failure and must not
	// touch the breaker, even when a timer/error path also fires.
	recordFailure := func() {
		if tracker.operatorCancelled() {
			return
		}
		h.recordFailure(cand.inst)
	}
	attemptBody := body
	if cand.rewrite {
		attemptBody = rewriteModel(body, cand.model)
	}

	target, err := url.JoinPath(cand.inst.baseURL.String(), upstreamPath(r.URL.Path))
	if err != nil {
		recordFailure()
		return false, 0, m, fmt.Errorf("join upstream URL: %w", err)
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	trace := &headerProfile{}
	requestCtx := r.Context()
	budget := time.Duration(0)
	var budgetFired atomic.Bool
	var budgetTimer *time.Timer
	var cancelBudget context.CancelFunc
	if !lockedExempt && cand.inst.timeout > 0 {
		// §24.26 M110: this budget covers connect, upload and the wait for
		// response headers. http.Client.Timeout would kill the SSE body, so the
		// deadline is a timer that cancels a derived context; stopping the
		// timer after Do returns removes the deadline from the stream.
		budget = cand.inst.timeout
		requestCtx, cancelBudget = context.WithCancel(requestCtx)
		remaining := time.Until(attemptStart.Add(budget))
		if remaining < 0 {
			remaining = 0
		}
		budgetTimer = time.AfterFunc(remaining, func() {
			budgetFired.Store(true)
			cancelBudget()
		})
		// Runs after the response body has been consumed; stopping the timer
		// is what removes the deadline once headers arrive.
		defer cancelBudget()
	}
	requestCtx = httptrace.WithClientTrace(requestCtx, trace.trace())

	outReq, err := http.NewRequestWithContext(requestCtx, r.Method, target, bytes.NewReader(attemptBody))
	if err != nil {
		recordFailure()
		return false, 0, m, fmt.Errorf("build upstream request: %w", err)
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

	client := cand.inst.client
	if lockedExempt && cand.inst.lockedClient != nil {
		// §24.25 M95: the pinned instance uses the timeout-free client.
		client = cand.inst.lockedClient
	}
	resp, err := client.Do(outReq)
	if budgetTimer != nil {
		budgetTimer.Stop()
	}
	budgetHit := budgetFired.Load()
	if err != nil {
		// §24.29: an operator cancel wins over budget/upstream errors and never
		// counts as an instance failure.
		if tracker.operatorCancelled() {
			return false, 0, m, errRequestCancelledByOperator
		}
		recordFailure()
		if budgetHit {
			// §24.27 M121/M122: the budget fired exactly when the diagnostic is
			// needed. Use the trigger moment as the observation point so hdr is
			// about the instance timeout and passes M118's half-timeout gate.
			trace.log(cand.inst.name, attemptStart, time.Now(), cand.inst.timeout, len(attemptBody), budget)
			// §24.26 M111: the pre-header budget fired before any response.
			return false, 0, m, fmt.Errorf("response header budget exceeded after %s: %w", formatProfileDuration(budget), err)
		}
		return false, 0, m, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()
	headersAt := time.Now()
	// §24.26 M118: only slow header waits are logged.
	trace.log(cand.inst.name, attemptStart, headersAt, cand.inst.timeout, len(attemptBody), budget)

	// §24.20 M49–M51: a final status outside 2xx is a failed candidate. Nothing
	// has been written to the client yet, so the body is buffered and the chain
	// may move on; if this turns out to be the last opportunity, serveChain
	// forwards the buffered response verbatim (M52).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorBodyLimit+1))
		// §24.29 M142: a cancel during the error-body read is still not an
		// upstream failure.
		if tracker.operatorCancelled() {
			return false, resp.StatusCode, m, errRequestCancelledByOperator
		}
		recordFailure()
		if readErr != nil {
			return false, 0, m, fmt.Errorf("read upstream status %d body: %w", resp.StatusCode, readErr)
		}
		if len(errBody) > upstreamErrorBodyLimit {
			errBody = errBody[:upstreamErrorBodyLimit]
		}
		statusErr := &upstreamResponseError{
			status: resp.StatusCode,
			header: resp.Header.Clone(),
			body:   errBody,
		}
		m.upstreamBody = statusErr.excerpt()
		return false, resp.StatusCode, m, statusErr
	}

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
			recordFailure()
			return false, 0, m, fmt.Errorf("decompress upstream gzip body: %w", gzErr)
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
		streamIdle, streamFirstToken, streamContentIdle := h.idleTimeout, h.firstTokenTimeout, cand.inst.timeout
		slowInterrupt := proto == protocolAnthropic
		minTPS := h.minTokensPerSecond
		if lockedExempt {
			// §24.25 M95/M97: the pinned instance is exempt from every time
			// budget, including the real-time slow-stream interruption.
			streamIdle, streamFirstToken, streamContentIdle = 0, 0, 0
			slowInterrupt = false
			minTPS = 0
		}
		// For streams, headers are committed together with the first data chunk
		// so a first-token timeout can still fail over to another instance.
		outcome, streamErr := h.copySSE(w, respBody, streamIdle, streamFirstToken, streamContentIdle,
			minTPS, slowInterrupt, parseable, cand.inst.name,
			proto == protocolAnthropic && parseable, proto == protocolAnthropic && parseable,
			lockedExempt, handoffBeforeContent, writeResponseHead, tracker)
		// §24.23 M76: exactly one stage summary per streaming attempt.
		logStreamPhases(cand.inst.name, headersAt.Sub(attemptStart), outcome, trace.connID())
		// §24.26 M117: response shape sits next to the stage summary.
		injected := 0
		if outcome.injected {
			injected = 1
		}
		log.Printf("resp_shape thinking_blocks=%d tool_use_blocks=%d signature_empty=%d injected=%d",
			outcome.thinkingBlocks, outcome.toolUseBlocks, outcome.signatureEmpty, injected)
		// §24.29 M141: cancellation is checked before the idle/slow branches so
		// a concurrent content-idle timer cannot append a pause_turn tail. The
		// committed state decides whether serveChain truncates or returns 502.
		if tracker.operatorCancelled() {
			if outcome.committed {
				return true, resp.StatusCode, m, errRequestCancelledByOperator
			}
			return false, 0, m, errRequestCancelledByOperator
		}
		if outcome.handoff {
			// §24.25 M94b: the aborted non-locked attempt counts as a failure
			// (M92 exempts only the locked instance).
			recordFailure()
			return false, 0, m, errLockedHandoff
		}
		if outcome.firstTokenTimeout {
			log.Printf("instance %s timeout: first-token for %s; stream terminated", cand.inst.name, h.firstTokenTimeout)
			recordFailure()
			return false, 0, m, &attemptFailure{
				reason: retryReasonFirstTokenTimeout,
				err:    fmt.Errorf("first token timeout after %s", h.firstTokenTimeout),
			}
		}
		if !outcome.committed {
			if tracker.operatorCancelled() {
				return false, 0, m, errRequestCancelledByOperator
			}
			if streamErr != nil {
				recordFailure()
				return false, 0, m, fmt.Errorf("read stream: %w", streamErr)
			}
			// Empty stream: commit the (empty) response and treat as success.
			writeResponseHead()
			h.recordSuccess(cand.inst)
			return true, resp.StatusCode, m, nil
		}

		if outcome.chunks > 0 {
			m.genDuration = outcome.lastBlockAt.Sub(outcome.firstBlockAt)
			m.hasGen = outcome.chunks > 1
		}
		m.inTokens, m.hasIn = outcome.state.inTokens, outcome.state.hasIn
		m.outTokens, m.hasOut = outcome.state.tokens, outcome.state.hasTokens

		if outcome.idle || outcome.contentIdle {
			// §24.29 M141: even if a timer won the race, an operator cancel must
			// not append the pause_turn tail.
			if tracker.operatorCancelled() {
				return true, resp.StatusCode, m, errRequestCancelledByOperator
			}
			// §24.23 M73: the content-idle verdict shares this branch verbatim,
			// so the client-visible bytes match an idle_timeout exactly.
			// Record and log before truncating: an OpenAI truncation closes the
			// client connection immediately, and the reason line must not race
			// the client's EOF.
			recordFailure()
			if outcome.contentIdle {
				log.Printf("instance %s timeout: content-idle for %s (no content block); stream terminated", cand.inst.name, cand.inst.timeout)
			} else {
				log.Printf("instance %s idle for %s; stream terminated", cand.inst.name, h.idleTimeout)
			}
			if proto == protocolAnthropic && !outcome.state.finished {
				if err := writePauseTurnTail(w, &outcome.state); err != nil {
					return true, resp.StatusCode, m, fmt.Errorf("write pause_turn tail: %w", err)
				}
			} else if proto == protocolOpenAI {
				truncateClientConnection(w)
			}
			return true, resp.StatusCode, m, nil
		}
		if outcome.slow {
			// §24.29 M141: same guard for the slow-stream pause_turn tail.
			if tracker.operatorCancelled() {
				return true, resp.StatusCode, m, errRequestCancelledByOperator
			}
			// §19 real-time low throughput: the verdict already implies a failed
			// attempt, so the post-hoc throughput judgement is skipped.
			recordFailure()
			log.Printf("instance %s slow stream (realtime): %.0fB/s", cand.inst.name, outcome.slowBytesPerSecond)
			log.Printf("instance %s timeout: slow for %.0fB/s (min %st/s); stream %s", cand.inst.name,
				outcome.slowBytesPerSecond, formatTPS(h.minTokensPerSecond), slowStreamState(proto))
			if proto == protocolAnthropic && !outcome.state.finished {
				if err := writePauseTurnTail(w, &outcome.state); err != nil {
					return true, resp.StatusCode, m, fmt.Errorf("write pause_turn tail: %w", err)
				}
			}
			return true, resp.StatusCode, m, nil
		}
		if streamErr != nil {
			recordFailure()
			return true, resp.StatusCode, m, fmt.Errorf("copy response body: %w", streamErr)
		}
		if !lockedExempt && h.judgeThroughput(&m) {
			recordFailure()
			log.Printf("instance %s slow stream: %st/s", cand.inst.name, formatTPS(m.genTPS))
		} else {
			h.recordSuccess(cand.inst)
		}
		return true, resp.StatusCode, m, nil
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
		if tracker.operatorCancelled() {
			return true, resp.StatusCode, m, errRequestCancelledByOperator
		}
		recordFailure()
		return true, resp.StatusCode, m, fmt.Errorf("copy response body: %w", copyErr)
	}
	if tracker.operatorCancelled() {
		return true, resp.StatusCode, m, errRequestCancelledByOperator
	}
	if !lockedExempt && h.judgeThroughput(&m) {
		recordFailure()
		log.Printf("instance %s slow: %st/s", cand.inst.name, formatTPS(m.genTPS))
	} else {
		h.recordSuccess(cand.inst)
	}
	return true, resp.StatusCode, m, nil
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

// sseUsage is the usage subset shared by non-streaming bodies and SSE events,
// including Anthropic's nested message_start.message.usage object.
type sseUsage struct {
	PromptTokens             *int `json:"prompt_tokens"`
	CompletionTokens         *int `json:"completion_tokens"`
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
}

// inputTokens returns the full input count. Anthropic splits input into
// input_tokens plus cache creation/read counters, so they are summed (§24.15
// M17); OpenAI-style prompt_tokens already includes cached tokens. The primary
// counter is required so a cache-only frame cannot clobber a known total.
func (u sseUsage) inputTokens() (int, bool) {
	if u.InputTokens != nil {
		total := *u.InputTokens
		if u.CacheCreationInputTokens != nil {
			total += *u.CacheCreationInputTokens
		}
		if u.CacheReadInputTokens != nil {
			total += *u.CacheReadInputTokens
		}
		return total, true
	}
	if u.PromptTokens != nil {
		return *u.PromptTokens, true
	}
	return 0, false
}

func (u sseUsage) outputTokens() (int, bool) {
	if u.OutputTokens != nil {
		return *u.OutputTokens, true
	}
	if u.CompletionTokens != nil {
		return *u.CompletionTokens, true
	}
	return 0, false
}

// parseUsage extracts prompt/input and completion/output token counts. Missing
// or unparseable usage yields hasIn/hasOut=false.
func parseUsage(proto protocol, body []byte) usageTokens {
	var payload struct {
		Usage sseUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return usageTokens{}
	}
	var usage usageTokens
	usage.in, usage.hasIn = payload.Usage.inputTokens()
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

// copyBody copies a non-streaming upstream body to the client. It deliberately
// does not flush: net/http already drains the write buffer as it fills, and
// leaving the tail buffered until the handler returns keeps request statistics
// (recorded in serveChain's defer) ordered before the client sees EOF.
func copyBody(w http.ResponseWriter, src io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
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

// copyBodyTee copies like copyBody while capturing the bytes for usage parsing.
func copyBodyTee(w http.ResponseWriter, src io.Reader, capture io.Writer) error {
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
	// contentDeltas counts content_block_delta events (§24.8 Y6). Unlike
	// tokens (usage only, reported once at stream end) this ticks on every
	// incremental content event, so per-chunk diffs identify real content.
	contentDeltas int
	// events counts every recognised data event. contentDeltas is a subset;
	// the difference is the heartbeat/non-content count logged by §24.23 M76.
	events int
	// realContentDeltas counts only deltas that carry payload (Anthropic
	// text/thinking/input_json, non-empty; OpenAI non-empty content/reasoning/
	// tool arguments). signature_delta is excluded (§24.25 M94a). It is kept
	// separate so §24.23's contentDeltas semantics stay unchanged.
	realContentDeltas int
	// §24.26 M103/M117 response shape: thinking/tool_use block counts, the
	// highest block index seen, and which thinking blocks started with an empty
	// signature but never received a non-empty signature_delta.
	thinkingBlocks     int
	toolUseBlocks      int
	maxBlockIndex      int
	hasBlockIndex      bool
	emptyThinkingStart map[int]bool
	filledThinkingSig  map[int]bool
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
		s.events++
		s.finished = true
		return
	}
	var event struct {
		Type  string   `json:"type"`
		Index *int     `json:"index"`
		Usage sseUsage `json:"usage"`
		// Anthropic's message_start nests usage under message; the input count is
		// only reported there, while message_delta carries the final output count.
		Message struct {
			Usage sseUsage `json:"usage"`
		} `json:"message"`
		// §24.9 Z5: OpenAI chat.completion chunks carry no content_block_delta;
		// any non-empty delta field counts as one content-bearing chunk.
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			Signature   string `json:"signature"`
		} `json:"delta"`
		ContentBlock struct {
			Type      string  `json:"type"`
			Signature *string `json:"signature"`
		} `json:"content_block"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return
	}
	s.events++
	openAIContent := false
	for _, choice := range event.Choices {
		if choice.Delta.Content != "" || choice.Delta.ReasoningContent != "" {
			openAIContent = true
		}
		for _, call := range choice.Delta.ToolCalls {
			if call.Function.Arguments != "" {
				openAIContent = true
			}
		}
	}
	if tokens, known := event.Usage.inputTokens(); known {
		s.inTokens, s.hasIn = tokens, true
	}
	if tokens, known := event.Message.Usage.inputTokens(); known {
		s.inTokens, s.hasIn = tokens, true
	}
	if tokens, known := event.Usage.outputTokens(); known {
		s.tokens, s.hasTokens = tokens, true
	}
	switch event.Type {
	case "content_block_start":
		s.inBlock = true
		blockIndex := -1
		if event.Index != nil {
			blockIndex = *event.Index
			s.blockIndex = blockIndex
			if !s.hasBlockIndex || blockIndex > s.maxBlockIndex {
				s.maxBlockIndex = blockIndex
				s.hasBlockIndex = true
			}
		}
		switch event.ContentBlock.Type {
		case "thinking":
			s.thinkingBlocks++
			if event.ContentBlock.Signature == nil || *event.ContentBlock.Signature == "" {
				if s.emptyThinkingStart == nil {
					s.emptyThinkingStart = make(map[int]bool)
				}
				s.emptyThinkingStart[blockIndex] = true
			}
		case "tool_use":
			s.toolUseBlocks++
		}
	case "content_block_stop":
		s.inBlock = false
	case "message_stop":
		s.finished = true
	}
	if event.Type == "content_block_delta" && event.Delta.Type == "signature_delta" && event.Delta.Signature != "" {
		index := s.blockIndex
		if event.Index != nil {
			index = *event.Index
		}
		if s.filledThinkingSig == nil {
			s.filledThinkingSig = make(map[int]bool)
		}
		s.filledThinkingSig[index] = true
	}
	if event.Type == "content_block_delta" || openAIContent {
		// Anthropic: one per content_block_delta. OpenAI: one per chunk that
		// carries content/reasoning/tool arguments, even if several appear
		// together (§24.9 Z5).
		s.contentDeltas++
	}
	// §24.25 M94a/M71: only deltas that carry actual payload count as "has
	// content"; signature_delta and empty deltas do not.
	realContent := false
	switch event.Type {
	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta":
			realContent = event.Delta.Text != ""
		case "thinking_delta":
			realContent = event.Delta.Thinking != ""
		case "input_json_delta":
			realContent = event.Delta.PartialJSON != ""
		}
	default:
		realContent = openAIContent
	}
	if realContent {
		s.realContentDeltas++
	}
}

// signatureEmptyCount returns the number of thinking blocks whose start
// signature was empty/missing and that never received a non-empty
// signature_delta (§24.26 M117).
func (s *sseState) signatureEmptyCount() int {
	n := 0
	for index := range s.emptyThinkingStart {
		if !s.filledThinkingSig[index] {
			n++
		}
	}
	return n
}

// nextBlockIndex is the index for an injected block: the highest index seen
// plus one (§24.26 M104).
func (s *sseState) nextBlockIndex() int {
	if !s.hasBlockIndex {
		return 0
	}
	return s.maxBlockIndex + 1
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

	// §24.23 M70: the content-idle timer fired.
	contentIdle bool
	// §24.25 M94: a non-locked stream was aborted before its first real
	// content block so the request can be handed to the locked instance.
	handoff bool
	// §24.26 M103/M117: an empty thinking block was injected, and the
	// response-shape counters observed on the original bytes.
	injected       bool
	thinkingBlocks int
	toolUseBlocks  int
	signatureEmpty int
	// §24.23 M76 stream phases, all relative to the response headers.
	firstBlockDelay   time.Duration
	firstContentDelay time.Duration
	blockCount        int
	pingCount         int
	contentCount      int
	lastGap           time.Duration
}

// §24.23 M78: timer identities recorded by the stream timeout reason line.
const (
	sseTimeoutNone int32 = iota
	sseTimeoutFirstToken
	sseTimeoutIdle
	sseTimeoutContent
)

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
// to an idle timer reset on every chunk. §24.23 adds a content-idle timer: it
// starts with the first chunk and is reset only by chunks that carry content
// (the same counter the token statistics use), so heartbeats and block
// boundaries cannot keep an empty stream alive. When a timer fires the upstream
// body is closed and the outcome reports which phase timed out.
//
// It also tracks byte throughput over a sliding 10s window (§19). Once the
// window is full and the rate is below minTokensPerSecond*bytesPerToken the
// outcome reports slow; with slowInterrupt (Anthropic) the upstream body is
// closed immediately, otherwise (OpenAI) the stream keeps flowing.
//
// parse is false for bodies in an encoding the proxy cannot decode (§20.2);
// such streams are forwarded but neither usage, the byte window nor the
// content-idle timer is tracked (content cannot be recognised).
func (h *Handler) copySSE(w http.ResponseWriter, body io.ReadCloser, idle, firstToken, contentIdle time.Duration, minTokensPerSecond float64, slowInterrupt, parse bool, instName string, normalizeSignature, injectThinking, exemptLocked, handoffBeforeContent bool, commit func(), tracker *requestMonitor) (outcome sseOutcome, err error) {
	streamStart := time.Now()
	// §24.24 M81: signature normalization is Anthropic SSE only; the caller
	// passes false for OpenAI and for encodings the proxy cannot decode.
	var sigNorm *sseSignatureNormalizer
	if normalizeSignature {
		sigNorm = &sseSignatureNormalizer{}
	}
	// §24.26 M103: injection is a separate switch from normalization (M113).
	var injector *sseThinkingInjector
	if injectThinking {
		injector = &sseThinkingInjector{instName: instName}
	}
	var firstHit, idleHit, contentHit, slowHit atomic.Bool
	// timeoutReason records which timer fired first (§24.23 M78). The two idle
	// timers can both fire before the blocked read returns.
	var timeoutReason atomic.Int32

	var timerMu sync.Mutex
	var phaseTimer, contentTimer *time.Timer

	var window []byteSample
	var windowBytes int
	var firstChunkAt time.Time
	var firstContentAt time.Time
	var slowBytes float64

	defer func() {
		outcome.slow = slowHit.Load()
		outcome.slowBytesPerSecond = slowBytes
		outcome.blockCount = outcome.chunks
		outcome.contentCount = outcome.state.contentDeltas
		outcome.pingCount = outcome.state.events - outcome.state.contentDeltas
		if !outcome.firstBlockAt.IsZero() {
			outcome.firstBlockDelay = outcome.firstBlockAt.Sub(streamStart)
		}
		if !firstContentAt.IsZero() {
			outcome.firstContentDelay = firstContentAt.Sub(streamStart)
		}
		if !outcome.lastBlockAt.IsZero() {
			outcome.lastGap = time.Since(outcome.lastBlockAt)
		}
		// §24.26 M117: response shape is read from the original bytes.
		outcome.thinkingBlocks = outcome.state.thinkingBlocks
		outcome.toolUseBlocks = outcome.state.toolUseBlocks
		outcome.signatureEmpty = outcome.state.signatureEmptyCount()
	}()
	if sigNorm != nil || injector != nil {
		defer func() {
			// §24.25 M94c: a handoff aborts before any client byte, so pending
			// event bytes must not leak to the client.
			if outcome.handoff {
				return
			}
			var tail []byte
			if sigNorm != nil {
				// §24.24 M86: an incomplete trailing event is forwarded
				// verbatim, never dropped or truncated.
				tail = sigNorm.flush()
			}
			if injector != nil {
				// §24.26 M103: the final message_delta/message_stop may itself
				// be pending here, so injection still runs before it.
				tail = injector.feed(tail, &outcome.state)
				tail = append(tail, injector.flush()...)
				outcome.injected = injector.injected
			}
			if len(tail) > 0 {
				_, _ = w.Write(tail)
			}
			// §24.24 M89: at most one summary line per streaming attempt, and
			// only when something was actually rewritten.
			if sigNorm != nil && sigNorm.blocks > 0 {
				log.Printf("thinking signature normalized: blocks=%d inst=%s", sigNorm.blocks, instName)
			}
		}()
	}
	if exemptLocked {
		defer func() {
			// §24.25 M101: the pinned stream is exempt from every timeout; the
			// old content-idle line is never printed for it.
			if outcome.committed {
				log.Printf("instance %s locked; stream not interrupted", instName)
			}
		}()
	}

	markTimeout := func(reason int32) {
		timeoutReason.CompareAndSwap(sseTimeoutNone, reason)
		_ = body.Close()
	}
	// armPhase covers the first-token and idle phases; they share one slot
	// because idle only starts once the first chunk arrived.
	armPhase := func(d time.Duration, reason int32) {
		timerMu.Lock()
		defer timerMu.Unlock()
		if phaseTimer != nil {
			phaseTimer.Stop()
			phaseTimer = nil
		}
		if d <= 0 {
			return
		}
		phaseTimer = time.AfterFunc(d, func() {
			if reason == sseTimeoutFirstToken {
				firstHit.Store(true)
			} else {
				idleHit.Store(true)
			}
			markTimeout(reason)
		})
	}
	// armContent (re)starts the §24.23 M70 content-idle timer, which has its own
	// reset condition and therefore its own slot.
	armContent := func(d time.Duration) {
		timerMu.Lock()
		defer timerMu.Unlock()
		if contentTimer != nil {
			contentTimer.Stop()
			contentTimer = nil
		}
		if d <= 0 {
			return
		}
		contentTimer = time.AfterFunc(d, func() {
			contentHit.Store(true)
			markTimeout(sseTimeoutContent)
		})
	}
	defer func() {
		timerMu.Lock()
		if phaseTimer != nil {
			phaseTimer.Stop()
		}
		if contentTimer != nil {
			contentTimer.Stop()
		}
		timerMu.Unlock()
	}()
	armPhase(firstToken, sseTimeoutFirstToken)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		readStart := time.Now()
		n, err := body.Read(buf)
		if n > 0 {
			now := time.Now()
			deltaTokens, cumTokens, tokensKnown, contentDeltas := 0, 0, false, 0
			if parse {
				before := outcome.state.tokens
				beforeContent := outcome.state.contentDeltas
				outcome.state.feed(buf[:n])
				after := outcome.state.tokens
				deltaTokens = after - before
				if deltaTokens < 0 {
					deltaTokens = 0
				}
				cumTokens = after
				tokensKnown = outcome.state.hasTokens
				contentDeltas = outcome.state.contentDeltas - beforeContent
				if contentDeltas > 0 && firstContentAt.IsZero() {
					firstContentAt = now
				}
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
			// §24.25 M94b: while pinned, judge a non-locked attempt once per
			// chunk. Without a real content delta the upstream is closed before
			// commit(), so the client has seen no byte and can be rerouted.
			if handoffBeforeContent && parse && outcome.state.realContentDeltas == 0 {
				_ = body.Close()
				outcome.handoff = true
				return outcome, nil
			}
			firstChunk := outcome.chunks == 0
			if !outcome.committed {
				commit()
				outcome.committed = true
			}
			if firstChunk {
				outcome.firstBlockAt = now
			}
			outcome.lastBlockAt = now
			outcome.chunks++
			if parse && (firstChunk || contentDeltas > 0) {
				// §24.23 M70: the clock starts with the first chunk and is
				// reset only by content, so heartbeats and block boundaries
				// cannot extend an empty stream.
				armContent(contentIdle)
			}
			tracker.chunk(readStart, now, n, deltaTokens, contentDeltas, cumTokens, tokensKnown)
			// §24.24 M85: sseState.feed above sees the original bytes; only
			// this client-facing copy may be rewritten.
			forward := buf[:n]
			if sigNorm != nil {
				forward = sigNorm.feed(buf[:n])
			}
			if injector != nil {
				forward = injector.feed(forward, &outcome.state)
			}
			if len(forward) > 0 {
				if _, werr := w.Write(forward); werr != nil {
					return outcome, werr
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			armPhase(idle, sseTimeoutIdle)
		}
		if err == io.EOF {
			return outcome, nil
		}
		if err != nil {
			if firstHit.Load() && outcome.chunks == 0 {
				outcome.firstTokenTimeout = true
				return outcome, nil
			}
			// The first timer to fire decides the reason; only one can win the
			// CAS even if both close the body before the read returns.
			if outcome.committed {
				switch timeoutReason.Load() {
				case sseTimeoutContent:
					outcome.contentIdle = true
					return outcome, nil
				case sseTimeoutIdle:
					outcome.idle = true
					return outcome, nil
				case sseTimeoutNone:
					// Defensive fallback; a timer always marks its reason.
					if contentHit.Load() {
						outcome.contentIdle = true
						return outcome, nil
					}
					if idleHit.Load() {
						outcome.idle = true
						return outcome, nil
					}
				}
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

// §24.23 M76: one stage-summary line per streaming attempt, fixed field order.
// Unreached phases print "-". The line is log-only and never feeds statistics.
func logStreamPhases(instName string, hdr time.Duration, outcome sseOutcome, conn string) {
	firstBlock, firstContent, lastGap := "-", "-", "-"
	if outcome.blockCount > 0 {
		firstBlock = formatStreamPhase(outcome.firstBlockDelay)
		lastGap = formatStreamPhase(outcome.lastGap)
	}
	if outcome.contentCount > 0 {
		firstContent = formatStreamPhase(outcome.firstContentDelay)
	}
	log.Printf("stream %s hdr=%s first_block=%s first_content=%s blocks=%d pings=%d content=%d last_gap=%s conn=%s",
		instName, formatStreamPhase(hdr), firstBlock, firstContent,
		outcome.blockCount, outcome.pingCount, outcome.contentCount, lastGap, conn)
}

// formatStreamPhase renders a phase duration with the sample's precision:
// hundredths below 10s, tenths above.
func formatStreamPhase(d time.Duration) string {
	if d < 10*time.Second {
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// slowStreamState describes the §19 realtime verdict in the M78 reason line.
func slowStreamState(proto protocol) string {
	if proto == protocolAnthropic {
		return "terminated"
	}
	return "continues"
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

// serveStaticAsset emits a small immutable-in-a-day static asset.
func serveStaticAsset(w http.ResponseWriter, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(body)
}

func (s *Server) serveFavicon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	serveStaticAsset(w, "image/x-icon", web.FaviconICO)
}

// serveLogo serves the vector brand mark to anonymous requests (§24.14 L1).
// The login page shows it before any session exists, so it must not sit
// behind the admin auth gate; a <img> tag also cannot send Basic credentials.
func (s *Server) serveLogo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	serveStaticAsset(w, "image/svg+xml", web.LogoSVG)
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
	monitor    *monitorHub
	sessions   *sessionStore
	startedAt  time.Time
	// §24.31 M157: the periodic balance checks of the current state.
	balanceMu     sync.Mutex
	balanceClient *http.Client
	balances      *balancePoller
}

type serverState struct {
	cfg     *config.Config
	handler *Handler
}

// NewServer builds a Server for cfg, which must have been loaded from
// configPath so that reload reads the same file.
func NewServer(configPath string, cfg *config.Config) (*Server, error) {
	startedAt := time.Now()
	s := &Server{
		configPath: configPath,
		monitor:    newMonitorHub(),
		sessions:   newSessionStore(),
		startedAt:  startedAt,
		// §24.31 M154: the balance query has its own fixed timeout.
		balanceClient: newBalanceClient(),
	}
	s.monitor.startedAt = startedAt
	s.monitor.statsSource = s.statsSnapshot
	store := newStatsStore(configPath)
	s.monitor.stats.attachStore(store, store.load())
	st, err := buildState(cfg, s.monitor)
	if err != nil {
		return nil, err
	}
	s.state.Store(st)
	s.startBalanceChecks(st)
	return s, nil
}

// startBalanceChecks (re)starts the periodic balance checks of st (§24.31 M157).
// The first check of every instance runs right away so the state is known
// within seconds of the startup.
func (s *Server) startBalanceChecks(st *serverState) {
	s.balanceMu.Lock()
	defer s.balanceMu.Unlock()
	if s.balances != nil {
		s.balances.stop()
		s.balances = nil
	}
	var insts []*instanceRuntime
	for _, proto := range []protocol{protocolOpenAI, protocolAnthropic} {
		insts = append(insts, st.handler.byProtocol[proto]...)
	}
	s.balances = newBalancePoller(s.balanceClient, insts)
}

func buildState(cfg *config.Config, monitor *monitorHub) (*serverState, error) {
	handler, err := New(cfg)
	if err != nil {
		return nil, err
	}
	handler.monitor = monitor
	return &serverState{cfg: cfg, handler: handler}, nil
}

// StartPersistence starts the throttled stats writer (§24.10 B1). Tests that
// drive NewServer directly may leave it off; main always starts it.
func (s *Server) StartPersistence() {
	s.monitor.stats.startPersistence()
}

// Close flushes pending statistics and stops the background writer. It is
// idempotent.
func (s *Server) Close() {
	s.monitor.stats.stopPersistence()
	// §24.31 M157: the balance checks stop with the server.
	s.balanceMu.Lock()
	if s.balances != nil {
		s.balances.stop()
		s.balances = nil
	}
	s.balanceMu.Unlock()
}

func (s *Server) reload() error {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		return err
	}
	st, err := buildState(cfg, s.monitor)
	if err != nil {
		return err
	}
	s.state.Store(st)
	s.startBalanceChecks(st)
	return nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	// §24.13 K3: browsers request the favicon without credentials, so it must
	// bypass the admin auth gate entirely (never a 302 to the login page).
	if r.URL.Path == "/favicon.ico" {
		s.serveFavicon(w, r)
		return
	}
	// §24.14 L1: the left-top brand logo is an anonymous static asset for the
	// same reason — the login page renders it before authentication.
	if r.URL.Path == PathLogo {
		s.serveLogo(w, r)
		return
	}
	if isAdminPath(r.URL.Path) {
		if st.cfg.AdminPassword == "" {
			writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "not_found", "admin surface is disabled")
			return
		}
		if s.adminRequestAllowed(st.cfg, r) {
			s.serveAdmin(w, r, st)
			return
		}
		if isPageRequest(r) {
			target := PathLoginPage
			if strings.HasPrefix(r.URL.Path, "/admin") {
				target += "?next=" + url.QueryEscape(r.URL.Path)
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="model-router"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	st.handler.ServeHTTP(w, r)
}

// adminRequestAllowed applies the §24.7 D4 priority: login paths, then a live
// session cookie, then the retained Basic/query fallbacks for scripts.
func (s *Server) adminRequestAllowed(cfg *config.Config, r *http.Request) bool {
	if isLoginPath(r.URL.Path) {
		return true
	}
	if s.sessions.authorized(r) {
		return true
	}
	if adminAuthorized(cfg, r) {
		return true
	}
	return isWSPath(r.URL.Path) && monitorQueryAuthorized(cfg, r)
}

// isPageRequest decides between a login redirect (pages) and a 401 (API, WS).
func isPageRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/admin") ||
		strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/") ||
		path == "/api" || strings.HasPrefix(path, "/api/")
}

func adminAuthorized(cfg *config.Config, r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	return ok && adminCredentialsMatch(cfg, user, pass)
}

func adminCredentialsMatch(cfg *config.Config, user, pass string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(cfg.AdminUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.AdminPassword)) == 1
	return userOK && passOK
}

func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request, st *serverState) {
	switch r.URL.Path {
	case "/admin":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusFound)
	case "/admin/", "/admin/monitor", "/admin/config", "/admin/json":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(web.IndexHTML)
	case PathLoginPage:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(web.LoginHTML)
	case PathLogin:
		s.serveLogin(w, r, st.cfg)
	case PathLogout:
		s.serveLogout(w, r)
	case PathStats:
		s.serveStats(w, r)
	case PathInstancesLock:
		s.serveInstanceLock(w, r, st)
	case PathInstancesUnlock:
		s.serveInstanceUnlock(w, r, st)
	case PathInstancesBalance:
		s.serveInstanceBalance(w, r, st)
	case PathStatsClear:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.monitor.stats.clearCumulative()
		writeAdminOK(w)
	case PathWS:
		s.monitor.serveWS(w, r)
	case PathMonitorClear:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.monitor.stats.clearRequests()
		writeAdminOK(w)
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
