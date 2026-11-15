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
		"instances": {
			"openai": [{
				"base_url": "http://127.0.0.1:9000/",
				"api_key": " sk-test ",
				"provider": " DeepSeek ",
				"models": [" gpt-a ", "", "gpt-b", "gpt-a"]
			}]
		}
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
	if cfg.Fallback.FailureThreshold != config.DefaultFailureThreshold {
		t.Errorf("failure_threshold = %d, want %d", cfg.Fallback.FailureThreshold, config.DefaultFailureThreshold)
	}
	if got := cfg.Fallback.DegradeDuration.Duration(); got != config.DefaultDegradeDuration {
		t.Errorf("degrade_duration = %s, want %s", got, config.DefaultDegradeDuration)
	}
	if cfg.Fallback.MinTokensPerSecond != config.DefaultMinTokensPerSecond {
		t.Errorf("min_tokens_per_second = %v, want %v", cfg.Fallback.MinTokensPerSecond, config.DefaultMinTokensPerSecond)
	}
	if got := cfg.Fallback.IdleTimeout.Duration(); got != config.DefaultIdleTimeout {
		t.Errorf("idle_timeout = %s, want %s", got, config.DefaultIdleTimeout)
	}

	if len(cfg.Instances) != 1 {
		t.Fatalf("len(instances) = %d, want 1", len(cfg.Instances))
	}
	inst := cfg.Instances[0]
	if inst.Protocol != config.ProtocolOpenAI {
		t.Errorf("protocol = %q, want %q (derived from group)", inst.Protocol, config.ProtocolOpenAI)
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
}

func TestLoadPreservesInstanceOrder(t *testing.T) {
	path := writeConfig(t, `{
		"listen": "127.0.0.1:18080",
		"auth_token": "client-token",
		"instances": {
			"openai": [
				{"base_url": "https://openai-a.example", "api_key": "k1", "timeout": "1m", "models": ["gpt-a"]},
				{"base_url": "https://openai-b.example", "api_key": "k2", "models": ["gpt-b"]}
			],
			"anthropic": [
				{"base_url": "https://anthropic.example", "api_key": "k3", "timeout": "20s", "roles": {"sonnet": "claude-a"}}
			]
		}
	}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Instances) != 3 {
		t.Fatalf("len(instances) = %d, want 3", len(cfg.Instances))
	}
	if got := cfg.Instances[0].Protocol; got != config.ProtocolOpenAI {
		t.Errorf("instances[0].protocol = %q, want openai", got)
	}
	if got := cfg.Instances[0].BaseURL; got != "https://openai-a.example" {
		t.Errorf("instances[0] = %q, want first openai instance", got)
	}
	if got := cfg.Instances[1].BaseURL; got != "https://openai-b.example" {
		t.Errorf("instances[1] = %q, want second openai instance", got)
	}
	if got := cfg.Instances[2].Protocol; got != config.ProtocolAnthropic {
		t.Errorf("instances[2].protocol = %q, want anthropic", got)
	}
	if got := cfg.Instances[0].Timeout.Duration(); got != time.Minute {
		t.Errorf("openai[0] timeout = %s, want 1m", got)
	}
	if got := cfg.Instances[2].Timeout.Duration(); got != 20*time.Second {
		t.Errorf("anthropic[0] timeout = %s, want 20s", got)
	}
}

func TestLoadNormalizesAnthropicRoles(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"instances": {
			"anthropic": [{
				"base_url": "https://example.com",
				"api_key": "k",
				"roles": {" Sonnet ": " deepseek-flash ", "HAIKU": "deepseek-pro", "*": " deepseek-flash "}
			}]
		}
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

func TestRoleNamesConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`{
		"auth_token": "t",
		"instances": {
			"anthropic": [{
				"base_url": "https://example.com",
				"api_key": "k",
				"roles": {"sonnet": "mti-pro", "haiku": "mti-flash"},
				"role_names": {" Sonnet ": " claude-sonnet-6 ", "HAIKU": "claude-haiku-5"}
			}]
		}
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	want := map[string]string{
		"sonnet": "claude-sonnet-6",
		"haiku":  "claude-haiku-5",
	}
	if got := cfg.Instances[0].RoleNames; !reflect.DeepEqual(got, want) {
		t.Errorf("role_names = %v, want %v (keys lowercased, values trimmed)", got, want)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if !strings.Contains(string(data), `"role_names":{`) {
		t.Errorf("marshalled config lost role_names: %s", data)
	}
	reparsed, err := config.Parse(data)
	if err != nil {
		t.Fatalf("reparse config: %v", err)
	}
	if got := reparsed.Instances[0].RoleNames; !reflect.DeepEqual(got, want) {
		t.Errorf("reparsed role_names = %v, want %v", got, want)
	}
}

func TestRoleNamesConfigRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"wildcard_key": `{"sonnet": "claude-sonnet-6", "*": "claude-any"}`,
		"unknown_role": `{"gemini": "claude-gemini-5"}`,
		"empty_value":  `{"sonnet": "   "}`,
	}
	for name, roleNames := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{
				"auth_token": "t",
				"instances": {"anthropic": [{
					"base_url": "https://example.com",
					"api_key": "k",
					"roles": {"sonnet": "mti-pro"},
					"role_names": `+roleNames+`
				}]}
			}`)
			if _, err := config.Load(path); err == nil {
				t.Fatalf("load config succeeded, want role_names error for %s", name)
			}
		})
	}
}

func TestModelCapabilitiesConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`{
		"auth_token": "t",
		"instances": {
			"anthropic": [{
				"base_url": "https://example.com",
				"api_key": "k",
				"roles": {"sonnet": "mti-pro", "haiku": "mti-flash"},
				"model_capabilities": {
					"mti-pro": {
						"max_input_tokens": 1000000,
						"max_tokens": 128000,
						"image_input": true,
						"pdf_input": false,
						"thinking": true,
						"structured_outputs": true
					},
					" mti-flash ": {"max_input_tokens": 0, "image_input": false}
				}
			}]
		}
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	capabilities := cfg.Instances[0].ModelCapabilities
	if len(capabilities) != 2 {
		t.Fatalf("model_capabilities = %v, want 2 entries", capabilities)
	}
	pro, ok := capabilities["mti-pro"]
	if !ok {
		t.Fatalf("model_capabilities missing mti-pro: %v", capabilities)
	}
	if pro.MaxInputTokens != 1000000 || pro.MaxTokens != 128000 {
		t.Errorf("mti-pro tokens = %d/%d, want 1000000/128000", pro.MaxInputTokens, pro.MaxTokens)
	}
	if pro.ImageInput == nil || !*pro.ImageInput {
		t.Errorf("mti-pro image_input = %v, want true", pro.ImageInput)
	}
	if pro.PDFInput == nil || *pro.PDFInput {
		t.Errorf("mti-pro pdf_input = %v, want false", pro.PDFInput)
	}
	if pro.Thinking == nil || !*pro.Thinking || pro.StructuredOutputs == nil || !*pro.StructuredOutputs {
		t.Errorf("mti-pro thinking/structured_outputs = %v/%v, want true/true", pro.Thinking, pro.StructuredOutputs)
	}
	flash, ok := capabilities["mti-flash"]
	if !ok {
		t.Fatalf("model_capabilities key was not trimmed: %v", capabilities)
	}
	if flash.MaxInputTokens != 0 || flash.ImageInput == nil || *flash.ImageInput {
		t.Errorf("mti-flash = %+v, want zero window and image_input false", flash)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	for _, want := range []string{
		`"model_capabilities":{`,
		`"max_input_tokens":1000000`,
		`"image_input":true`,
		`"pdf_input":false`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marshalled config missing %s: %s", want, data)
		}
	}
	reparsed, err := config.Parse(data)
	if err != nil {
		t.Fatalf("reparse config: %v", err)
	}
	if got := reparsed.Instances[0].ModelCapabilities; !reflect.DeepEqual(got, capabilities) {
		t.Errorf("reparsed model_capabilities = %v, want %v", got, capabilities)
	}
}

func TestModelCapabilitiesConfigRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"negative_max_input_tokens": `{"m": {"max_input_tokens": -1}}`,
		"negative_max_tokens":       `{"m": {"max_tokens": -1}}`,
		"empty_model_name":          `{"  ": {"image_input": true}}`,
	}
	for name, capabilities := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{
				"auth_token": "t",
				"instances": {"anthropic": [{
					"base_url": "https://example.com",
					"api_key": "k",
					"roles": {"sonnet": "m"},
					"model_capabilities": `+capabilities+`
				}]}
			}`)
			if _, err := config.Load(path); err == nil {
				t.Fatalf("load config succeeded, want model_capabilities error for %s", name)
			}
		})
	}
}

func TestModelCapabilitiesAllowedOnOpenAIInstance(t *testing.T) {
	cfg, err := config.Parse([]byte(`{
		"auth_token": "t",
		"instances": {
			"openai": [{
				"base_url": "https://example.com",
				"api_key": "k",
				"models": ["gpt-a"],
				"model_capabilities": {"gpt-a": {"max_tokens": 4096, "thinking": true}}
			}]
		}
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	capability := cfg.Instances[0].ModelCapabilities["gpt-a"]
	if capability.MaxTokens != 4096 || capability.Thinking == nil || !*capability.Thinking {
		t.Errorf("gpt-a capability = %+v, want max_tokens 4096 and thinking true", capability)
	}
}

func TestFallbackExplicit(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "t",
		"fallback": {
			"failure_threshold": 5,
			"degrade_duration": "1m",
			"min_tokens_per_second": 0,
			"idle_timeout": "45s"
		},
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
	}`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Fallback.FailureThreshold != 5 {
		t.Errorf("failure_threshold = %d, want 5", cfg.Fallback.FailureThreshold)
	}
	if got := cfg.Fallback.DegradeDuration.Duration(); got != time.Minute {
		t.Errorf("degrade_duration = %s, want 1m", got)
	}
	if cfg.Fallback.MinTokensPerSecond != 0 {
		t.Errorf("min_tokens_per_second = %v, want explicit 0 (disabled)", cfg.Fallback.MinTokensPerSecond)
	}
	if got := cfg.Fallback.IdleTimeout.Duration(); got != 45*time.Second {
		t.Errorf("idle_timeout = %s, want 45s", got)
	}
	values := cfg.Fallback.Values()
	if values.FailureThreshold != 5 || values.DegradeDuration != time.Minute ||
		values.MinTokensPerSecond != 0 || values.IdleTimeout != 45*time.Second {
		t.Errorf("effective fallback = %+v, want 5/1m/0/45s", values)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	for _, want := range []string{
		`"failure_threshold":5`,
		`"degrade_duration":"1m"`,
		`"min_tokens_per_second":0`,
		`"idle_timeout":"45s"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marshaled config = %s, want %s", data, want)
		}
	}
}

func TestFallbackIdleZero(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "t",
		"fallback": {"idle_timeout": "0s"},
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
	}`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Fallback.IdleTimeout != 0 {
		t.Errorf("idle_timeout = %s, want explicit 0 (disabled)", cfg.Fallback.IdleTimeout.Duration())
	}
	if got := cfg.Fallback.Values().IdleTimeout; got != 0 {
		t.Errorf("effective idle_timeout = %s, want 0", got)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if !strings.Contains(string(data), `"idle_timeout":"0s"`) {
		t.Errorf("marshaled config = %s, want idle_timeout 0s", data)
	}
}

func TestFallbackInvalid(t *testing.T) {
	cases := map[string]string{
		"threshold_zero":     `{"failure_threshold":0}`,
		"threshold_negative": `{"failure_threshold":-1}`,
		"duration_zero":      `{"degrade_duration":"0s"}`,
		"duration_negative":  `{"degrade_duration":"-5m"}`,
		"duration_invalid":   `{"degrade_duration":"soon"}`,
		"tps_negative":       `{"min_tokens_per_second":-1}`,
		"idle_negative":      `{"idle_timeout":"-5s"}`,
		"idle_invalid":       `{"idle_timeout":"abc"}`,
		"unknown_field":      `{"failure_treshold":3}`,
	}
	for name, fallback := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{
				"auth_token": "t",
				"fallback": `+fallback+`,
				"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
			}`)
			if _, err := config.Load(path); err == nil {
				t.Fatalf("load config succeeded, want error for %s", name)
			}
		})
	}
}

func TestDurationJSONRoundTrip(t *testing.T) {
	cfg, err := config.Parse([]byte(`{
		"auth_token": "token",
		"instances": {"openai": [
			{"base_url": "https://example.com", "api_key": "k", "models": ["m"], "timeout": "20s"},
			{"base_url": "https://example.com", "api_key": "k", "models": ["m2"]}
		]}
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
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
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
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
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
		"fallback": {"failure_threshold": 4, "degrade_duration": "2m", "min_tokens_per_second": 10, "idle_timeout": "10s"},
		"instances": {
			"openai": [
				{"base_url": "https://api.deepseek.com", "api_key": "sk-test", "provider": "deepseek", "models": ["deepseek-chat", "deepseek-reasoner"], "timeout": "30s"}
			],
			"anthropic": [
				{"base_url": "https://api.deepseek.com/anthropic", "api_key": "sk-test", "roles": {"sonnet": "deepseek-v4-flash", "haiku": "deepseek-v4-flash"}, "timeout": "30s"}
			]
		}
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	for _, want := range []string{`"openai":`, `"anthropic":`, `"fallback":`, `"failure_threshold": 4`, `"idle_timeout": "10s"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("saved config = %s, want %s", raw, want)
		}
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if !reflect.DeepEqual(cfg, loaded) {
		t.Errorf("round trip mismatch:\n saved = %+v\n loaded = %+v", cfg, loaded)
	}
}

func TestInstancesMarshalGrouped(t *testing.T) {
	cfg, err := config.Parse([]byte(`{
		"auth_token": "t",
		"instances": {
			"openai": [{"base_url": "https://openai.example", "api_key": "k", "models": ["m"]}],
			"anthropic": [{"base_url": "https://anthropic.example", "api_key": "k", "roles": {"sonnet": "cm"}}]
		}
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	var raw struct {
		Instances struct {
			OpenAI    []map[string]any `json:"openai"`
			Anthropic []map[string]any `json:"anthropic"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal marshaled config: %v", err)
	}
	if len(raw.Instances.OpenAI) != 1 || len(raw.Instances.Anthropic) != 1 {
		t.Fatalf("groups = %d/%d, want 1/1", len(raw.Instances.OpenAI), len(raw.Instances.Anthropic))
	}
	for _, group := range [][]map[string]any{raw.Instances.OpenAI, raw.Instances.Anthropic} {
		for _, inst := range group {
			if _, ok := inst["protocol"]; ok {
				t.Errorf("marshaled instance contains protocol: %v", inst)
			}
		}
	}
	if _, ok := raw.Instances.OpenAI[0]["models"]; !ok {
		t.Errorf("openai instance lost models: %v", raw.Instances.OpenAI[0])
	}
	if _, ok := raw.Instances.Anthropic[0]["roles"]; !ok {
		t.Errorf("anthropic instance lost roles: %v", raw.Instances.Anthropic[0])
	}
}

func TestLoadRejectsEmptyInstances(t *testing.T) {
	cases := map[string]string{
		"empty_groups": `"instances": {"openai": [], "anthropic": []}`,
		"missing":      `"instances": {}`,
	}
	for name, instances := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{"auth_token": "client-token", `+instances+`}`)
			if _, err := config.Load(path); err == nil {
				t.Fatal("load config succeeded, want error for no instances")
			}
		})
	}
}

func TestLoadRejectsMissingAuthToken(t *testing.T) {
	path := writeConfig(t, `{
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for missing auth_token")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]},
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

func TestLoadRejectsLegacyInstancesArray(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"instances": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"]}]
	}`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("load config succeeded, want error for legacy instances array")
	}
	if !strings.Contains(err.Error(), "分组结构") {
		t.Errorf("error = %q, want a grouped-instances migration hint", err)
	}
}

func TestLoadRejectsInstanceProtocolField(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "client-token",
		"instances": {"openai": [{"protocol": "openai", "base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
	}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("load config succeeded, want error for instance protocol field")
	}
}

func TestLoadRejectsInvalidInstance(t *testing.T) {
	cases := map[string]struct {
		group    string
		instance string
	}{
		"missing_base_url":        {config.ProtocolOpenAI, `{"api_key": "k", "models": ["m"]}`},
		"relative_base_url":       {config.ProtocolOpenAI, `{"base_url": "api.example.com", "api_key": "k", "models": ["m"]}`},
		"missing_api_key":         {config.ProtocolOpenAI, `{"base_url": "https://example.com", "models": ["m"]}`},
		"openai_missing_models":   {config.ProtocolOpenAI, `{"base_url": "https://example.com", "api_key": "k"}`},
		"openai_empty_models":     {config.ProtocolOpenAI, `{"base_url": "https://example.com", "api_key": "k", "models": ["", "  "]}`},
		"openai_with_roles":       {config.ProtocolOpenAI, `{"base_url": "https://example.com", "api_key": "k", "models": ["m"], "roles": {"sonnet": "x"}}`},
		"anthropic_missing_roles": {config.ProtocolAnthropic, `{"base_url": "https://example.com", "api_key": "k"}`},
		"anthropic_with_models":   {config.ProtocolAnthropic, `{"base_url": "https://example.com", "api_key": "k", "roles": {"sonnet": "x"}, "models": ["m"]}`},
		"anthropic_invalid_role":  {config.ProtocolAnthropic, `{"base_url": "https://example.com", "api_key": "k", "roles": {"gemini": "x"}}`},
		"anthropic_empty_model":   {config.ProtocolAnthropic, `{"base_url": "https://example.com", "api_key": "k", "roles": {"sonnet": "  "}}`},
		"invalid_timeout":         {config.ProtocolOpenAI, `{"base_url": "https://example.com", "api_key": "k", "models": ["m"], "timeout": "soon"}`},
		"unknown_provider":        {config.ProtocolOpenAI, `{"base_url": "https://example.com", "api_key": "k", "provider": "nope", "models": ["m"]}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var groups string
			if tc.group == config.ProtocolAnthropic {
				groups = `"anthropic": [` + tc.instance + `]`
			} else {
				groups = `"openai": [` + tc.instance + `]`
			}
			path := writeConfig(t, `{"auth_token": "client-token", "instances": {`+groups+`}}`)
			if _, err := config.Load(path); err == nil {
				t.Fatalf("load config succeeded, want error for %s", name)
			}
		})
	}
}

func TestLoadProvider(t *testing.T) {
	valid := writeConfig(t, `{
		"auth_token": "token",
		"instances": {"anthropic": [{"base_url": "https://example.com", "api_key": "k", "provider": "deepseek", "roles": {"sonnet": "m"}}]}
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
		"instances": {"anthropic": [{"base_url": "https://example.com", "api_key": "k", "roles": {"sonnet": "m"}}]}
	}`)
	cfg, err = config.Load(custom)
	if err != nil {
		t.Fatalf("load config without provider: %v", err)
	}
	if cfg.Instances[0].Provider != "" {
		t.Errorf("provider = %q, want empty for custom instance", cfg.Instances[0].Provider)
	}
}

func TestLoadInstanceName(t *testing.T) {
	path := writeConfig(t, `{
		"auth_token": "token",
		"instances": {
			"openai": [{"name": "MTI", "base_url": "https://example.com", "api_key": "k", "models": ["m"]}],
			"anthropic": [{"base_url": "https://example.com", "api_key": "k", "roles": {"sonnet": "m"}}]
		}
	}`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config with name: %v", err)
	}
	if got := cfg.Instances[0].Name; got != "MTI" {
		t.Errorf("name = %q, want MTI", got)
	}
	if got := cfg.Instances[1].Name; got != "" {
		t.Errorf("name = %q, want empty when omitted", got)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	data, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("example config is invalid: %v", err)
	}
	if len(cfg.Instances) == 0 {
		t.Fatal("example config has no instances")
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
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["file-model"]}]}
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
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
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
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
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
