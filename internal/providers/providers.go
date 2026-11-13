// Package providers ships the built-in provider presets used by model-router.
//
// The presets are static data. They never drive routing or forwarding: an
// instance's own base_url/api_key remain authoritative. The only behavioral
// hook is ModelsOverride, a generic way to say "when listing models for this
// protocol, use a different endpoint/auth scheme".
package providers

// Auth schemes usable by models_override.
const (
	AuthBearer = "bearer"
	AuthXAPIKey = "x-api-key"
)

// ModelOverride describes where and how to list models for one protocol.
type ModelOverride struct {
	BaseURL string `json:"base_url"`
	Auth    string `json:"auth"`
}

// Provider is one built-in provider preset.
type Provider struct {
	ID               string                   `json:"id"`
	Name             string                   `json:"name"`
	OpenAIBaseURL    string                   `json:"openai_base_url,omitempty"`
	AnthropicBaseURL string                   `json:"anthropic_base_url,omitempty"`
	ModelsOverride   map[string]ModelOverride `json:"models_override,omitempty"`
}

// BaseURL returns the provider's base URL for protocol, if supported.
func (p Provider) BaseURL(protocol string) (string, bool) {
	switch protocol {
	case "openai":
		return p.OpenAIBaseURL, p.OpenAIBaseURL != ""
	case "anthropic":
		return p.AnthropicBaseURL, p.AnthropicBaseURL != ""
	default:
		return "", false
	}
}

var presets = []Provider{
	{
		ID:               "deepseek",
		Name:             "DeepSeek",
		OpenAIBaseURL:    "https://api.deepseek.com",
		AnthropicBaseURL: "https://api.deepseek.com/anthropic",
		ModelsOverride: map[string]ModelOverride{
			"anthropic": {BaseURL: "https://api.deepseek.com", Auth: AuthBearer},
		},
	},
	{
		ID:            "openai",
		Name:          "OpenAI",
		OpenAIBaseURL: "https://api.openai.com/v1",
	},
	{
		ID:               "anthropic",
		Name:             "Anthropic",
		AnthropicBaseURL: "https://api.anthropic.com",
	},
	{
		ID:               "zhipu",
		Name:             "智谱 GLM",
		OpenAIBaseURL:    "https://open.bigmodel.cn/api/paas/v4",
		AnthropicBaseURL: "https://open.bigmodel.cn/api/anthropic",
	},
	{
		ID:               "volcengine",
		Name:             "火山方舟(豆包)",
		OpenAIBaseURL:    "https://ark.cn-beijing.volces.com/api/v3",
		AnthropicBaseURL: "https://ark.cn-beijing.volces.com/api/anthropic",
	},
	{
		ID:               "moonshot",
		Name:             "月之暗面 Kimi",
		OpenAIBaseURL:    "https://api.moonshot.cn/v1",
		AnthropicBaseURL: "https://api.moonshot.cn/anthropic",
	},
	{
		ID:            "dashscope",
		Name:          "通义千问",
		OpenAIBaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
	},
	{
		ID:            "siliconflow",
		Name:          "硅基流动",
		OpenAIBaseURL: "https://api.siliconflow.cn/v1",
	},
	{
		ID:            "groq",
		Name:          "Groq",
		OpenAIBaseURL: "https://api.groq.com/openai/v1",
	},
	{
		ID:            "mistral",
		Name:          "Mistral",
		OpenAIBaseURL: "https://api.mistral.ai/v1",
	},
	{
		ID:            "xai",
		Name:          "xAI Grok",
		OpenAIBaseURL: "https://api.x.ai/v1",
	},
}

func clone(p Provider) Provider {
	if p.ModelsOverride == nil {
		return p
	}
	copied := make(map[string]ModelOverride, len(p.ModelsOverride))
	for protocol, override := range p.ModelsOverride {
		copied[protocol] = override
	}
	p.ModelsOverride = copied
	return p
}

// List returns a copy of the built-in provider presets.
func List() []Provider {
	out := make([]Provider, 0, len(presets))
	for _, preset := range presets {
		out = append(out, clone(preset))
	}
	return out
}

// Get looks up a provider by id.
func Get(id string) (Provider, bool) {
	for _, preset := range presets {
		if preset.ID == id {
			return clone(preset), true
		}
	}
	return Provider{}, false
}

// GetModelsOverride returns the models_override entry for providerID+protocol.
func GetModelsOverride(providerID, protocol string) (ModelOverride, bool) {
	provider, ok := Get(providerID)
	if !ok {
		return ModelOverride{}, false
	}
	override, ok := provider.ModelsOverride[protocol]
	return override, ok
}

// ModelsSource resolves where and how to list models. A provider override wins
// over the instance's own base URL/auth; otherwise the fallback is returned.
func ModelsSource(providerID, protocol, fallbackBaseURL, fallbackAuth string) (baseURL, auth string) {
	if override, ok := GetModelsOverride(providerID, protocol); ok {
		return override.BaseURL, override.Auth
	}
	return fallbackBaseURL, fallbackAuth
}
