package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

func chunkSnapshotKeys(t *testing.T, snapshot monitorRequestSnapshot) map[string]bool {
	t.Helper()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	list, ok := raw["chunksList"].([]any)
	if !ok {
		t.Fatalf("snapshot JSON has no chunksList array: %s", data)
	}
	if len(list) == 0 {
		return map[string]bool{}
	}
	entry, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("chunksList entry is not an object: %s", data)
	}
	keys := make(map[string]bool, len(entry))
	for key := range entry {
		keys[key] = true
	}
	return keys
}

// TestSnapshotChunksListRecordsBlocks covers §24.9 Z4: completed requests keep a
// compact per-chunk copy even when no WebSocket client is connected, and the
// JSON shape carries exactly the documented fields.
func TestSnapshotChunksListRecordsBlocks(t *testing.T) {
	hub := newMonitorHub()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	tracker := hub.trackRequest(req, protocolAnthropic, "sonnet")
	start := time.Now()
	for i := 0; i < 3; i++ {
		tracker.chunk(start.Add(time.Duration(i)*time.Millisecond),
			start.Add(time.Duration(i+1)*time.Millisecond), 10+i, i, i, i, true)
	}
	tracker.end(200, nil, attemptMetrics{})

	snapshots := hub.stats.recentRequests()
	if len(snapshots) != 1 {
		t.Fatalf("recent requests = %d, want 1", len(snapshots))
	}
	blocks := snapshots[0].ChunksList
	if len(blocks) != 3 {
		t.Fatalf("chunksList length = %d, want 3", len(blocks))
	}
	for i, block := range blocks {
		if block.Seq != i || block.Bytes != 10+i || block.ContentDeltas != i ||
			block.DeltaTokens != i || block.CumTokens != i {
			t.Errorf("block %d = %+v, want seq/bytes/content/delta/cum %d/%d/%d/%d/%d",
				i, block, i, 10+i, i, i, i)
		}
	}

	keys := chunkSnapshotKeys(t, snapshots[0])
	want := []string{"bytes", "contentDeltas", "cumTokens", "deltaTokens", "seq", "tsEnd", "tsStart"}
	got := make([]string, 0, len(keys))
	for key := range keys {
		got = append(got, key)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("chunksList keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chunksList keys = %v, want %v", got, want)
			break
		}
	}

	// A non-streaming request keeps an explicit empty array (not null).
	plain := hub.trackRequest(req, protocolAnthropic, "sonnet")
	plain.end(200, nil, attemptMetrics{})
	latest := hub.stats.recentRequests()[0]
	if latest.ChunksList == nil || len(latest.ChunksList) != 0 {
		t.Fatalf("non-streaming chunksList = %#v, want empty non-nil slice", latest.ChunksList)
	}
	if keys := chunkSnapshotKeys(t, latest); len(keys) != 0 {
		t.Errorf("non-streaming chunksList should be empty, keys = %v", keys)
	}
}

// TestSnapshotChunksListCompaction covers §24.9 Z4c: over 400 chunks keep the
// first 200 and last 199 plus one synthetic middle block; sums and the span of
// the skipped range are preserved.
func TestSnapshotChunksListCompaction(t *testing.T) {
	const total = 500
	hub := newMonitorHub()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	tracker := hub.trackRequest(req, protocolOpenAI, "gpt-x")
	start := time.Now()
	wantBytes, wantContent, wantDelta := 0, 0, 0
	for i := 0; i < total; i++ {
		bytes := 100 + i
		content := i % 2
		delta := i % 3
		wantBytes += bytes
		wantContent += content
		wantDelta += delta
		tracker.chunk(start.Add(time.Duration(i)*time.Millisecond),
			start.Add(time.Duration(i+1)*time.Millisecond), bytes, delta, content, i, true)
	}
	tracker.end(200, nil, attemptMetrics{})

	blocks := hub.stats.recentRequests()[0].ChunksList
	if len(blocks) != snapshotChunksLimit {
		t.Fatalf("compacted length = %d, want %d", len(blocks), snapshotChunksLimit)
	}

	for i := 0; i < snapshotChunksHead; i++ {
		if blocks[i].Seq != i {
			t.Fatalf("head block %d seq = %d, want %d", i, blocks[i].Seq, i)
		}
	}
	synthetic := blocks[snapshotChunksHead]
	// Skipped range is original seq [200, 300] (101 chunks); midpoint floor 250.
	if synthetic.Seq != 250 {
		t.Errorf("synthetic seq = %d, want 250", synthetic.Seq)
	}
	wantSynthStart := start.Add(200 * time.Millisecond).UnixMilli()
	wantSynthEnd := start.Add(301 * time.Millisecond).UnixMilli()
	if synthetic.TSStart != wantSynthStart || synthetic.TSEnd != wantSynthEnd {
		t.Errorf("synthetic span = [%d,%d], want [%d,%d]",
			synthetic.TSStart, synthetic.TSEnd, wantSynthStart, wantSynthEnd)
	}
	if synthetic.CumTokens != 300 {
		t.Errorf("synthetic cumTokens = %d, want 300", synthetic.CumTokens)
	}
	if blocks[snapshotChunksHead+1].Seq != 301 {
		t.Errorf("tail starts at seq %d, want 301", blocks[snapshotChunksHead+1].Seq)
	}
	if last := blocks[len(blocks)-1]; last.Seq != total-1 {
		t.Errorf("last seq = %d, want %d", last.Seq, total-1)
	}

	gotBytes, gotContent, gotDelta := 0, 0, 0
	for _, block := range blocks {
		gotBytes += block.Bytes
		gotContent += block.ContentDeltas
		gotDelta += block.DeltaTokens
	}
	if gotBytes != wantBytes || gotContent != wantContent || gotDelta != wantDelta {
		t.Errorf("sums = bytes %d/content %d/delta %d, want %d/%d/%d",
			gotBytes, gotContent, gotDelta, wantBytes, wantContent, wantDelta)
	}
}

// TestSnapshotJSONCarriesChunksList checks the wire field name used by the
// snapshot frame (§24.9 Z4/Z4b).
func TestSnapshotJSONCarriesChunksList(t *testing.T) {
	payload, err := json.Marshal(monitorSnapshotEvent{
		Type: "snapshot",
		Requests: []monitorRequestSnapshot{{
			ID:     "r1",
			TS:     1700000000000,
			Ended:  true,
			Chunks: 2,
			ChunksList: []monitorChunkSnapshot{
				{Seq: 0, TSStart: 1700000000000, TSEnd: 1700000000001, Bytes: 5, ContentDeltas: 1, CumTokens: 1},
				{Seq: 1, TSStart: 1700000000001, TSEnd: 1700000000002, Bytes: 6, ContentDeltas: 2, CumTokens: 3},
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshal snapshot event: %v", err)
	}
	var raw struct {
		Requests []struct {
			ChunksList []struct {
				Seq           int `json:"seq"`
				TSStart       int `json:"tsStart"`
				TSEnd         int `json:"tsEnd"`
				Bytes         int `json:"bytes"`
				DeltaTokens   int `json:"deltaTokens"`
				ContentDeltas int `json:"contentDeltas"`
				CumTokens     int `json:"cumTokens"`
			} `json:"chunksList"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal snapshot event: %v", err)
	}
	if len(raw.Requests) != 1 || len(raw.Requests[0].ChunksList) != 2 {
		t.Fatalf("chunksList not decoded: %s", payload)
	}
	first := raw.Requests[0].ChunksList[0]
	if first.Seq != 0 || first.Bytes != 5 || first.ContentDeltas != 1 || first.CumTokens != 1 {
		t.Errorf("first chunk = %+v", first)
	}
}
