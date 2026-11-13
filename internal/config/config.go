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
)

// Model roles an anthropic instance can map via instances[].roles.
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
	return json.Marshal(d.Duration().String())
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
	// Instances is ordered; order is the routing priority.
	Instances []Instance `json:"instances"`
}

// Instance is one provider instance carrying a set of model mappings.
// OpenAI instances declare Models; Anthropic instances declare Roles.
type Instance struct {
	Protocol string   `json:"protocol"`
	BaseURL  string   `json:"base_url"`
	APIKey   string   `json:"api_key"`
	// Provider is an optional built-in provider id; empty means custom.
	Provider string            `json:"provider,omitempty"`
	Timeout  Duration          `json:"timeout"`
	Models   []string          `json:"models,omitempty"`
	Roles    map[string]string `json:"roles,omitempty"`
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

// Save writes cfg to path atomically (temp file + rename).
func Save(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
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

	if len(c.Instances) == 0 {
		return errors.New("instances must contain at least one instance")
	}
	for i := range c.Instances {
		if err := c.Instances[i].applyDefaults(); err != nil {
			return fmt.Errorf("instances[%d]: %w", i, err)
		}
	}
	return nil
}

func (in *Instance) applyDefaults() error {
	in.Protocol = strings.ToLower(strings.TrimSpace(in.Protocol))
	switch in.Protocol {
	case ProtocolOpenAI, ProtocolAnthropic:
	default:
		return fmt.Errorf("protocol must be %q or %q, got %q", ProtocolOpenAI, ProtocolAnthropic, in.Protocol)
	}

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
