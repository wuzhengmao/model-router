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
	// DefaultTimeout is the upstream timeout when the config file omits backend.timeout.
	DefaultTimeout = 15 * time.Second
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

// Config is the top-level configuration file structure.
type Config struct {
	Listen string `json:"listen"`
	// AuthToken is the Bearer token clients must present to use this proxy.
	// It is never forwarded upstream.
	AuthToken string  `json:"auth_token"`
	Backend   Backend `json:"backend"`
}

// Backend describes the single upstream model backend served in M1.
type Backend struct {
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key"`
	Model   string   `json:"model"`
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

	c.Backend.APIKey = strings.TrimSpace(c.Backend.APIKey)
	if c.Backend.APIKey == "" {
		return errors.New("backend.api_key is required")
	}

	c.Backend.BaseURL = strings.TrimRight(strings.TrimSpace(c.Backend.BaseURL), "/")
	if c.Backend.BaseURL == "" {
		return errors.New("backend.base_url is required")
	}
	u, err := url.Parse(c.Backend.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("backend.base_url must be an absolute http(s) URL, got %q", c.Backend.BaseURL)
	}
	return nil
}
