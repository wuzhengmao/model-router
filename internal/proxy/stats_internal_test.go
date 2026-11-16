package proxy

import (
	"errors"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"model-router/internal/config"
)

func TestPercentilesNeedTenSamples(t *testing.T) {
	samples := make([]int64, 0, statsMinSamples)
	for i := int64(1); i < statsMinSamples; i++ {
		samples = append(samples, i)
	}
	if got := percentilesOf(samples); got.P50 != nil || got.P95 != nil {
		t.Fatalf("percentiles with %d samples = %+v, want nulls", len(samples), got)
	}

	samples = append(samples, statsMinSamples)
	got := percentilesOf(samples)
	if got.P50 == nil || *got.P50 != 5 {
		t.Errorf("P50 = %v, want 5", got.P50)
	}
	if got.P95 == nil || *got.P95 != 10 {
		t.Errorf("P95 = %v, want 10", got.P95)
	}
}

func TestSampleRingKeepsLastWindow(t *testing.T) {
	var ring sampleRing
	for i := int64(1); i <= statsSampleWindow+500; i++ {
		ring.add(i)
	}
	values := ring.values()
	if len(values) != statsSampleWindow {
		t.Fatalf("ring length = %d, want %d", len(values), statsSampleWindow)
	}
	slices.Sort(values)
	if values[0] != 501 || values[len(values)-1] != int64(statsSampleWindow+500) {
		t.Errorf("ring window = [%d..%d], want [501..%d]", values[0], values[len(values)-1], statsSampleWindow+500)
	}
}

func TestFormatUptime(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{51*time.Hour + 14*time.Minute, "2d 3h 14m"},
		{3*time.Hour + 14*time.Minute, "3h 14m"},
		{14 * time.Minute, "14m"},
		{0, "0m"},
	}
	for _, tc := range cases {
		if got := formatUptime(tc.d); got != tc.want {
			t.Errorf("formatUptime(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                           "/admin/",
		"/admin":                     "/admin",
		"/admin/monitor":             "/admin/monitor",
		"//evil.example/admin":       "/admin/",
		"https://evil.example/admin": "/admin/",
		`/\evil.example/admin`:       "/admin/",
		"/otherapp":                  "/admin/",
	}
	for next, want := range cases {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

// TestTrackerRecordsStatsWithoutClients covers §24.7 D9: start/end are recorded
// even when no WebSocket client is connected, without broadcasting anything.
func TestTrackerRecordsStatsWithoutClients(t *testing.T) {
	hub := newMonitorHub()
	req := httptest.NewRequest("POST", "/v1/messages", nil)

	if hub.hasClients() {
		t.Fatal("fresh hub reports clients")
	}
	success := hub.trackRequest(req, protocolAnthropic, "sonnet")
	success.begin(&candidate{inst: &instanceRuntime{name: "A"}, model: "model-a"})
	success.end(200, nil, attemptMetrics{})

	failed := hub.trackRequest(req, protocolAnthropic, "sonnet")
	failed.begin(&candidate{inst: &instanceRuntime{name: "B"}, model: "model-b"})
	failed.end(200, errors.New("stream broke"), attemptMetrics{})

	snapshot := hub.stats.snapshot(0, time.Now())
	if snapshot.Requests.Total != 2 || snapshot.Requests.Success != 1 || snapshot.Requests.Failed != 1 {
		t.Errorf("stats total/success/failed = %d/%d/%d, want 2/1/1",
			snapshot.Requests.Total, snapshot.Requests.Success, snapshot.Requests.Failed)
	}
	if snapshot.Requests.SuccessRate != 50 {
		t.Errorf("successRate = %v, want 50", snapshot.Requests.SuccessRate)
	}
	if snapshot.ByProtocol[config.ProtocolAnthropic] != 2 {
		t.Errorf("byProtocol = %v, want anthropic=2", snapshot.ByProtocol)
	}
	if len(snapshot.RecentErrors) != 1 || snapshot.RecentErrors[0].Error != "stream broke" {
		t.Errorf("recentErrors = %+v, want the failed request", snapshot.RecentErrors)
	}
	if requests := hub.stats.recentRequests(); len(requests) != 2 {
		t.Errorf("recent request ring = %d, want 2", len(requests))
	}

	hub.stats.clearRequests()
	if requests := hub.stats.recentRequests(); len(requests) != 0 {
		t.Errorf("recent request ring after clear = %d, want 0", len(requests))
	}
	if snapshot := hub.stats.snapshot(0, time.Now()); snapshot.Requests.Total != 2 {
		t.Errorf("clear touched cumulative total: %d, want 2", snapshot.Requests.Total)
	}
}

func TestStatsErrorSummaryTruncated(t *testing.T) {
	hub := newMonitorHub()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	tracker := hub.trackRequest(req, protocolAnthropic, "sonnet")
	tracker.end(502, errors.New(stringsOfLength(statsErrorSummaryRune+40)), attemptMetrics{})

	snapshot := hub.stats.snapshot(0, time.Now())
	if len(snapshot.RecentErrors) != 1 {
		t.Fatalf("recentErrors = %d, want 1", len(snapshot.RecentErrors))
	}
	if got := len([]rune(snapshot.RecentErrors[0].Error)); got != statsErrorSummaryRune {
		t.Errorf("error summary length = %d runes, want %d", got, statsErrorSummaryRune)
	}
}

func stringsOfLength(n int) string {
	runes := make([]rune, n)
	for i := range runes {
		runes[i] = 'x'
	}
	return string(runes)
}
