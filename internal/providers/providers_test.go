package providers_test

import (
	"net/url"
	"reflect"
	"testing"

	"model-router/internal/providers"
)

func TestListContainsBuiltinProviders(t *testing.T) {
	list := providers.List()
	if len(list) != 12 {
		t.Fatalf("len(List()) = %d, want 12", len(list))
	}

	wantIDs := []string{
		"deepseek", "openai", "anthropic", "zhipu", "volcengine", "moonshot",
		"dashscope", "siliconflow", "groq", "mistral", "xai", "minimax",
	}
	seen := make(map[string]bool, len(list))
	for _, provider := range list {
		if provider.ID == "" || provider.Name == "" {
			t.Errorf("provider has empty id or name: %+v", provider)
		}
		if seen[provider.ID] {
			t.Errorf("duplicate provider id %q", provider.ID)
		}
		seen[provider.ID] = true
		if provider.OpenAIBaseURL == "" && provider.AnthropicBaseURL == "" {
			t.Errorf("provider %q supports no protocol", provider.ID)
		}
		for _, raw := range []string{provider.OpenAIBaseURL, provider.AnthropicBaseURL} {
			if raw == "" {
				continue
			}
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				t.Errorf("provider %q has invalid base URL %q", provider.ID, raw)
			}
		}
		for protocol, override := range provider.ModelsOverride {
			if protocol != "openai" && protocol != "anthropic" {
				t.Errorf("provider %q has override for unknown protocol %q", provider.ID, protocol)
			}
			if override.BaseURL == "" {
				t.Errorf("provider %q override %q has empty base_url", provider.ID, protocol)
			}
			if override.Auth != providers.AuthBearer && override.Auth != providers.AuthXAPIKey {
				t.Errorf("provider %q override %q has invalid auth %q", provider.ID, protocol, override.Auth)
			}
		}
	}
	for _, id := range wantIDs {
		if !seen[id] {
			t.Errorf("missing built-in provider %q", id)
		}
	}
}

func TestGet(t *testing.T) {
	provider, ok := providers.Get("deepseek")
	if !ok {
		t.Fatal("Get(deepseek) failed")
	}
	if provider.Name != "DeepSeek" {
		t.Errorf("deepseek name = %q, want DeepSeek", provider.Name)
	}
	if _, ok := providers.Get("no-such-provider"); ok {
		t.Error("Get(no-such-provider) succeeded, want miss")
	}
}

// TestMiniMaxPreset covers §24.30 M150: the domestic MiniMax preset carries
// both base URLs and needs no models_override.
func TestMiniMaxPreset(t *testing.T) {
	provider, ok := providers.Get("minimax")
	if !ok {
		t.Fatal("Get(minimax) failed")
	}
	if provider.Name != "MiniMax" {
		t.Errorf("minimax name = %q, want MiniMax", provider.Name)
	}
	if want := "https://api.minimax.cn/v1"; provider.OpenAIBaseURL != want {
		t.Errorf("minimax openai_base_url = %q, want %q", provider.OpenAIBaseURL, want)
	}
	if want := "https://api.minimax.cn/anthropic"; provider.AnthropicBaseURL != want {
		t.Errorf("minimax anthropic_base_url = %q, want %q", provider.AnthropicBaseURL, want)
	}
	if provider.ModelsOverride != nil {
		t.Errorf("minimax models_override = %+v, want none", provider.ModelsOverride)
	}
	if _, ok := providers.GetModelsOverride("minimax", "openai"); ok {
		t.Error("minimax has an openai override, want none")
	}
	if _, ok := providers.GetModelsOverride("minimax", "anthropic"); ok {
		t.Error("minimax has an anthropic override, want none")
	}
	// §24.30 M150: MiniMax has no models_override, so ModelsSource falls back
	// to the base URL the caller passes in.
	url, auth := providers.ModelsSource("minimax", "anthropic", "https://fallback.example", "bearer")
	if url != "https://fallback.example" || auth != "bearer" {
		t.Errorf("ModelsSource(minimax, anthropic) = %q/%q, want the fallback", url, auth)
	}
}

func TestDeepSeekModelsOverride(t *testing.T) {
	override, ok := providers.GetModelsOverride("deepseek", "anthropic")
	if !ok {
		t.Fatal("deepseek has no anthropic models_override")
	}
	if want := "https://api.deepseek.com"; override.BaseURL != want {
		t.Errorf("override base_url = %q, want %q", override.BaseURL, want)
	}
	if want := providers.AuthBearer; override.Auth != want {
		t.Errorf("override auth = %q, want %q", override.Auth, want)
	}
	if _, ok := providers.GetModelsOverride("deepseek", "openai"); ok {
		t.Error("deepseek has an openai override, want none")
	}
	if _, ok := providers.GetModelsOverride("openai", "anthropic"); ok {
		t.Error("openai provider has an anthropic override, want none")
	}
}

func TestModelsSource(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		protocol  string
		fallback  string
		fallbackA string
		wantURL   string
		wantAuth  string
	}{
		{
			name:      "deepseek anthropic uses override",
			provider:  "deepseek",
			protocol:  "anthropic",
			fallback:  "https://api.deepseek.com/anthropic",
			fallbackA: providers.AuthXAPIKey,
			wantURL:   "https://api.deepseek.com",
			wantAuth:  providers.AuthBearer,
		},
		{
			name:      "deepseek openai has no override",
			provider:  "deepseek",
			protocol:  "openai",
			fallback:  "https://api.deepseek.com",
			fallbackA: providers.AuthBearer,
			wantURL:   "https://api.deepseek.com",
			wantAuth:  providers.AuthBearer,
		},
		{
			name:      "custom provider falls back",
			provider:  "",
			protocol:  "anthropic",
			fallback:  "https://custom.example/anthropic",
			fallbackA: providers.AuthXAPIKey,
			wantURL:   "https://custom.example/anthropic",
			wantAuth:  providers.AuthXAPIKey,
		},
		{
			name:      "unknown provider falls back",
			provider:  "nope",
			protocol:  "anthropic",
			fallback:  "https://custom.example/anthropic",
			fallbackA: providers.AuthXAPIKey,
			wantURL:   "https://custom.example/anthropic",
			wantAuth:  providers.AuthXAPIKey,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, gotAuth := providers.ModelsSource(tc.provider, tc.protocol, tc.fallback, tc.fallbackA)
			if gotURL != tc.wantURL || gotAuth != tc.wantAuth {
				t.Errorf("ModelsSource = %q/%q, want %q/%q", gotURL, gotAuth, tc.wantURL, tc.wantAuth)
			}
		})
	}
}

func TestListReturnsCopies(t *testing.T) {
	first := providers.List()
	for i := range first {
		if first[i].ModelsOverride != nil {
			first[i].ModelsOverride["anthropic"] = providers.ModelOverride{BaseURL: "https://mutated.example", Auth: providers.AuthBearer}
		}
	}
	override, ok := providers.GetModelsOverride("deepseek", "anthropic")
	if !ok || override.BaseURL != "https://api.deepseek.com" {
		t.Errorf("mutation leaked into presets: %+v (ok=%v)", override, ok)
	}
	if !reflect.DeepEqual(providers.List(), providers.List()) {
		t.Error("List() is not deterministic")
	}
}
