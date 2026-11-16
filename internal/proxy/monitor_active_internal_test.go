package proxy

import (
	"net/http/httptest"
	"testing"
	"time"
)

func newTestTracker(hub *monitorHub, instName string) *requestMonitor {
	request := httptest.NewRequest("POST", "/v1/messages", nil)
	tracker := hub.trackRequest(request, protocolAnthropic, "sonnet")
	tracker.begin(&candidate{inst: &instanceRuntime{name: instName}, model: "upstream-x"})
	return tracker
}

// TestSnapshotIncludesInProgress covers §24.11 H1: the snapshot merges ended
// and in-progress rows, sorts by ts descending, and in-progress rows carry
// ended=false plus their accumulated chunksList.
func TestSnapshotIncludesInProgress(t *testing.T) {
	hub := newMonitorHub()

	ended := newTestTracker(hub, "A")
	ended.chunk(time.Now(), time.Now(), 10, 0, 1, 0, true)
	ended.end(200, nil, attemptMetrics{})

	time.Sleep(2 * time.Millisecond)
	active := newTestTracker(hub, "B")
	active.chunk(time.Now(), time.Now(), 20, 0, 2, 3, true)
	time.Sleep(2 * time.Millisecond)

	snapshots := hub.snapshotRequests()
	if len(snapshots) != 2 {
		t.Fatalf("snapshot requests = %d, want 2", len(snapshots))
	}
	if snapshots[0].ID != active.id || snapshots[1].ID != ended.id {
		t.Fatalf("snapshot order = [%s %s], want newest (in-progress) first",
			snapshots[0].ID, snapshots[1].ID)
	}
	live := snapshots[0]
	if live.Ended {
		t.Error("in-progress row has ended=true")
	}
	if live.Chunks != 1 || len(live.ChunksList) != 1 {
		t.Errorf("in-progress chunks = %d / chunksList %d, want 1/1", live.Chunks, len(live.ChunksList))
	}
	if live.ChunksList[0].ContentDeltas != 2 || live.OutTokens != 3 {
		t.Errorf("in-progress chunk/outTokens = %+v/%d, want content=2 out=3", live.ChunksList[0], live.OutTokens)
	}
	if live.DurationMs <= 0 {
		t.Errorf("in-progress durationMs = %d, want > 0", live.DurationMs)
	}
	if !snapshots[1].Ended || len(snapshots[1].ChunksList) != 1 {
		t.Errorf("ended row = ended:%v chunks:%d, want true/1", snapshots[1].Ended, len(snapshots[1].ChunksList))
	}
}

// TestSnapshotActiveLimit covers §24.11 H1/H3e: in-progress rows are capped at
// 200 without consuming the 100 ended-request quota, dropping the oldest.
func TestSnapshotActiveLimit(t *testing.T) {
	hub := newMonitorHub()
	monitors := make([]*requestMonitor, 0, monitorActiveLimit+5)
	for i := 0; i < monitorActiveLimit+5; i++ {
		monitors = append(monitors, newTestTracker(hub, "A"))
	}
	hub.activeMu.Lock()
	registered := len(hub.active)
	hub.activeMu.Unlock()
	if registered != monitorActiveLimit {
		t.Fatalf("registered in-progress = %d, want %d", registered, monitorActiveLimit)
	}
	snapshots := hub.activeSnapshots(time.Now())
	if len(snapshots) != monitorActiveLimit {
		t.Fatalf("active snapshots = %d, want %d", len(snapshots), monitorActiveLimit)
	}
	dropped := make(map[string]bool)
	for i := 0; i < 5; i++ {
		dropped[monitors[i].id] = true
	}
	for _, snapshot := range snapshots {
		if dropped[snapshot.ID] {
			t.Errorf("oldest in-progress request %s was not dropped", snapshot.ID)
		}
	}
	if snapshots[0].ID != monitors[5].id {
		t.Errorf("first active snapshot = %s, want %s", snapshots[0].ID, monitors[5].id)
	}
}

// TestSnapshotInProgressContinuation covers §24.11 H1: a request snapshotted
// while running keeps appending chunks and then becomes an ended snapshot.
func TestSnapshotInProgressContinuation(t *testing.T) {
	hub := newMonitorHub()
	tracker := newTestTracker(hub, "A")
	tracker.chunk(time.Now(), time.Now(), 10, 0, 1, 1, true)
	tracker.chunk(time.Now(), time.Now(), 11, 0, 2, 2, true)

	before, ok := tracker.snapshotInProgress(time.Now())
	if !ok || before.Ended || before.Chunks != 2 || len(before.ChunksList) != 2 {
		t.Fatalf("in-progress snapshot = %+v (ok=%v), want ended=false chunks=2", before, ok)
	}

	tracker.chunk(time.Now(), time.Now(), 12, 0, 3, 3, true)
	tracker.end(200, nil, attemptMetrics{})
	if _, ok := tracker.snapshotInProgress(time.Now()); ok {
		t.Fatal("ended request still reports an in-progress snapshot")
	}

	ended := hub.stats.recentRequests()
	if len(ended) != 1 || !ended[0].Ended || ended[0].Chunks != 3 || len(ended[0].ChunksList) != 3 {
		t.Fatalf("ended snapshot = %+v, want chunks 3 and ended", ended)
	}
	if snapshots := hub.snapshotRequests(); len(snapshots) != 1 || !snapshots[0].Ended {
		t.Fatalf("merged snapshot = %+v, want one ended row", snapshots)
	}
}

// TestSnapshotPrefersEndedRecord covers the end/register race: an id present in
// both sets keeps the ended row only.
func TestSnapshotPrefersEndedRecord(t *testing.T) {
	hub := newMonitorHub()
	tracker := newTestTracker(hub, "A")
	tracker.mu.Lock()
	tracker.finished = false
	tracker.mu.Unlock()
	// Simulate the transient window: still registered as active but already in
	// the ended ring.
	hub.stats.recentRequests() // no-op warmup
	hub.stats.finish(monitorRequestSnapshot{ID: tracker.id, TS: tracker.start.UnixMilli(), Ended: true}, true)
	snapshots := hub.snapshotRequests()
	count := 0
	for _, snapshot := range snapshots {
		if snapshot.ID == tracker.id {
			count++
			if !snapshot.Ended {
				t.Error("duplicate id resolved to the in-progress row")
			}
		}
	}
	if count != 1 {
		t.Fatalf("id appears %d times, want 1 (ended wins)", count)
	}
}
