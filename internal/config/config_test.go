package config_test

import (
	"os"
	"path/filepath"
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
		"backend": {
			"base_url": "http://127.0.0.1:9000/",
			"api_key": "sk-test",
			"model": "gpt-test"
		}
	}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Listen != config.DefaultListen {
		t.Errorf("listen = %q, want default %q", cfg.Listen, config.DefaultListen)
	}
	if got := cfg.Backend.Timeout.Duration(); got != config.DefaultTimeout {
		t.Errorf("timeout = %s, want default %s", got, config.DefaultTimeout)
	}
	if want := "http://127.0.0.1:9000"; cfg.Backend.BaseURL != want {
		t.Errorf("base_url = %q, want %q", cfg.Backend.BaseURL, want)
	}
	if cfg.AuthToken != "client-token" {
		t.Errorf("auth_token = %q, want %q", cfg.AuthToken, "client-token")
	}
	if cfg.Backend.APIKey != "sk-test" || cfg.Backend.Model != "gpt-test" {
		t.Errorf("backend fields not preserved: %+v", cfg.Backend)
	}
}

func TestLoadParsesTimeout(t *testing.T) {
	path := writeConfig(t, `{
		"listen": "127.0.0.1:18080",
		"auth_token": "client-token",
		"backend": {"base_url": "https://example.com", "api_key": "sk-test", "timeout": "20s"}
	}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := cfg.Backend.Timeout.Duration(); got != 20*time.Second {
		t.Errorf("timeout = %s, want 20s", got)
	}
}

func TestLoadRejectsMissingBaseURL(t *testing.T) {
	path := writeConfig(t, `{
		"listen": "127.0.0.1:18080",
		"auth_token": "client-token",
		"backend": {"api_key": "sk-test"}
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for missing backend.base_url")
	}
}

func TestLoadRejectsInvalidTimeout(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"backend": {"base_url": "https://example.com", "api_key": "sk-test", "timeout": "soon"}
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for invalid timeout")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"backend": {"base_url": "https://example.com", "api_key": "sk-test"},
		"listne": "typo"
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for unknown field")
	}
}

func TestLoadRejectsMissingAuthToken(t *testing.T) {
	path := writeConfig(t, `{
		"backend": {"base_url": "https://example.com", "api_key": "sk-test"}
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for missing auth_token")
	}
}

func TestLoadRejectsMissingAPIKey(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"backend": {"base_url": "https://example.com"}
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for missing backend.api_key")
	}
}
