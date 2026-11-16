package proxy

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"model-router/internal/config"
	"model-router/internal/version"
)

// §24.7 D7: cumulative statistics are kept in memory from process start. Only
// the percentile windows are bounded (last 1000 samples), and the request ring
// holds the last 100 finished requests for the WS snapshot (D9/D10).
const (
	// PathStats exposes the same snapshot the WS stats frame carries (§24.7 D7).
	PathStats = "/api/stats"
	// PathStatsClear resets cumulative counters and persists immediately
	// without touching the recent-request ring (§24.10 B1).
	PathStatsClear = "/api/stats/clear"

	statsSampleWindow     = 1000
	statsMinSamples       = 10
	statsRecentRequests   = 100
	statsRecentErrors     = 20
	statsErrorSummaryRune = 80
	statsQPSWindow        = 60 * time.Second
)

type statsPercentiles struct {
	P50 *float64 `json:"p50"`
	P95 *float64 `json:"p95"`
}

type statsInstance struct {
	Name     string  `json:"name"`
	Requests int64   `json:"requests"`
	Percent  float64 `json:"percent"`
	Success  int64   `json:"success"`
	Failed   int64   `json:"failed"`
	AvgTTFT  float64 `json:"avgTtftMs"`
	AvgTPS   float64 `json:"avgTps"`
	Degraded bool    `json:"degraded"`
	// Removed marks persisted counters for an instance no longer configured
	// (§24.7 D11): the row stays visible with a "removed" label.
	Removed bool `json:"removed"`
}

type statsRequests struct {
	Total       int64   `json:"total"`
	Success     int64   `json:"success"`
	Failed      int64   `json:"failed"`
	SuccessRate float64 `json:"successRate"`
}

type statsError struct {
	TS            int64  `json:"ts"`
	Status        int    `json:"status"`
	InstName      string `json:"instName"`
	UpstreamModel string `json:"upstreamModel"`
	Model         string `json:"model"`
	Error         string `json:"error"`
}

// statsSnapshot is the single serialization shape shared by GET /api/stats and
// the WS {"type":"stats","data":...} frame (§24.7 D7).
type statsSnapshot struct {
	Uptime         string           `json:"uptime"`
	Version        string           `json:"version"`
	WSClients      int              `json:"wsClients"`
	ActiveRequests int64            `json:"activeRequests"`
	QPS            float64          `json:"qps"`
	Requests       statsRequests    `json:"requests"`
	ByProtocol     map[string]int64 `json:"byProtocol"`
	TTFT           statsPercentiles `json:"ttft"`
	Duration       statsPercentiles `json:"duration"`
	InTokens       int64            `json:"inTokens"`
	OutTokens      int64            `json:"outTokens"`
	AvgTPS         float64          `json:"avgTps"`
	Retries        int64            `json:"retries"`
	Instances      []statsInstance  `json:"instances"`
	RecentErrors   []statsError     `json:"recentErrors"`
}

type instanceCounters struct {
	requests  int64
	success   int64
	failure   int64
	ttftCount int64
	ttftSum   int64
	genOut    int64
	genMs     int64
}

type sampleRing struct {
	buf  []int64
	next int
}

func (r *sampleRing) add(value int64) {
	if len(r.buf) < statsSampleWindow {
		r.buf = append(r.buf, value)
		return
	}
	r.buf[r.next] = value
	r.next = (r.next + 1) % statsSampleWindow
}

func (r *sampleRing) values() []int64 {
	out := make([]int64, len(r.buf))
	copy(out, r.buf)
	return out
}

type statsAggregator struct {
	mu         sync.Mutex
	generation uint64

	active     int64
	total      int64
	success    int64
	failure    int64
	byProtocol map[string]int64
	inTokens   int64
	outTokens  int64
	genOut     int64
	genMs      int64
	retries    int64

	ttftSamples     sampleRing
	durationSamples sampleRing

	instances    map[string]*instanceCounters
	recentErrors []statsError
	requests     []monitorRequestSnapshot
	starts       []time.Time

	// Persistence state (§24.7 D11). The store is attached by NewServer; tests
	// and bare handlers may leave it nil.
	store            *statsStore
	dirty            bool
	lastPersist      time.Time
	persistStarted   atomic.Bool
	persistStop      chan struct{}
	persistDone      chan struct{}
	persistStartOnce sync.Once
	persistStopOnce  sync.Once
}

func newStatsAggregator() *statsAggregator {
	return &statsAggregator{
		byProtocol: map[string]int64{
			config.ProtocolOpenAI:    0,
			config.ProtocolAnthropic: 0,
		},
		instances:   make(map[string]*instanceCounters),
		persistStop: make(chan struct{}),
		persistDone: make(chan struct{}),
	}
}

func (a *statsAggregator) attachStore(store *statsStore, persisted persistedStats) {
	a.store = store
	a.restore(persisted)
}

func (a *statsAggregator) begin(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active++
	a.generation++
	cutoff := now.Add(-statsQPSWindow)
	kept := a.starts[:0]
	for _, ts := range a.starts {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	a.starts = append(kept, now)
}

func (a *statsAggregator) retry() {
	a.mu.Lock()
	a.retries++
	a.generation++
	a.dirty = true
	a.mu.Unlock()
}

// finish records one completed request. success is decided by the caller with
// the §24.7 D7 rule: 2xx/3xx and no error.
func (a *statsAggregator) finish(req monitorRequestSnapshot, success bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.generation++
	a.dirty = true
	if a.active > 0 {
		a.active--
	}
	a.total++
	a.byProtocol[req.Protocol]++
	if success {
		a.success++
	} else {
		a.failure++
	}
	a.ttftSamples.add(req.TTFTMs)
	a.durationSamples.add(req.DurationMs)
	a.inTokens += int64(req.InTokens)
	a.outTokens += int64(req.OutTokens)
	if req.GenMs > 0 {
		a.genMs += req.GenMs
		a.genOut += int64(req.OutTokens)
	}

	inst := a.instances[req.InstName]
	if inst == nil {
		inst = &instanceCounters{}
		a.instances[req.InstName] = inst
	}
	inst.requests++
	if success {
		inst.success++
	} else {
		inst.failure++
	}
	inst.ttftCount++
	inst.ttftSum += req.TTFTMs
	if req.GenMs > 0 {
		inst.genMs += req.GenMs
		inst.genOut += int64(req.OutTokens)
	}

	if !success {
		a.recentErrors = append([]statsError{{
			TS:            time.Now().UnixMilli(),
			Status:        req.Status,
			InstName:      req.InstName,
			UpstreamModel: req.UpstreamModel,
			Model:         req.ReqModel,
			Error:         truncateRunes(req.Error, statsErrorSummaryRune),
		}}, a.recentErrors...)
		if len(a.recentErrors) > statsRecentErrors {
			a.recentErrors = a.recentErrors[:statsRecentErrors]
		}
	}

	if len(a.requests) < statsRecentRequests {
		a.requests = append(a.requests, req)
		return
	}
	copy(a.requests, a.requests[1:])
	a.requests[len(a.requests)-1] = req
}

func (a *statsAggregator) generationValue() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.generation
}

// recentRequests returns the ring buffer newest first (§24.7 D9).
func (a *statsAggregator) recentRequests() []monitorRequestSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]monitorRequestSnapshot, 0, len(a.requests))
	for i := len(a.requests) - 1; i >= 0; i-- {
		out = append(out, a.requests[i])
	}
	return out
}

// clearRequests empties the recent-request ring only; cumulative counters stay
// intact (§24.7 D10).
func (a *statsAggregator) clearRequests() {
	a.mu.Lock()
	a.requests = nil
	a.mu.Unlock()
}

func (a *statsAggregator) snapshot(wsClients int, startedAt time.Time) statsSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-statsQPSWindow)
	first := 0
	for first < len(a.starts) && !a.starts[first].After(cutoff) {
		first++
	}
	if first > 0 {
		a.starts = append([]time.Time(nil), a.starts[first:]...)
	}

	byProtocol := make(map[string]int64, len(a.byProtocol))
	for proto, count := range a.byProtocol {
		byProtocol[proto] = count
	}

	instances := make([]statsInstance, 0, len(a.instances))
	for name, counters := range a.instances {
		item := statsInstance{
			Name:     name,
			Requests: counters.requests,
			Success:  counters.success,
			Failed:   counters.failure,
		}
		if a.total > 0 {
			item.Percent = round1(float64(counters.requests) * 100 / float64(a.total))
		}
		if counters.ttftCount > 0 {
			item.AvgTTFT = round1(float64(counters.ttftSum) / float64(counters.ttftCount))
		}
		if counters.genMs > 0 {
			item.AvgTPS = round1(float64(counters.genOut) * 1000 / float64(counters.genMs))
		}
		instances = append(instances, item)
	}
	slices.SortFunc(instances, func(x, y statsInstance) int { return cmp.Compare(x.Name, y.Name) })

	recentErrors := make([]statsError, len(a.recentErrors))
	copy(recentErrors, a.recentErrors)

	snapshot := statsSnapshot{
		Uptime:         formatUptime(now.Sub(startedAt)),
		Version:        version.Get(),
		WSClients:      wsClients,
		ActiveRequests: a.active,
		QPS:            round1(float64(len(a.starts)) / statsQPSWindow.Seconds()),
		Requests: statsRequests{
			Total:   a.total,
			Success: a.success,
			Failed:  a.failure,
		},
		ByProtocol:   byProtocol,
		TTFT:         percentilesOf(a.ttftSamples.values()),
		Duration:     percentilesOf(a.durationSamples.values()),
		InTokens:     a.inTokens,
		OutTokens:    a.outTokens,
		Retries:      a.retries,
		Instances:    instances,
		RecentErrors: recentErrors,
	}
	if a.total > 0 {
		snapshot.Requests.SuccessRate = round1(float64(a.success) * 100 / float64(a.total))
	}
	if a.genMs > 0 {
		snapshot.AvgTPS = round1(float64(a.genOut) * 1000 / float64(a.genMs))
	}
	return snapshot
}

// percentilesOf uses nearest-rank. Fewer than 10 samples report null, which the
// frontend renders as "–" (§24.7 D7).
func percentilesOf(samples []int64) statsPercentiles {
	if len(samples) < statsMinSamples {
		return statsPercentiles{}
	}
	slices.Sort(samples)
	p50 := nearestRank(samples, 0.50)
	p95 := nearestRank(samples, 0.95)
	return statsPercentiles{P50: &p50, P95: &p95}
}

func nearestRank(sorted []int64, p float64) float64 {
	index := int(math.Ceil(p*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return float64(sorted[index])
}

func round1(value float64) float64 {
	return math.Round(value*10) / 10
}

func formatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int64(d / (24 * time.Hour))
	hours := int64(d/time.Hour) % 24
	minutes := int64(d/time.Minute) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// statsSnapshot builds the shared snapshot, filling the live degraded state
// from the currently active handler.
func (s *Server) statsSnapshot(wsClients int) statsSnapshot {
	snapshot := s.monitor.stats.snapshot(wsClients, s.startedAt)
	st := s.state.Load()
	if st == nil || st.handler == nil {
		return snapshot
	}
	now := time.Now()
	degraded := make(map[string]bool)
	configured := make(map[string]bool)
	for _, instances := range st.handler.byProtocol {
		for _, inst := range instances {
			configured[inst.name] = true
			if _, seen := degraded[inst.name]; !seen {
				degraded[inst.name] = inst.isDegraded(now)
			}
		}
	}
	for i := range snapshot.Instances {
		name := snapshot.Instances[i].Name
		snapshot.Instances[i].Degraded = degraded[name]
		snapshot.Instances[i].Removed = !configured[name]
	}
	return snapshot
}

func (s *Server) serveStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(s.statsSnapshot(int(s.monitor.count.Load())))
}
