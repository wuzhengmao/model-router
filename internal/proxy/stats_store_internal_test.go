package proxy

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"model-router/internal/config"
)

func persistenceTestConfig(name string) *config.Config {
	return &config.Config{
		Listen:    "127.0.0.1:0",
		AuthToken: "tok",
		Instances: config.Instances{{
			Protocol: config.ProtocolOpenAI,
			Name:     name,
			BaseURL:  "https://example.com",
			APIKey:   "k",
			Models:   []string{"m"},
			Timeout:  config.Duration(5 * time.Second),
		}},
	}
}

// recordStat simulates one finished AI request directly on the aggregator.
func recordStat(hub *monitorHub, proto protocol, instName string, status int, err error, inTokens, outTokens int, retry bool) {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	tracker := hub.trackRequest(req, proto, "model-x")
	tracker.begin(&candidate{inst: &instanceRuntime{name: instName}, model: "upstream-x"})
	if retry {
		tracker.retry(&candidate{inst: &instanceRuntime{name: instName}, model: "upstream-x"}, retryReasonError)
	}
	tracker.chunk(tracker.start.Add(10*time.Millisecond), tracker.start.Add(20*time.Millisecond), 5, 0, 1, 0, true)
	tracker.chunk(tracker.start.Add(30*time.Millisecond), tracker.start.Add(120*time.Millisecond), 6, 0, 2, 2, true)
	metrics := attemptMetrics{hasIn: true, inTokens: inTokens, hasOut: true, outTokens: outTokens}
	tracker.end(status, err, metrics)
}

func readPersisted(t *testing.T, path string) persistedStats {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var persisted persistedStats
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode stats file: %v (%s)", err, data)
	}
	return persisted
}

// TestStatsPersistenceRoundTrip covers §24.10 B1: cumulative counters and recent
// errors are written atomically, reloaded on startup and the request snapshot /
// percentiles stay in memory.
func TestStatsPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	server, err := NewServer(configPath, persistenceTestConfig("A"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	recordStat(server.monitor, protocolOpenAI, "A", 200, nil, 100, 40, true)
	recordStat(server.monitor, protocolOpenAI, "B", 502, errors.New("boom"), 10, 0, false)
	server.Close()

	statsPath := filepath.Join(dir, statsFileName)
	persisted := readPersisted(t, statsPath)
	if persisted.Schema != statsSchemaVersion {
		t.Errorf("schema = %d, want %d", persisted.Schema, statsSchemaVersion)
	}
	if persisted.Requests.Total != 2 || persisted.Requests.Success != 1 || persisted.Requests.Failed != 1 {
		t.Errorf("requests = %+v, want total/success/failed 2/1/1", persisted.Requests)
	}
	if persisted.InTokens != 110 || persisted.OutTokens != 40 {
		t.Errorf("tokens = in %d / out %d, want 110/40", persisted.InTokens, persisted.OutTokens)
	}
	if persisted.Retries != 1 {
		t.Errorf("retries = %d, want 1", persisted.Retries)
	}
	if len(persisted.Instances) != 2 {
		t.Fatalf("instances = %+v, want A and B", persisted.Instances)
	}
	if len(persisted.RecentErrors) != 1 || persisted.RecentErrors[0].Error != "boom" {
		t.Errorf("recentErrors = %+v, want the failed request", persisted.RecentErrors)
	}

	reloaded, err := NewServer(configPath, persistenceTestConfig("A"))
	if err != nil {
		t.Fatalf("NewServer reload: %v", err)
	}
	defer reloaded.Close()
	snapshot := reloaded.statsSnapshot(0)
	if snapshot.Requests.Total != 2 || snapshot.Requests.Success != 1 || snapshot.Requests.Failed != 1 {
		t.Errorf("reloaded requests = %+v, want 2/1/1", snapshot.Requests)
	}
	if snapshot.InTokens != 110 || snapshot.OutTokens != 40 || snapshot.Retries != 1 {
		t.Errorf("reloaded tokens/retries = %d/%d/%d, want 110/40/1",
			snapshot.InTokens, snapshot.OutTokens, snapshot.Retries)
	}
	var removed, kept bool
	for _, inst := range snapshot.Instances {
		switch inst.Name {
		case "A":
			kept = true
			if inst.Removed {
				t.Error("instance A is configured but marked removed")
			}
		case "B":
			removed = true
			if !inst.Removed {
				t.Error("instance B is no longer configured but not marked removed")
			}
		}
	}
	if !kept || !removed {
		t.Errorf("instances = %+v, want configured A and removed B", snapshot.Instances)
	}
	if snapshot.TTFT.P50 != nil || snapshot.Duration.P95 != nil {
		t.Error("percentiles must not be restored from disk")
	}
	if got := len(reloaded.monitor.stats.recentRequests()); got != 0 {
		t.Errorf("recent request ring after reload = %d, want 0 (not persisted)", got)
	}
}

// TestStatsPersistenceCorruptFile covers §24.10 B1: a bad file is renamed to
// stats.json.bad and startup continues with zero statistics.
func TestStatsPersistenceCorruptFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"truncated json", "{"},
		{"unknown schema", `{"schema":2,"requests":{"total":9}}`},
		{"negative counter", `{"schema":1,"requests":{"total":-1}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			statsPath := filepath.Join(dir, statsFileName)
			if err := os.WriteFile(statsPath, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write stats file: %v", err)
			}
			server, err := NewServer(configPath, persistenceTestConfig("A"))
			if err != nil {
				t.Fatalf("NewServer must not fail on a bad stats file: %v", err)
			}
			defer server.Close()
			if snapshot := server.statsSnapshot(0); snapshot.Requests.Total != 0 {
				t.Errorf("total = %d, want 0", snapshot.Requests.Total)
			}
			if _, err := os.Stat(statsPath); !os.IsNotExist(err) {
				t.Errorf("stats.json still exists: %v", err)
			}
			badPath := filepath.Join(dir, statsBadFileName)
			data, err := os.ReadFile(badPath)
			if err != nil {
				t.Fatalf("read backup: %v", err)
			}
			if string(data) != tc.content {
				t.Errorf("backup = %q, want %q", data, tc.content)
			}
		})
	}
}

// TestStatsPersistenceThrottle covers the ≥5s-with-change write cadence without
// sleeping.
func TestStatsPersistenceThrottle(t *testing.T) {
	dir := t.TempDir()
	store := newStatsStore(filepath.Join(dir, "config.json"))
	agg := newStatsAggregator()
	agg.attachStore(store, persistedStats{})

	recordStat(&monitorHub{stats: agg}, protocolOpenAI, "A", 200, nil, 1, 1, false)
	agg.flush(false)
	first := readPersisted(t, store.path)
	if first.Requests.Total != 1 {
		t.Fatalf("first write total = %d, want 1", first.Requests.Total)
	}

	recordStat(&monitorHub{stats: agg}, protocolOpenAI, "A", 200, nil, 1, 1, false)
	agg.flush(false)
	if again := readPersisted(t, store.path); again.Requests.Total != 1 {
		t.Errorf("throttled write changed the file: total = %d, want 1", again.Requests.Total)
	}

	agg.mu.Lock()
	agg.lastPersist = time.Now().Add(-statsPersistPeriod - time.Second)
	agg.mu.Unlock()
	agg.flush(false)
	if after := readPersisted(t, store.path); after.Requests.Total != 2 {
		t.Errorf("write after the interval total = %d, want 2", after.Requests.Total)
	}
}

// TestStatsClearCumulative covers §24.10 B1: clear resets cumulative values and
// persists immediately while leaving the request ring alone.
func TestStatsClearCumulative(t *testing.T) {
	dir := t.TempDir()
	store := newStatsStore(filepath.Join(dir, "config.json"))
	agg := newStatsAggregator()
	agg.attachStore(store, persistedStats{})
	hub := &monitorHub{stats: agg}

	recordStat(hub, protocolOpenAI, "A", 200, nil, 10, 5, false)
	if len(agg.recentRequests()) != 1 {
		t.Fatalf("recent requests = %d, want 1", len(agg.recentRequests()))
	}

	agg.clearCumulative()

	snapshot := agg.snapshot(0, time.Now())
	if snapshot.Requests.Total != 0 || snapshot.InTokens != 0 || snapshot.OutTokens != 0 || snapshot.Retries != 0 {
		t.Errorf("cumulative values after clear = %+v", snapshot.Requests)
	}
	if len(snapshot.Instances) != 0 || len(snapshot.RecentErrors) != 0 {
		t.Errorf("instances/errors after clear = %d/%d, want 0/0",
			len(snapshot.Instances), len(snapshot.RecentErrors))
	}
	if got := len(agg.recentRequests()); got != 1 {
		t.Errorf("request ring after clear = %d, want 1 (clear must not touch it)", got)
	}
	if persisted := readPersisted(t, store.path); persisted.Requests.Total != 0 {
		t.Errorf("stats.json after clear total = %d, want 0", persisted.Requests.Total)
	}
}
