package proxy

import (
	"path/filepath"
	"testing"
	"time"

	"model-router/internal/config"
)

func sortTestConfig() *config.Config {
	return &config.Config{
		AuthToken: "tok",
		Instances: config.Instances{
			{
				Protocol: config.ProtocolOpenAI,
				Name:     "keep-o",
				BaseURL:  "https://example.com",
				APIKey:   "k",
				Models:   []string{"m"},
				Timeout:  config.Duration(5 * time.Second),
			},
			{
				Protocol: config.ProtocolAnthropic,
				Name:     "keep-a",
				BaseURL:  "https://example.com",
				APIKey:   "k",
				Roles:    map[string]string{"*": "m"},
				Timeout:  config.Duration(5 * time.Second),
			},
		},
	}
}

// TestStatsInstanceOrderByProtocolAndPriority covers §24.15 M15r: rows are
// grouped by protocol (openai first), then ordered by configured priority, then
// by name; removed rows follow the same comparator instead of being appended
// after every configured row.
func TestStatsInstanceOrderByProtocolAndPriority(t *testing.T) {
	server, err := NewServer(filepath.Join(t.TempDir(), "config.json"), sortTestConfig())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)

	recordStat(server.monitor, protocolOpenAI, "zzz-gone", 200, nil, 1, 1, false)

	got := server.statsSnapshot(0).Instances
	want := []instanceKey{
		{protocol: config.ProtocolOpenAI, name: "keep-o"},
		{protocol: config.ProtocolOpenAI, name: "zzz-gone"},
		{protocol: config.ProtocolAnthropic, name: "keep-a"},
	}
	if len(got) != len(want) {
		t.Fatalf("instances = %+v, want %d rows", got, len(want))
	}
	for i, key := range want {
		if got[i].Protocol != key.protocol || got[i].Name != key.name {
			t.Errorf("instance[%d] = %s/%s, want %s/%s",
				i, got[i].Protocol, got[i].Name, key.protocol, key.name)
		}
	}
	if !got[1].Removed {
		t.Errorf("openai/zzz-gone = %+v, want removed", got[1])
	}

	// The bare aggregator snapshot (no handler) must use the same protocol
	// grouping even though "anthropic" sorts before "openai" lexicographically.
	hub := newMonitorHub()
	recordStat(hub, protocolAnthropic, "aaa", 200, nil, 1, 1, false)
	recordStat(hub, protocolOpenAI, "zzz", 200, nil, 1, 1, false)
	aggregated := hub.stats.snapshot(0, time.Now())
	if len(aggregated.Instances) != 2 ||
		aggregated.Instances[0].Protocol != config.ProtocolOpenAI ||
		aggregated.Instances[1].Protocol != config.ProtocolAnthropic {
		t.Errorf("aggregator instances = %+v, want openai before anthropic", aggregated.Instances)
	}
}
