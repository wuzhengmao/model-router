package proxy

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordTokenRequest simulates one finished AI request on the aggregator with
// explicit usage numbers. chunks==2 spans a 1s generation window; chunks==1 has
// a single trunk so genMs stays 0, like a thinking-only stream arriving in one
// read.
func recordTokenRequest(hub *monitorHub, proto protocol, instName string, status int, err error, in, out, chunks int) {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	tracker := hub.trackRequest(req, proto, "model-x")
	tracker.begin(&candidate{inst: &instanceRuntime{name: instName}, model: "upstream-x"})
	if chunks > 0 {
		first := tracker.start.Add(100 * time.Millisecond)
		tracker.chunk(tracker.start.Add(10*time.Millisecond), first, 5, 0, 1, 0, true)
		if chunks > 1 {
			tracker.chunk(tracker.start.Add(200*time.Millisecond), tracker.start.Add(1100*time.Millisecond), 6, 0, 2, 2, true)
		}
	}
	tracker.end(status, err, attemptMetrics{hasIn: true, inTokens: in, hasOut: true, outTokens: out})
}

func instanceRow(t *testing.T, snapshot statsSnapshot, proto protocol, name string) statsInstance {
	t.Helper()
	for _, row := range snapshot.Instances {
		if row.Protocol == string(proto) && row.Name == name {
			return row
		}
	}
	t.Fatalf("instance %s/%s missing from snapshot: %+v", proto, name, snapshot.Instances)
	return statsInstance{}
}

// TestStatsInstanceTokenColumns covers M15o: every finished request adds its
// full usage to the instance row, failed requests included. The JSON names are
// part of the §24.15.3 interface.
func TestStatsInstanceTokenColumns(t *testing.T) {
	hub := newMonitorHub()
	recordTokenRequest(hub, protocolAnthropic, "A", 200, nil, 100, 40, 2)
	recordTokenRequest(hub, protocolAnthropic, "A", 502, errors.New("stream broke"), 10, 5, 2)

	snapshot := hub.stats.snapshot(0, time.Now())
	row := instanceRow(t, snapshot, protocolAnthropic, "A")
	if row.Requests != 2 {
		t.Errorf("requests = %d, want 2", row.Requests)
	}
	if row.InTokens != 110 || row.OutTokens != 45 {
		t.Errorf("instance tokens = %d/%d, want 110/45", row.InTokens, row.OutTokens)
	}
	if snapshot.InTokens != 110 || snapshot.OutTokens != 45 {
		t.Errorf("aggregate tokens = %d/%d, want the same 110/45",
			snapshot.InTokens, snapshot.OutTokens)
	}

	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal instance row: %v", err)
	}
	for _, want := range []string{`"inTokens":110`, `"outTokens":45`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("instance JSON missing %s: %s", want, encoded)
		}
	}
}

// TestStatsInstanceOutTokensIncludeThinking covers M15p: the output column uses
// the full usage output (thinking included), not the genOut throughput sample.
// A thinking-only stream that arrives in a single trunk has no generation
// window, so genOut never grows while outTokens does.
func TestStatsInstanceOutTokensIncludeThinking(t *testing.T) {
	var state sseState
	state.feed([]byte("data: {\"type\":\"content_block_delta\",\"index\":0," +
		"\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":512}}\n\n"))
	if state.contentDeltas == 0 {
		t.Fatal("fixture must carry a thinking content delta")
	}
	if !state.hasTokens || state.tokens != 512 {
		t.Fatalf("thinking stream usage = %d (known=%v), want 512", state.tokens, state.hasTokens)
	}

	hub := newMonitorHub()
	// Thinking-only request: one trunk, so genMs == 0 and genOut stays untouched.
	recordTokenRequest(hub, protocolAnthropic, "A", 200, nil, 42, state.tokens, 1)
	// Normal request with a 1s generation window and 100 output tokens.
	recordTokenRequest(hub, protocolAnthropic, "A", 200, nil, 8, 100, 2)

	hub.stats.mu.Lock()
	counters := hub.stats.instances[instanceKey{protocol: string(protocolAnthropic), name: "A"}]
	if counters == nil {
		hub.stats.mu.Unlock()
		t.Fatal("instance counters missing")
	}
	outTokens, genOut := counters.outTokens, counters.genOut
	hub.stats.mu.Unlock()

	if outTokens != 612 {
		t.Errorf("instance outTokens = %d, want 612 (512 thinking + 100 normal)", outTokens)
	}
	if genOut != 100 {
		t.Errorf("instance genOut = %d, want 100 (thinking-only request has no genMs)", genOut)
	}

	row := instanceRow(t, hub.stats.snapshot(0, time.Now()), protocolAnthropic, "A")
	if row.OutTokens != 612 {
		t.Errorf("output token column = %d, want 612", row.OutTokens)
	}
	if row.AvgTPS != 100 {
		t.Errorf("avgTps = %v, want 100 (only the 100-token/1s request is sampled)", row.AvgTPS)
	}
}

// TestUsageInputTokensIncludeCache covers the M17 input rule: Anthropic splits
// input across input_tokens and the cache counters, so the full input is their
// sum; OpenAI's prompt_tokens already includes cached tokens.
func TestUsageInputTokensIncludeCache(t *testing.T) {
	usage := parseUsage(protocolAnthropic, []byte(`{"usage":{"input_tokens":100,
		"cache_creation_input_tokens":25,"cache_read_input_tokens":50,"output_tokens":7}}`))
	if !usage.hasIn || usage.in != 175 {
		t.Errorf("non-stream input = %d (known=%v), want 175", usage.in, usage.hasIn)
	}
	if !usage.hasOut || usage.out != 7 {
		t.Errorf("non-stream output = %d (known=%v), want 7", usage.out, usage.hasOut)
	}

	openai := parseUsage(protocolOpenAI, []byte(`{"usage":{"prompt_tokens":100,
		"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":50}}}`))
	if !openai.hasIn || openai.in != 100 {
		t.Errorf("OpenAI input = %d (known=%v), want 100 (cached is a subset)", openai.in, openai.hasIn)
	}

	var state sseState
	state.feed([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{" +
		"\"input_tokens\":100,\"cache_creation_input_tokens\":25,\"cache_read_input_tokens\":50}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n"))
	if !state.hasIn || state.inTokens != 175 {
		t.Errorf("stream input = %d (known=%v), want 175 from message.usage", state.inTokens, state.hasIn)
	}
	if !state.hasTokens || state.tokens != 7 {
		t.Errorf("stream output = %d (known=%v), want 7", state.tokens, state.hasTokens)
	}

	// A cache-only frame without the primary input counter must not clobber the
	// full input already learned from message_start.
	state.feed([]byte("data: {\"type\":\"message_delta\",\"usage\":{" +
		"\"cache_read_input_tokens\":999,\"output_tokens\":8}}\n\n"))
	if !state.hasIn || state.inTokens != 175 {
		t.Errorf("input after cache-only frame = %d (known=%v), want 175", state.inTokens, state.hasIn)
	}
	if !state.hasTokens || state.tokens != 8 {
		t.Errorf("output after cache-only frame = %d (known=%v), want 8", state.tokens, state.hasTokens)
	}
}

// TestStatsInstanceTokensPersistence covers M17 persistence: instance tokens
// round-trip through stats.json, and a pre-M17 schema-1 file restores the old
// counters with zero token columns.
func TestStatsInstanceTokensPersistence(t *testing.T) {
	dir := t.TempDir()
	store := newStatsStore(filepath.Join(dir, "config.json"))
	agg := newStatsAggregator()
	agg.attachStore(store, persistedStats{})

	recordTokenRequest(&monitorHub{stats: agg}, protocolOpenAI, "A", 200, nil, 7, 3, 2)
	agg.flush(true)

	persisted := readPersisted(t, store.path)
	if len(persisted.Instances) != 1 {
		t.Fatalf("persisted instances = %+v, want one", persisted.Instances)
	}
	if got := persisted.Instances[0]; got.InTokens != 7 || got.OutTokens != 3 {
		t.Errorf("persisted instance tokens = %d/%d, want 7/3", got.InTokens, got.OutTokens)
	}

	restored := newStatsAggregator()
	restored.attachStore(store, store.load())
	row := instanceRow(t, restored.snapshot(0, time.Now()), protocolOpenAI, "A")
	if row.InTokens != 7 || row.OutTokens != 3 {
		t.Errorf("restored instance tokens = %d/%d, want 7/3", row.InTokens, row.OutTokens)
	}

	// A stats.json written before M17 has no instance token fields; the file
	// must still load and keep its request counters.
	legacyPath := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacyPath, []byte(`{"schema":1,
		"requests":{"total":1,"success":1,"failed":0},
		"instances":[{"name":"A","protocol":"openai","requests":1,"success":1,"failed":0,
		"ttftCount":1,"ttftSumMs":10,"genOutTokens":2,"genMs":100}]}`), 0o600); err != nil {
		t.Fatalf("write legacy stats file: %v", err)
	}
	legacyStore := &statsStore{path: legacyPath}
	legacy := newStatsAggregator()
	legacy.attachStore(legacyStore, legacyStore.load())
	legacyRow := instanceRow(t, legacy.snapshot(0, time.Now()), protocolOpenAI, "A")
	if legacyRow.Requests != 1 || legacyRow.InTokens != 0 || legacyRow.OutTokens != 0 {
		t.Errorf("legacy row = requests %d, tokens %d/%d; want 1 and 0/0",
			legacyRow.Requests, legacyRow.InTokens, legacyRow.OutTokens)
	}
}
