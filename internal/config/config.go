// Package config loads the static JSON configuration used by model-router.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"model-router/internal/providers"
)

const (
	// DefaultListen is used when the config file omits listen.
	DefaultListen = "127.0.0.1:8080"
	// DefaultTimeout is the upstream timeout when an instance omits timeout.
	DefaultTimeout = 15 * time.Second

	// ProtocolOpenAI marks an instance that speaks the OpenAI protocol.
	ProtocolOpenAI = "openai"
	// ProtocolAnthropic marks an instance that speaks the Anthropic protocol.
	ProtocolAnthropic = "anthropic"

	// Fallback defaults (§12.2 / §13.2).
	DefaultFailureThreshold   = 3
	DefaultDegradeDuration    = 5 * time.Minute
	DefaultMinTokensPerSecond = 30.0
	DefaultIdleTimeout        = 30 * time.Second
	DefaultFirstTokenTimeout  = 10 * time.Second
)

// Model roles an anthropic instance can map via instances.anthropic[].roles.
const (
	RoleSonnet   = "sonnet"
	RoleOpus     = "opus"
	RoleFable    = "fable"
	RoleHaiku    = "haiku"
	RoleWildcard = "*"
)

// IsValidRole reports whether role is one of the supported role values.
// Callers must pass a normalized (trimmed, lowercased) value.
func IsValidRole(role string) bool {
	switch role {
	case RoleSonnet, RoleOpus, RoleFable, RoleHaiku, RoleWildcard:
		return true
	}
	return false
}

// Environment variables that override top-level scalar settings. They are
// applied after JSON decoding and before defaults/validation.
const (
	EnvListen        = "MODEL_ROUTER_LISTEN"
	EnvAuthToken     = "MODEL_ROUTER_AUTH_TOKEN"
	EnvAdminUser     = "MODEL_ROUTER_ADMIN_USER"
	EnvAdminPassword = "MODEL_ROUTER_ADMIN_PASSWORD"
)

// Duration is a time.Duration that is written as a string such as "15s" in JSON.
type Duration time.Duration

// UnmarshalJSON parses a Go duration string ("15s", "1m30s").
func (d *Duration) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("timeout must be a duration string such as \"15s\": %w", err)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid timeout %q: %w", raw, err)
	}
	if parsed <= 0 {
		return fmt.Errorf("timeout must be positive, got %q", raw)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the configured timeout, falling back to DefaultTimeout.
func (d Duration) Duration() time.Duration {
	if d <= 0 {
		return DefaultTimeout
	}
	return time.Duration(d)
}

// MarshalJSON writes the effective duration back as a string such as "15s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(formatDuration(d.Duration()))
}

// formatDuration renders compact durations: "5m" instead of Go's "5m0s".
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// OptionalDuration is an idle-timeout duration that accepts zero (disabled)
// while rejecting negative or malformed values.
type OptionalDuration time.Duration

// UnmarshalJSON parses a Go duration string; "0s" is valid.
func (d *OptionalDuration) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("idle_timeout must be a duration string such as \"30s\": %w", err)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid idle_timeout %q: %w", raw, err)
	}
	if parsed < 0 {
		return fmt.Errorf("idle_timeout must be >= 0, got %q", raw)
	}
	*d = OptionalDuration(parsed)
	return nil
}

// Duration returns the configured value (0 = disabled).
func (d OptionalDuration) Duration() time.Duration {
	return time.Duration(d)
}

// MarshalJSON writes the value back as a string; zero is written as "0s".
func (d OptionalDuration) MarshalJSON() ([]byte, error) {
	duration := time.Duration(d)
	if duration == 0 {
		return json.Marshal("0s")
	}
	return json.Marshal(formatDuration(duration))
}

// Fallback holds the optional fallback state-machine parameters.
type Fallback struct {
	FailureThreshold   int              `json:"failure_threshold"`
	DegradeDuration    Duration         `json:"degrade_duration"`
	MinTokensPerSecond float64          `json:"min_tokens_per_second"`
	IdleTimeout        OptionalDuration `json:"idle_timeout"`
	FirstTokenTimeout  OptionalDuration `json:"first_token_timeout"`

	configured bool
}

// UnmarshalJSON applies per-field defaults and validates explicit values.
func (f *Fallback) UnmarshalJSON(data []byte) error {
	var raw struct {
		FailureThreshold   *int              `json:"failure_threshold"`
		DegradeDuration    *Duration         `json:"degrade_duration"`
		MinTokensPerSecond *float64          `json:"min_tokens_per_second"`
		IdleTimeout        *OptionalDuration `json:"idle_timeout"`
		FirstTokenTimeout  *OptionalDuration `json:"first_token_timeout"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("fallback: %w", err)
	}
	if err := dec.Decode(new(struct{})); err != io.EOF {
		return errors.New("fallback: unexpected trailing data")
	}

	*f = Fallback{
		FailureThreshold:   DefaultFailureThreshold,
		DegradeDuration:    Duration(DefaultDegradeDuration),
		MinTokensPerSecond: DefaultMinTokensPerSecond,
		IdleTimeout:        OptionalDuration(DefaultIdleTimeout),
		FirstTokenTimeout:  OptionalDuration(DefaultFirstTokenTimeout),
		configured:         true,
	}
	if raw.FailureThreshold != nil {
		if *raw.FailureThreshold < 1 {
			return fmt.Errorf("fallback.failure_threshold must be >= 1, got %d", *raw.FailureThreshold)
		}
		f.FailureThreshold = *raw.FailureThreshold
	}
	if raw.DegradeDuration != nil {
		f.DegradeDuration = *raw.DegradeDuration
	}
	if raw.MinTokensPerSecond != nil {
		if *raw.MinTokensPerSecond < 0 {
			return fmt.Errorf("fallback.min_tokens_per_second must be >= 0, got %v", *raw.MinTokensPerSecond)
		}
		f.MinTokensPerSecond = *raw.MinTokensPerSecond
	}
	if raw.IdleTimeout != nil {
		f.IdleTimeout = *raw.IdleTimeout
	}
	if raw.FirstTokenTimeout != nil {
		f.FirstTokenTimeout = *raw.FirstTokenTimeout
	}
	return nil
}

func (f *Fallback) applyDefaults() {
	if !f.configured {
		f.FailureThreshold = DefaultFailureThreshold
		f.DegradeDuration = Duration(DefaultDegradeDuration)
		f.MinTokensPerSecond = DefaultMinTokensPerSecond
		f.IdleTimeout = OptionalDuration(DefaultIdleTimeout)
		f.FirstTokenTimeout = OptionalDuration(DefaultFirstTokenTimeout)
		return
	}
	if f.FailureThreshold < 1 {
		f.FailureThreshold = DefaultFailureThreshold
	}
	if f.DegradeDuration <= 0 {
		f.DegradeDuration = Duration(DefaultDegradeDuration)
	}
	if f.MinTokensPerSecond < 0 {
		f.MinTokensPerSecond = DefaultMinTokensPerSecond
	}
	if f.IdleTimeout < 0 {
		f.IdleTimeout = OptionalDuration(DefaultIdleTimeout)
	}
	if f.FirstTokenTimeout < 0 {
		f.FirstTokenTimeout = OptionalDuration(DefaultFirstTokenTimeout)
	}
}

// FallbackValues is the effective, concrete fallback configuration.
type FallbackValues struct {
	FailureThreshold   int
	DegradeDuration    time.Duration
	MinTokensPerSecond float64
	IdleTimeout        time.Duration
	FirstTokenTimeout  time.Duration
}

// Values returns the effective fallback parameters. An explicit
// min_tokens_per_second=0 means "disable throughput judgement"; an explicit
// idle_timeout=0 means "disable idle-timeout detection". When fallback was not
// configured at all (e.g. a direct Config literal), zeros mean defaults.
func (f Fallback) Values() FallbackValues {
	values := FallbackValues{
		FailureThreshold:   f.FailureThreshold,
		DegradeDuration:    time.Duration(f.DegradeDuration),
		MinTokensPerSecond: f.MinTokensPerSecond,
		IdleTimeout:        time.Duration(f.IdleTimeout),
		FirstTokenTimeout:  time.Duration(f.FirstTokenTimeout),
	}
	if values.FailureThreshold <= 0 {
		values.FailureThreshold = DefaultFailureThreshold
	}
	if values.DegradeDuration <= 0 {
		values.DegradeDuration = DefaultDegradeDuration
	}
	if values.MinTokensPerSecond < 0 {
		values.MinTokensPerSecond = DefaultMinTokensPerSecond
	}
	if values.IdleTimeout < 0 {
		values.IdleTimeout = DefaultIdleTimeout
	}
	if values.FirstTokenTimeout < 0 {
		values.FirstTokenTimeout = DefaultFirstTokenTimeout
	}
	if !f.configured {
		if values.MinTokensPerSecond == 0 {
			values.MinTokensPerSecond = DefaultMinTokensPerSecond
		}
		if values.IdleTimeout == 0 {
			values.IdleTimeout = DefaultIdleTimeout
		}
		if values.FirstTokenTimeout == 0 {
			values.FirstTokenTimeout = DefaultFirstTokenTimeout
		}
	}
	return values
}

// Config is the top-level configuration file structure.
type Config struct {
	Listen string `json:"listen"`
	// AuthToken is the Bearer token clients must present to use this proxy.
	// It is never forwarded upstream.
	AuthToken string `json:"auth_token"`
	// AdminUser is the HTTP Basic Auth user for /admin and /api/*.
	AdminUser string `json:"admin_user"`
	// AdminPassword enables the admin surface when non-empty.
	AdminPassword string `json:"admin_password"`
	// Fallback holds optional fallback state-machine parameters.
	Fallback Fallback `json:"fallback"`
	// Instances is a flat, protocol-tagged view of the grouped JSON. The JSON
	// form is {"openai":[...],"anthropic":[...]} (see Instances).
	Instances Instances `json:"instances"`
}

// Instance is one provider instance carrying a set of model mappings.
// OpenAI instances declare Models; Anthropic instances declare Roles.
type Instance struct {
	// Protocol is derived from the JSON group and is not accepted as an
	// instance field.
	Protocol string `json:"-"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
	// Provider is an optional built-in provider id; empty means custom.
	Provider string `json:"provider,omitempty"`
	// Name is an optional display name used only in logs; it never takes part
	// in routing, matching or fallback.
	Name    string            `json:"name,omitempty"`
	Timeout Duration          `json:"timeout"`
	Models   []string          `json:"models,omitempty"`
	Roles    map[string]string `json:"roles,omitempty"`
}

// Instances is the flat runtime representation of the grouped JSON
// {"openai": [...], "anthropic": [...]}. Within-group order is preserved.
type Instances []Instance

// UnmarshalJSON decodes the grouped object into a flat protocol-tagged slice.
func (g *Instances) UnmarshalJSON(data []byte) error {
	var raw struct {
		OpenAI    []Instance `json:"openai"`
		Anthropic []Instance `json:"anthropic"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("instances: %w", err)
	}
	if err := dec.Decode(new(struct{})); err != io.EOF {
		return errors.New("instances: unexpected trailing data")
	}

	out := make(Instances, 0, len(raw.OpenAI)+len(raw.Anthropic))
	for i := range raw.OpenAI {
		inst := raw.OpenAI[i]
		inst.Protocol = ProtocolOpenAI
		out = append(out, inst)
	}
	for i := range raw.Anthropic {
		inst := raw.Anthropic[i]
		inst.Protocol = ProtocolAnthropic
		out = append(out, inst)
	}
	*g = out
	return nil
}

// MarshalJSON groups the flat instances by protocol.
func (g Instances) MarshalJSON() ([]byte, error) {
	grouped := struct {
		OpenAI    []Instance `json:"openai"`
		Anthropic []Instance `json:"anthropic"`
	}{
		OpenAI:    []Instance{},
		Anthropic: []Instance{},
	}
	for _, inst := range g {
		switch inst.Protocol {
		case ProtocolOpenAI:
			grouped.OpenAI = append(grouped.OpenAI, inst)
		case ProtocolAnthropic:
			grouped.Anthropic = append(grouped.Anthropic, inst)
		default:
			return nil, fmt.Errorf("instances: unknown protocol %q", inst.Protocol)
		}
	}
	return json.Marshal(grouped)
}

// Load reads, parses and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse parses and validates configuration bytes.
func Parse(data []byte) (*Config, error) {
	if err := rejectLegacyBackends(data); err != nil {
		return nil, err
	}
	if err := rejectLegacyInstancesArray(data); err != nil {
		return nil, err
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := dec.Decode(new(struct{})); err != io.EOF {
		return nil, errors.New("parse config: unexpected trailing data")
	}
	cfg.applyEnvOverrides()
	if err := cfg.applyDefaults(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

// rejectLegacyBackends turns the removed "backends" field into a migration hint.
func rejectLegacyBackends(data []byte) error {
	var legacy struct {
		Backends json.RawMessage `json:"backends"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil // let the strict decode report the syntax error
	}
	if legacy.Backends != nil {
		return errors.New(`config field "backends" is no longer supported; please migrate to "instances" (see docs/design.md §9.1)`)
	}
	return nil
}

// rejectLegacyInstancesArray turns the old array form into a migration hint.
func rejectLegacyInstancesArray(data []byte) error {
	var legacy struct {
		Instances json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil
	}
	trimmed := bytes.TrimSpace(legacy.Instances)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return errors.New(`instances 已改为分组结构 {openai, anthropic}，请迁移配置`)
	}
	return nil
}

// Save writes cfg to path atomically (temp file + rename).
func Save(path string, cfg *Config) error {
	normalized := *cfg
	normalized.Fallback.applyDefaults()
	data, err := json.MarshalIndent(&normalized, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".model-router-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set temp config mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config %s: %w", path, err)
	}
	return nil
}

func (c *Config) applyDefaults() error {
	c.Listen = strings.TrimSpace(c.Listen)
	if c.Listen == "" {
		c.Listen = DefaultListen
	}

	c.AuthToken = strings.TrimSpace(c.AuthToken)
	if c.AuthToken == "" {
		return errors.New("auth_token is required")
	}

	c.AdminUser = strings.TrimSpace(c.AdminUser)
	if c.AdminUser == "" {
		c.AdminUser = "admin"
	}
	c.AdminPassword = strings.TrimSpace(c.AdminPassword)

	c.Fallback.applyDefaults()

	if len(c.Instances) == 0 {
		return errors.New("instances must contain at least one instance")
	}
	openaiIndex, anthropicIndex := 0, 0
	for i := range c.Instances {
		inst := &c.Instances[i]
		proto := strings.ToLower(strings.TrimSpace(inst.Protocol))
		switch proto {
		case ProtocolOpenAI:
			inst.Protocol = proto
			if err := inst.applyDefaults(); err != nil {
				return fmt.Errorf("instances.openai[%d]: %w", openaiIndex, err)
			}
			openaiIndex++
		case ProtocolAnthropic:
			inst.Protocol = proto
			if err := inst.applyDefaults(); err != nil {
				return fmt.Errorf("instances.anthropic[%d]: %w", anthropicIndex, err)
			}
			anthropicIndex++
		default:
			return fmt.Errorf("instances: instance has no valid protocol (derived from group), got %q", inst.Protocol)
		}
	}
	return nil
}

func (in *Instance) applyDefaults() error {
	in.APIKey = strings.TrimSpace(in.APIKey)
	if in.APIKey == "" {
		return errors.New("api_key is required")
	}

	in.Provider = strings.ToLower(strings.TrimSpace(in.Provider))
	if in.Provider != "" {
		if _, ok := providers.Get(in.Provider); !ok {
			return fmt.Errorf("unknown provider %q: must be a built-in provider id or empty for custom", in.Provider)
		}
	}

	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if in.BaseURL == "" {
		return errors.New("base_url is required")
	}
	u, err := url.Parse(in.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("base_url must be an absolute http(s) URL, got %q", in.BaseURL)
	}

	models := make([]string, 0, len(in.Models))
	seen := make(map[string]bool, len(in.Models))
	for _, raw := range in.Models {
		model := strings.TrimSpace(raw)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	if len(models) == 0 {
		in.Models = nil
	} else {
		in.Models = models
	}

	roles := make(map[string]string, len(in.Roles))
	for rawRole, rawModel := range in.Roles {
		role := strings.ToLower(strings.TrimSpace(rawRole))
		if !IsValidRole(role) {
			return fmt.Errorf("invalid role %q: must be one of sonnet, opus, fable, haiku, *", rawRole)
		}
		model := strings.TrimSpace(rawModel)
		if model == "" {
			return fmt.Errorf("role %q must map to a non-empty model", role)
		}
		roles[role] = model
	}
	if len(roles) == 0 {
		in.Roles = nil
	} else {
		in.Roles = roles
	}

	switch in.Protocol {
	case ProtocolOpenAI:
		if len(in.Models) == 0 {
			return errors.New("openai instance requires at least one models entry")
		}
		if len(in.Roles) > 0 {
			return errors.New("openai instance must not define roles")
		}
	case ProtocolAnthropic:
		if len(in.Roles) == 0 {
			return errors.New("anthropic instance requires at least one roles entry")
		}
		if len(in.Models) > 0 {
			return errors.New("anthropic instance must not define models")
		}
	}
	return nil
}

// applyEnvOverrides replaces top-level scalars with non-empty environment
// values. Empty or unset variables leave the file value untouched. It runs
// before applyDefaults so overrides get the same trim/validation/default rules.
func (c *Config) applyEnvOverrides() {
	overrides := []struct {
		env  string
		dest *string
	}{
		{EnvListen, &c.Listen},
		{EnvAuthToken, &c.AuthToken},
		{EnvAdminUser, &c.AdminUser},
		{EnvAdminPassword, &c.AdminPassword},
	}
	for _, override := range overrides {
		if value := os.Getenv(override.env); value != "" {
			*override.dest = value
		}
	}
}
