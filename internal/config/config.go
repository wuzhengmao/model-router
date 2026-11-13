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
	"strings"
	"time"
)

const (
	// DefaultListen is used when the config file omits listen.
	DefaultListen = "127.0.0.1:8080"
	// DefaultTimeout is the upstream timeout when a backend omits timeout.
	DefaultTimeout = 15 * time.Second

	// ProtocolOpenAI marks a backend that speaks the OpenAI protocol.
	ProtocolOpenAI = "openai"
	// ProtocolAnthropic marks a backend that speaks the Anthropic protocol.
	ProtocolAnthropic = "anthropic"
)

// Model roles a backend can claim via backends[].roles.
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

// Config is the top-level configuration file structure.
type Config struct {
	Listen string `json:"listen"`
	// AuthToken is the Bearer token clients must present to use this proxy.
	// It is never forwarded upstream.
	AuthToken string `json:"auth_token"`
	// Backends is ordered; order within a protocol is the routing priority.
	Backends []Backend `json:"backends"`
}

// Backend describes one upstream model backend.
type Backend struct {
	Protocol string   `json:"protocol"`
	BaseURL  string   `json:"base_url"`
	APIKey   string   `json:"api_key"`
	Model    string   `json:"model"`
	// Matches lists client-facing model aliases that route to this backend.
	Matches []string `json:"matches"`
	// Roles lists model roles (sonnet/opus/fable/haiku/*) this backend serves.
	Roles   []string `json:"roles"`
	Timeout Duration `json:"timeout"`
}

// Load reads, parses and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := dec.Decode(new(struct{})); err != io.EOF {
		return nil, fmt.Errorf("parse config %s: unexpected trailing data", path)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
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

	if len(c.Backends) == 0 {
		return errors.New("backends must contain at least one backend")
	}
	for i := range c.Backends {
		if err := c.Backends[i].applyDefaults(); err != nil {
			return fmt.Errorf("backends[%d]: %w", i, err)
		}
	}
	return nil
}

func (b *Backend) applyDefaults() error {
	b.Protocol = strings.ToLower(strings.TrimSpace(b.Protocol))
	switch b.Protocol {
	case ProtocolOpenAI, ProtocolAnthropic:
	default:
		return fmt.Errorf("protocol must be %q or %q, got %q", ProtocolOpenAI, ProtocolAnthropic, b.Protocol)
	}

	b.APIKey = strings.TrimSpace(b.APIKey)
	if b.APIKey == "" {
		return errors.New("api_key is required")
	}

	b.Model = strings.TrimSpace(b.Model)
	if b.Model == "" {
		return errors.New("model is required")
	}

	matches := make([]string, 0, len(b.Matches))
	for _, alias := range b.Matches {
		if alias = strings.TrimSpace(alias); alias != "" {
			matches = append(matches, alias)
		}
	}
	if len(matches) == 0 {
		b.Matches = nil
	} else {
		b.Matches = matches
	}

	roles := make([]string, 0, len(b.Roles))
	for _, raw := range b.Roles {
		role := strings.ToLower(strings.TrimSpace(raw))
		if !IsValidRole(role) {
			return fmt.Errorf("invalid role %q: must be one of sonnet, opus, fable, haiku, *", raw)
		}
		roles = append(roles, role)
	}
	if len(roles) == 0 {
		b.Roles = nil
	} else {
		b.Roles = roles
	}

	b.BaseURL = strings.TrimRight(strings.TrimSpace(b.BaseURL), "/")
	if b.BaseURL == "" {
		return errors.New("base_url is required")
	}
	u, err := url.Parse(b.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("base_url must be an absolute http(s) URL, got %q", b.BaseURL)
	}
	return nil
}
