package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
		"instances": [{
			"protocol": "OpenAI",
			"base_url": "http://127.0.0.1:9000/",
			"api_key": " sk-test ",
			"provider": " DeepSeek ",
			"models": [" gpt-a ", "", "gpt-b", "gpt-a"]
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
	if cfg.AdminUser != "admin" || cfg.AdminPassword != "" {
		t.Errorf("admin credentials = %q/%q, want admin/empty", cfg.AdminUser, cfg.AdminPassword)
	}
	if len(cfg.Instances) != 1 {
		t.Fatalf("len(instances) = %d, want 1", len(cfg.Instances))
	}
	inst := cfg.Instances[0]
	if inst.Protocol != config.ProtocolOpenAI {
		t.Errorf("protocol = %q, want normalized %q", inst.Protocol, config.ProtocolOpenAI)
	}
	if got := inst.Timeout.Duration(); got != config.DefaultTimeout {
		t.Errorf("timeout = %s, want default %s", got, config.DefaultTimeout)
	}
	if want := "http://127.0.0.1:9000"; inst.BaseURL != want {
		t.Errorf("base_url = %q, want %q", inst.BaseURL, want)
	}
	if inst.APIKey != "sk-test" {
		t.Errorf("api_key = %q, want trimmed %q", inst.APIKey, "sk-test")
	}
	if inst.Provider != "deepseek" {
		t.Errorf("provider = %q, want normalized %q", inst.Provider, "deepseek")
	}
	wantModels := []string{"gpt-a", "gpt-b"}
	if !reflect.DeepEqual(inst.Models, wantModels) {
		t.Errorf("models = %v, want %v (trimmed, de-duplicated, order kept)", inst.Models, wantModels)
	}
	if len(inst.Roles) != 0 {
		t.Errorf("roles = %v, want empty for openai instance", inst.Roles)
	}
}

func TestLoadPreservesInstanceOrder(t *testing.T) {
	path := writeConfig(t, `{
		"listen": "127.0.0.1:18080",
		"auth_token": "client-token",
		"instances": [
			{
				"protocol": "Anthropic",
				"base_url": "https://anthropic.example",
				"api_key": "sk-ant",
				"timeout": "20s",
				"roles": {"sonnet": "claude-a"}
			},
			{
				"protocol": "openai",
				"base_url": "https://openai.example",
				"api_key": "sk-oai",
				"timeout": "1m",
				"models": ["gpt-a"]
			}
		]
	}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Instances) != 2 {
		t.Fatalf("len(instances) = %d, want 2", len(cfg.Instances))
	}
	if cfg.Instances[0].Protocol != config.ProtocolAnthropic || cfg.Instances[1].Protocol != config.ProtocolOpenAI {
		t.Errorf("order not preserved: %+v", cfg.Instances)
	}
	if got := cfg.Instances[0].Timeout.Duration(); got != 20*time.Second {
		t.Errorf("instances[0] timeout = %s, want 20s", got)
	}
	if got := cfg.Instances[1].Timeout.Duration(); got != time.Minute {
		t.Errorf("instances[1] timeout = %s, want 1m", got)
	}
}

func TestLoadNormalizesAnthropicRoles(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"instances": [{
			"protocol": "anthropic",
			"base_url": "https://example.com",
			"api_key": "k",
			"roles": {" Sonnet ": " deepseek-flash ", "HAIKU": "deepseek-pro", "*": " deepseek-flash "}
		}]
	}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	want := map[string]string{
		"sonnet": "deepseek-flash",
		"haiku":  "deepseek-pro",
		"*":      "deepseek-flash",
	}
	if !reflect.DeepEqual(cfg.Instances[0].Roles, want) {
		t.Errorf("roles = %v, want %v (keys lowercased, values trimmed)", cfg.Instances[0].Roles, want)
	}
}

func TestDurationJSONRoundTrip(t *testing.T) {
	cfg, err := config.Parse([]byte(`{
		"auth_token": "token",
		"instances": [
			{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"], "timeout": "20s"},
			{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m2"]}
		]
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if !strings.Contains(string(data), `"timeout":"20s"`) {
		t.Errorf("marshaled config = %s, want explicit timeout 20s", data)
	}
	if !strings.Contains(string(data), `"timeout":"15s"`) {
		t.Errorf("marshaled config = %s, want default timeout 15s", data)
	}

	reparsed, err := config.Parse(data)
	if err != nil {
		t.Fatalf("reparse marshaled config: %v", err)
	}
	if got := reparsed.Instances[0].Timeout.Duration(); got != 20*time.Second {
		t.Errorf("round-trip timeout = %s, want 20s", got)
	}
	if got := reparsed.Instances[1].Timeout.Duration(); got != config.DefaultTimeout {
		t.Errorf("round-trip default timeout = %s, want %s", got, config.DefaultTimeout)
	}
}

func TestLoadAdminDefaults(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "token",
		"instances": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"]}]
	}`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.AdminUser != "admin" {
		t.Errorf("admin_user = %q, want %q", cfg.AdminUser, "admin")
	}
	if cfg.AdminPassword != "" {
		t.Errorf("admin_password = %q, want empty (admin disabled)", cfg.AdminPassword)
	}

	path = writeConfig(t, `{
		"auth_token": "token",
		"admin_user": "  Admin  ",
		"admin_password": "  secret  ",
		"instances": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"]}]
	}`)
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.AdminUser != "Admin" || cfg.AdminPassword != "secret" {
		t.Errorf("admin credentials = %q/%q, want Admin/secret", cfg.AdminUser, cfg.AdminPassword)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	cfg, err := config.Parse([]byte(`{
		"listen": "127.0.0.1:18080",
		"auth_token": "token",
		"admin_user": "admin",
		"admin_password": "secret",
		"instances": [
			{
				"protocol": "openai",
				"base_url": "https://api.deepseek.com",
				"api_key": "sk-test",
				"provider": "deepseek",
				"models": ["deepseek-chat", "deepseek-reasoner"],
				"timeout": "30s"
			},
			{
				"protocol": "anthropic",
				"base_url": "https://api.deepseek.com/anthropic",
				"api_key": "sk-test",
				"roles": {"sonnet": "deepseek-v4-flash", "haiku": "deepseek-v4-flash"},
				"timeout": "30s"
			}
		]
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if !reflect.DeepEqual(cfg, loaded) {
		t.Errorf("round trip mismatch:\n saved = %+v\n loaded = %+v", cfg, loaded)
	}
	if got := loaded.Instances[0].Timeout.Duration(); got != 30*time.Second {
		t.Errorf("timeout = %s, want 30s", got)
	}
}

func TestLoadProvider(t *testing.T) {
	valid := writeConfig(t, `{
		"auth_token": "token",
		"instances": [{"protocol": "anthropic", "base_url": "https://example.com", "api_key": "k", "provider": "deepseek", "roles": {"sonnet": "m"}}]
	}`)
	cfg, err := config.Load(valid)
	if err != nil {
		t.Fatalf("load config with known provider: %v", err)
	}
	if cfg.Instances[0].Provider != "deepseek" {
		t.Errorf("provider = %q, want deepseek", cfg.Instances[0].Provider)
	}

	custom := writeConfig(t, `{
		"auth_token": "token",
		"instances": [{"protocol": "anthropic", "base_url": "https://example.com", "api_key": "k", "roles": {"sonnet": "m"}}]
	}`)
	cfg, err = config.Load(custom)
	if err != nil {
		t.Fatalf("load config without provider: %v", err)
	}
	if cfg.Instances[0].Provider != "" {
		t.Errorf("provider = %q, want empty for custom instance", cfg.Instances[0].Provider)
	}

	unknown := writeConfig(t, `{
		"auth_token": "token",
		"instances": [{"protocol": "anthropic", "base_url": "https://example.com", "api_key": "k", "provider": "nope", "roles": {"sonnet": "m"}}]
	}`)
	if _, err := config.Load(unknown); err == nil {
		t.Fatal("load config succeeded with unknown provider, want error")
	}
}

func TestLoadRejectsEmptyInstances(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"instances": []
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for empty instances")
	}
}

func TestLoadRejectsMissingAuthToken(t *testing.T) {
	path := writeConfig(t, `{
		"instances": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"]}]
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for missing auth_token")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"instances": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"]}],
		"listne": "typo"
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for unknown field")
	}
}

func TestLoadRejectsLegacyBackends(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"backends": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "model": "m"}]
	}`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("load config succeeded, want error for legacy backends field")
	}
	if !strings.Contains(err.Error(), "instances") {
		t.Errorf("error = %q, want a migration hint mentioning instances", err)
	}
}

func TestLoadRejectsInvalidInstance(t *testing.T) {
	cases := map[string]string{
		"missing_protocol":       `{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}`,
		"invalid_protocol":       `{"protocol": "gemini", "base_url": "https://example.com", "api_key": "k", "models": ["m"]}`,
		"missing_base_url":       `{"protocol": "openai", "api_key": "k", "models": ["m"]}`,
		"relative_base_url":      `{"protocol": "openai", "base_url": "api.example.com", "api_key": "k", "models": ["m"]}`,
		"missing_api_key":        `{"protocol": "openai", "base_url": "https://example.com", "models": ["m"]}`,
		"openai_missing_models":  `{"protocol": "openai", "base_url": "https://example.com", "api_key": "k"}`,
		"openai_empty_models":    `{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["", "  "]}`,
		"openai_with_roles":      `{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"], "roles": {"sonnet": "x"}}`,
		"anthropic_missing_roles": `{"protocol": "anthropic", "base_url": "https://example.com", "api_key": "k"}`,
		"anthropic_with_models":  `{"protocol": "anthropic", "base_url": "https://example.com", "api_key": "k", "roles": {"sonnet": "x"}, "models": ["m"]}`,
		"anthropic_invalid_role": `{"protocol": "anthropic", "base_url": "https://example.com", "api_key": "k", "roles": {"gemini": "x"}}`,
		"anthropic_empty_model":  `{"protocol": "anthropic", "base_url": "https://example.com", "api_key": "k", "roles": {"sonnet": "  "}}`,
		"invalid_timeout":        `{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"], "timeout": "soon"}`,
		"unknown_provider":       `{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "provider": "nope", "models": ["m"]}`,
	}
	for name, instance := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{"auth_token": "client-token", "instances": [`+instance+`]}`)
			if _, err := config.Load(path); err == nil {
				t.Fatalf("load config succeeded, want error for %s", name)
			}
		})
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	data, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	if _, err := config.Parse(data); err != nil {
		t.Fatalf("example config is invalid: %v", err)
	}
}

func clearModelRouterEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		config.EnvListen,
		config.EnvAuthToken,
		config.EnvAdminUser,
		config.EnvAdminPassword,
	} {
		t.Setenv(name, "")
	}
}

func TestEnvOverridesConfig(t *testing.T) {
	clearModelRouterEnv(t)
	t.Setenv(config.EnvListen, " 0.0.0.0:9999 ")
	t.Setenv(config.EnvAuthToken, " env-token ")
	t.Setenv(config.EnvAdminUser, " ops ")
	t.Setenv(config.EnvAdminPassword, " env-secret ")

	path := writeConfig(t, `{
		"listen": "127.0.0.1:18080",
		"auth_token": "file-token",
		"admin_user": "admin",
		"admin_password": "file-secret",
		"instances": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["file-model"]}]
	}`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Listen != "0.0.0.0:9999" {
		t.Errorf("listen = %q, want env value %q", cfg.Listen, "0.0.0.0:9999")
	}
	if cfg.AuthToken != "env-token" {
		t.Errorf("auth_token = %q, want env value %q", cfg.AuthToken, "env-token")
	}
	if cfg.AdminUser != "ops" {
		t.Errorf("admin_user = %q, want env value %q", cfg.AdminUser, "ops")
	}
	if cfg.AdminPassword != "env-secret" {
		t.Errorf("admin_password = %q, want env value %q", cfg.AdminPassword, "env-secret")
	}
	if len(cfg.Instances) != 1 || !reflect.DeepEqual(cfg.Instances[0].Models, []string{"file-model"}) {
		t.Errorf("instances changed by env overrides: %+v", cfg.Instances)
	}
}

func TestEnvEmptyKeepsFileValues(t *testing.T) {
	clearModelRouterEnv(t)

	path := writeConfig(t, `{
		"listen": "127.0.0.1:18080",
		"auth_token": "file-token",
		"admin_user": "file-admin",
		"admin_password": "file-secret",
		"instances": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"]}]
	}`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Listen != "127.0.0.1:18080" || cfg.AuthToken != "file-token" {
		t.Errorf("listen/auth_token = %q/%q, want file values", cfg.Listen, cfg.AuthToken)
	}
	if cfg.AdminUser != "file-admin" || cfg.AdminPassword != "file-secret" {
		t.Errorf("admin credentials = %q/%q, want file values", cfg.AdminUser, cfg.AdminPassword)
	}
}

func TestEnvAuthTokenSatisfiesRequired(t *testing.T) {
	clearModelRouterEnv(t)

	path := writeConfig(t, `{
		"instances": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"]}]
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded without auth_token in file or environment, want fail-closed error")
	}

	t.Setenv(config.EnvAuthToken, "from-env")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config with env auth_token: %v", err)
	}
	if cfg.AuthToken != "from-env" {
		t.Errorf("auth_token = %q, want env value %q", cfg.AuthToken, "from-env")
	}
}
