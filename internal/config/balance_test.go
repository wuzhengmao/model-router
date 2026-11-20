package config_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

// balanceInstanceJSON builds a config with one OpenAI instance carrying the
// given balance object.
func balanceInstanceJSON(balance string) []byte {
	return []byte(`{"auth_token":"t","instances":{"openai":[{"name":"a","base_url":"http://127.0.0.1:1","api_key":"k","models":["m"],"balance":` + balance + `}]}}`)
}

// TestBalanceObjectParsing covers §24.31 M153: the balance object is optional,
// strict about unknown fields and known kinds only.
func TestBalanceObjectParsing(t *testing.T) {
	t.Run("an instance without a balance keeps working", func(t *testing.T) {
		cfg, err := config.Parse([]byte(`{"auth_token":"t","instances":{"openai":[{"name":"a","base_url":"http://127.0.0.1:1","api_key":"k","models":["m"]}]}}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Instances[0].Balance != nil {
			t.Fatalf("balance = %+v, want none", cfg.Instances[0].Balance)
		}
	})

	t.Run("both kinds are accepted", func(t *testing.T) {
		for _, kind := range []string{config.BalanceKindMiniMaxPlan, config.BalanceKindDeepSeek} {
			cfg, err := config.Parse(balanceInstanceJSON(`{"kind":"` + kind + `"}`))
			if err != nil {
				t.Fatalf("Parse(%s): %v", kind, err)
			}
			balance := cfg.Instances[0].Balance
			if balance == nil || balance.Kind != kind {
				t.Fatalf("balance = %+v, want kind %q", balance, kind)
			}
			if balance.URL != "" {
				t.Errorf("url = %q, want empty", balance.URL)
			}
			if got := balance.PollInterval(); got != config.DefaultBalanceInterval {
				t.Errorf("PollInterval = %s, want the default %s", got, config.DefaultBalanceInterval)
			}
		}
	})

	t.Run("an unknown kind is a configuration error", func(t *testing.T) {
		_, err := config.Parse(balanceInstanceJSON(`{"kind":"openai"}`))
		if err == nil || !strings.Contains(err.Error(), "balance.kind") {
			t.Fatalf("Parse error = %v, want a balance.kind error", err)
		}
	})

	t.Run("an unknown field is a configuration error", func(t *testing.T) {
		_, err := config.Parse(balanceInstanceJSON(`{"kind":"deepseek","interval":"1m","extra":true}`))
		if err == nil || !strings.Contains(err.Error(), "extra") {
			t.Fatalf("Parse error = %v, want an unknown field error", err)
		}
	})

	t.Run("the url must be absolute", func(t *testing.T) {
		_, err := config.Parse(balanceInstanceJSON(`{"kind":"deepseek","url":"/user/balance"}`))
		if err == nil || !strings.Contains(err.Error(), "balance.url") {
			t.Fatalf("Parse error = %v, want a balance.url error", err)
		}
	})

	t.Run("the interval accepts a duration", func(t *testing.T) {
		cfg, err := config.Parse(balanceInstanceJSON(`{"kind":"deepseek","interval":"90s"}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := cfg.Instances[0].Balance.PollInterval(); got != 90*time.Second {
			t.Errorf("PollInterval = %s, want 90s", got)
		}
	})

	t.Run("a zero interval disables the poll", func(t *testing.T) {
		cfg, err := config.Parse(balanceInstanceJSON(`{"kind":"deepseek","interval":"0s"}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := cfg.Instances[0].Balance.PollInterval(); got != 0 {
			t.Errorf("PollInterval = %s, want 0", got)
		}
	})

	t.Run("the balance survives a config round trip", func(t *testing.T) {
		cfg, err := config.Parse(balanceInstanceJSON(`{"kind":"minimax_plan","url":"http://127.0.0.1:9/remains","interval":"0s"}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		again, err := config.Parse(data)
		if err != nil {
			t.Fatalf("Parse round trip: %v", err)
		}
		balance := again.Instances[0].Balance
		if balance == nil || balance.Kind != config.BalanceKindMiniMaxPlan {
			t.Fatalf("balance = %+v, want the minimax_plan kind", balance)
		}
		if balance.URL != "http://127.0.0.1:9/remains" {
			t.Errorf("url = %q, want the configured url", balance.URL)
		}
		if got := balance.PollInterval(); got != 0 {
			t.Errorf("PollInterval = %s, want 0", got)
		}
	})
}
