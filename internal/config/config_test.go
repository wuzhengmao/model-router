package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"model-router/internal/config"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"backends": [{
			"protocol": "OpenAI",
			"base_url": "http://127.0.0.1:9000/",
			"api_key": " sk-test ",
			"model": "gpt-test"
		}]
	}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Listen != config.DefaultListen {
		t.Errorf("listen = %q, want default %q", cfg.Listen, config.DefaultListen)
	}
	if cfg.AuthToken != "client-token" {
		t.Errorf("auth_token = %q, want %q", cfg.AuthToken, "client-token")
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("len(backends) = %d, want 1", len(cfg.Backends))
	}
	b := cfg.Backends[0]
	if b.Protocol != config.ProtocolOpenAI {
		t.Errorf("protocol = %q, want normalized %q", b.Protocol, config.ProtocolOpenAI)
	}
	if got := b.Timeout.Duration(); got != config.DefaultTimeout {
		t.Errorf("timeout = %s, want default %s", got, config.DefaultTimeout)
	}
	if want := "http://127.0.0.1:9000"; b.BaseURL != want {
		t.Errorf("base_url = %q, want %q", b.BaseURL, want)
	}
	if b.APIKey != "sk-test" || b.Model != "gpt-test" {
		t.Errorf("backend fields not preserved: %+v", b)
	}
}

func TestLoadPreservesBackendOrder(t *testing.T) {
	path := writeConfig(t, `{
		"listen": "127.0.0.1:18080",
		"auth_token": "client-token",
		"backends": [
			{"protocol": "Anthropic", "base_url": "https://anthropic.example", "api_key": "sk-ant", "model": "claude-test", "timeout": "20s"},
			{"protocol": "openai", "base_url": "https://openai.example", "api_key": "sk-oai", "model": "gpt-test", "timeout": "1m"}
		]
	}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("len(backends) = %d, want 2", len(cfg.Backends))
	}
	if cfg.Backends[0].Protocol != config.ProtocolAnthropic || cfg.Backends[1].Protocol != config.ProtocolOpenAI {
		t.Errorf("order not preserved: %+v", cfg.Backends)
	}
	if len(cfg.Backends[0].Matches) != 0 {
		t.Errorf("matches = %v, want empty when omitted", cfg.Backends[0].Matches)
	}
	if len(cfg.Backends[0].Roles) != 0 {
		t.Errorf("roles = %v, want empty when omitted", cfg.Backends[0].Roles)
	}
	if got := cfg.Backends[0].Timeout.Duration(); got != 20*time.Second {
		t.Errorf("backends[0] timeout = %s, want 20s", got)
	}
	if got := cfg.Backends[1].Timeout.Duration(); got != time.Minute {
		t.Errorf("backends[1] timeout = %s, want 1m", got)
	}
}

func TestLoadNormalizesMatches(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"backends": [{
			"protocol": "anthropic",
			"base_url": "https://api.deepseek.com/anthropic",
			"api_key": "sk-test",
			"model": "deepseek-v4-flash",
			"matches": [" claude-sonnet-4-5 ", "", "   ", "claude-sonnet-4"]
		}]
	}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	want := []string{"claude-sonnet-4-5", "claude-sonnet-4"}
	if !reflect.DeepEqual(cfg.Backends[0].Matches, want) {
		t.Errorf("matches = %v, want %v (trimmed, empty items dropped)", cfg.Backends[0].Matches, want)
	}
}

func TestLoadNormalizesRoles(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"backends": [
			{
				"protocol": "anthropic",
				"base_url": "https://api.deepseek.com/anthropic",
				"api_key": "sk-test",
				"model": "deepseek-v4-flash",
				"roles": [" Sonnet ", "HAIKU", "sonnet", "*"]
			},
			{
				"protocol": "anthropic",
				"base_url": "https://api.deepseek.com/anthropic",
				"api_key": "sk-test",
				"model": "deepseek-v4-pro",
				"roles": []
			}
		]
	}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	want := []string{"sonnet", "haiku", "sonnet", "*"}
	if !reflect.DeepEqual(cfg.Backends[0].Roles, want) {
		t.Errorf("roles = %v, want %v (trimmed, lowercased, duplicates kept)", cfg.Backends[0].Roles, want)
	}
	if len(cfg.Backends[1].Roles) != 0 {
		t.Errorf("empty roles = %v, want unconfigured", cfg.Backends[1].Roles)
	}
}

func TestLoadRejectsInvalidRole(t *testing.T) {
	cases := map[string]string{
		"unknown":     `["gemini"]`,
		"best":        `["best"]`,
		"1m_suffix":   `["sonnet[1m]"]`,
		"opusplan":    `["opusplan"]`,
		"empty":       `[""]`,
		"blank":       `["   "]`,
		"mixed_valid": `["sonnet", "gemini"]`,
	}
	for name, rolesJSON := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{
				"auth_token": "client-token",
				"backends": [{
					"protocol": "anthropic",
					"base_url": "https://example.com",
					"api_key": "sk-test",
					"model": "m",
					"roles": `+rolesJSON+`
				}]
			}`)
			if _, err := config.Load(path); err == nil {
				t.Fatalf("load config succeeded, want error for invalid roles %s", rolesJSON)
			}
		})
	}
}

func TestLoadRejectsEmptyBackends(t *testing.T) {	path := writeConfig(t, `{
		"auth_token": "client-token",
		"backends": []
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for empty backends")
	}
}

func TestLoadRejectsMissingAuthToken(t *testing.T) {
	path := writeConfig(t, `{
		"backends": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "sk-test", "model": "gpt-test"}]
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for missing auth_token")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"backends": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "sk-test", "model": "gpt-test"}],
		"backend": {"base_url": "https://example.com"}
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for unknown backend field")
	}
}

func TestLoadRejectsInvalidBackend(t *testing.T) {
	cases := map[string]string{
		"missing_protocol": `{"base_url": "https://example.com", "api_key": "sk-test", "model": "gpt-test"}`,
		"invalid_protocol": `{"protocol": "gemini", "base_url": "https://example.com", "api_key": "sk-test", "model": "gpt-test"}`,
		"missing_base_url": `{"protocol": "openai", "api_key": "sk-test", "model": "gpt-test"}`,
		"relative_base_url": `{"protocol": "openai", "base_url": "api.example.com", "api_key": "sk-test", "model": "gpt-test"}`,
		"missing_api_key":  `{"protocol": "openai", "base_url": "https://example.com", "model": "gpt-test"}`,
		"missing_model":    `{"protocol": "openai", "base_url": "https://example.com", "api_key": "sk-test"}`,
		"invalid_timeout":  `{"protocol": "openai", "base_url": "https://example.com", "api_key": "sk-test", "model": "gpt-test", "timeout": "soon"}`,
	}
	for name, backend := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{"auth_token": "client-token", "backends": [`+backend+`]}`)
			if _, err := config.Load(path); err == nil {
				t.Fatalf("load config succeeded, want error for %s", name)
			}
		})
	}
}
