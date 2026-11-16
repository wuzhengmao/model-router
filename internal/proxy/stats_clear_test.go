package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

// TestStatsClearEndpoint covers §24.10 B1: the authenticated clear resets the
// cumulative counters, persists immediately, and leaves the 100-request ring
// untouched.
func TestStatsClearEndpoint(t *testing.T) {
	backend, _, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	server, configPath := newAdminServer(t, monitorOpenAIConfig(backend.URL, serviceToken, "gpt-x"))

	resp := send(t, http.MethodPost, server.URL+"/v1/chat/completions", `{"model":"gpt-x"}`,
		withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if got := requestCounters(getStats(t, server.URL))["total"]; got != float64(1) {
		t.Fatalf("total before clear = %v, want 1", got)
	}

	unauthorized := noRedirectRequest(t, http.MethodPost, server.URL+"/api/stats/clear", "", nil)
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated clear = %d, want 401", unauthorized.StatusCode)
	}

	clear := noRedirectRequest(t, http.MethodPost, server.URL+"/api/stats/clear", "",
		basicAuthHeader("admin", "secret"))
	if clear.StatusCode != http.StatusOK {
		t.Fatalf("clear status = %d, want 200", clear.StatusCode)
	}

	stats := getStats(t, server.URL)
	if got := requestCounters(stats)["total"]; got != float64(0) {
		t.Errorf("total after clear = %v, want 0", got)
	}
	if got := stats["inTokens"]; got != float64(0) {
		t.Errorf("inTokens after clear = %v, want 0", got)
	}

	data, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "stats.json"))
	if err != nil {
		t.Fatalf("stats.json after clear: %v", err)
	}
	var persisted struct {
		Schema   int `json:"schema"`
		Requests struct {
			Total int64 `json:"total"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("stats.json is not JSON: %v (%s)", err, data)
	}
	if persisted.Schema != 1 || persisted.Requests.Total != 0 {
		t.Errorf("persisted stats = %+v, want schema 1 total 0", persisted)
	}

	client, wsResp := dialWSAt(t, server.URL, "/api/ws", "", basicAuthHeader("admin", "secret"))
	if wsResp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake = %d, want 101", wsResp.StatusCode)
	}
	snapshot, err := client.readEvent(5 * time.Second)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	requests, _ := snapshot["requests"].([]any)
	if len(requests) != 1 {
		t.Errorf("snapshot.requests after stats clear = %d, want 1 (ring must be untouched)", len(requests))
	}
}

// TestModelsOwnedByInstance covers §24.10 A2: /v1/models returns the instance
// name that first declares each model, and the set is the union in instance
// order without any Anthropic role names.
func TestModelsOwnedByInstance(t *testing.T) {
	first := openAIInstance("https://openai-a.example", "key-a", "m-alpha", "m-shared")
	first.Name = "alpha"
	second := openAIInstance("https://openai-b.example", "key-b", "m-shared", "m-beta")
	second.Name = "beta"
	server := newProxy(t, serviceToken, first, second)

	resp := send(t, http.MethodGet, server.URL+"/v1/models", "", withAuth(config.ProtocolOpenAI, nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &list); err != nil {
		t.Fatalf("models response is not JSON: %v", err)
	}
	want := map[string]string{
		"m-alpha":  "alpha",
		"m-shared": "alpha",
		"m-beta":   "beta",
	}
	if len(list.Data) != len(want) {
		t.Fatalf("models = %+v, want %d entries", list.Data, len(want))
	}
	for _, item := range list.Data {
		if strings.HasPrefix(item.ID, "claude-") {
			t.Errorf("OpenAI model list contains Anthropic role name %q", item.ID)
		}
		if owner, ok := want[item.ID]; !ok || item.OwnedBy != owner {
			t.Errorf("model %q owned_by = %q, want %q", item.ID, item.OwnedBy, want[item.ID])
		}
	}
}
