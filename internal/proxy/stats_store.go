package proxy

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

// §24.7 D11 / §24.10 B: cumulative statistics survive restarts. Only the
// cumulative counters and the recent-error table are written; percentile
// windows, QPS, uptime, active counts and the 100-request snapshot stay
// in memory.
const (
	statsSchemaVersion = 1
	statsFileName      = "stats.json"
	statsBadFileName   = "stats.json.bad"
	statsPersistPeriod = 5 * time.Second
	statsPersistTicker = time.Second
)

type persistedRequests struct {
	Total   int64 `json:"total"`
	Success int64 `json:"success"`
	Failed  int64 `json:"failed"`
}

type persistedInstance struct {
	Name         string `json:"name"`
	Requests     int64  `json:"requests"`
	Success      int64  `json:"success"`
	Failed       int64  `json:"failed"`
	TTFTCount    int64  `json:"ttftCount"`
	TTFTSumMs    int64  `json:"ttftSumMs"`
	GenOutTokens int64  `json:"genOutTokens"`
	GenMs        int64  `json:"genMs"`
}

type persistedStats struct {
	Schema       int                 `json:"schema"`
	Requests     persistedRequests   `json:"requests"`
	ByProtocol   map[string]int64    `json:"byProtocol"`
	InTokens     int64               `json:"inTokens"`
	OutTokens    int64               `json:"outTokens"`
	GenOutTokens int64               `json:"genOutTokens"`
	GenMs        int64               `json:"genMs"`
	Retries      int64               `json:"retries"`
	Instances    []persistedInstance `json:"instances"`
	RecentErrors []statsError        `json:"recentErrors"`
}

// valid rejects obviously corrupt counters; schema mismatches are handled in
// load before this check.
func (p persistedStats) valid() bool {
	nonNegative := []int64{
		p.Requests.Total, p.Requests.Success, p.Requests.Failed,
		p.InTokens, p.OutTokens, p.GenOutTokens, p.GenMs, p.Retries,
	}
	for _, value := range nonNegative {
		if value < 0 {
			return false
		}
	}
	if p.Requests.Total < p.Requests.Success+p.Requests.Failed {
		return false
	}
	for _, value := range p.ByProtocol {
		if value < 0 {
			return false
		}
	}
	for _, inst := range p.Instances {
		for _, value := range []int64{
			inst.Requests, inst.Success, inst.Failed, inst.TTFTCount,
			inst.TTFTSumMs, inst.GenOutTokens, inst.GenMs,
		} {
			if value < 0 {
				return false
			}
		}
	}
	return true
}

type statsStore struct {
	path string
}

func newStatsStore(configPath string) *statsStore {
	return &statsStore{path: filepath.Join(filepath.Dir(configPath), statsFileName)}
}

// load returns the persisted counters. A missing file is normal; a corrupt or
// unknown-schema file is renamed to stats.json.bad and startup continues with
// zero values — a bad statistics file must never block the server.
func (s *statsStore) load() persistedStats {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("warning: read %s: %v; starting with zero statistics", s.path, err)
		}
		return persistedStats{}
	}
	var parsed persistedStats
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.Schema != statsSchemaVersion || !parsed.valid() {
		badPath := filepath.Join(filepath.Dir(s.path), statsBadFileName)
		_ = os.Remove(badPath)
		if renameErr := os.Rename(s.path, badPath); renameErr != nil {
			log.Printf("warning: invalid %s (%v); rename to %s failed: %v; starting with zero statistics",
				s.path, err, badPath, renameErr)
		} else {
			log.Printf("warning: invalid %s; moved to %s and starting with zero statistics", s.path, badPath)
		}
		return persistedStats{}
	}
	return parsed
}

// write uses a same-directory temp file plus rename so readers never observe a
// partially written stats.json.
func (s *statsStore) write(data persistedStats) error {
	dir := filepath.Dir(s.path)
	temp, err := os.CreateTemp(dir, ".stats-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	discard := func() {
		_ = temp.Close()
		_ = os.Remove(name)
	}
	if err := json.NewEncoder(temp).Encode(data); err != nil {
		discard()
		return err
	}
	if err := temp.Sync(); err != nil {
		discard()
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// restore installs persisted counters into a fresh aggregator.
func (a *statsAggregator) restore(persisted persistedStats) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total = persisted.Requests.Total
	a.success = persisted.Requests.Success
	a.failure = persisted.Requests.Failed
	a.byProtocol = map[string]int64{
		"openai":    persisted.ByProtocol["openai"],
		"anthropic": persisted.ByProtocol["anthropic"],
	}
	a.inTokens = persisted.InTokens
	a.outTokens = persisted.OutTokens
	a.genOut = persisted.GenOutTokens
	a.genMs = persisted.GenMs
	a.retries = persisted.Retries
	for _, inst := range persisted.Instances {
		a.instances[inst.Name] = &instanceCounters{
			requests:  inst.Requests,
			success:   inst.Success,
			failure:   inst.Failed,
			ttftCount: inst.TTFTCount,
			ttftSum:   inst.TTFTSumMs,
			genOut:    inst.GenOutTokens,
			genMs:     inst.GenMs,
		}
	}
	a.recentErrors = append([]statsError(nil), persisted.RecentErrors...)
	if len(a.recentErrors) > statsRecentErrors {
		a.recentErrors = a.recentErrors[:statsRecentErrors]
	}
	a.dirty = false
	a.generation++
}

func (a *statsAggregator) persistedLocked() persistedStats {
	instances := make([]persistedInstance, 0, len(a.instances))
	for name, counters := range a.instances {
		instances = append(instances, persistedInstance{
			Name:         name,
			Requests:     counters.requests,
			Success:      counters.success,
			Failed:       counters.failure,
			TTFTCount:    counters.ttftCount,
			TTFTSumMs:    counters.ttftSum,
			GenOutTokens: counters.genOut,
			GenMs:        counters.genMs,
		})
	}
	recentErrors := make([]statsError, len(a.recentErrors))
	copy(recentErrors, a.recentErrors)
	byProtocol := make(map[string]int64, len(a.byProtocol))
	for proto, count := range a.byProtocol {
		byProtocol[proto] = count
	}
	return persistedStats{
		Schema: statsSchemaVersion,
		Requests: persistedRequests{
			Total:   a.total,
			Success: a.success,
			Failed:  a.failure,
		},
		ByProtocol:   byProtocol,
		InTokens:     a.inTokens,
		OutTokens:    a.outTokens,
		GenOutTokens: a.genOut,
		GenMs:        a.genMs,
		Retries:      a.retries,
		Instances:    instances,
		RecentErrors: recentErrors,
	}
}

// flush writes pending changes, at most once per statsPersistPeriod unless
// forced. Failures keep the dirty flag so the next tick retries.
func (a *statsAggregator) flush(force bool) {
	if a.store == nil {
		return
	}
	now := time.Now()
	a.mu.Lock()
	if !a.dirty {
		a.mu.Unlock()
		return
	}
	if !force && !a.lastPersist.IsZero() && now.Sub(a.lastPersist) < statsPersistPeriod {
		a.mu.Unlock()
		return
	}
	data := a.persistedLocked()
	a.dirty = false
	a.lastPersist = now
	a.mu.Unlock()
	if err := a.store.write(data); err != nil {
		log.Printf("warning: write %s: %v", a.store.path, err)
		a.mu.Lock()
		a.dirty = true
		a.mu.Unlock()
	}
}

func (a *statsAggregator) startPersistence() {
	if a.store == nil {
		return
	}
	a.persistStartOnce.Do(func() {
		a.persistStarted.Store(true)
		go func() {
			ticker := time.NewTicker(statsPersistTicker)
			defer ticker.Stop()
			defer close(a.persistDone)
			for {
				select {
				case <-a.persistStop:
					a.flush(true)
					return
				case <-ticker.C:
					a.flush(false)
				}
			}
		}()
	})
}

// stopPersistence flushes once and waits for the writer goroutine, if any.
func (a *statsAggregator) stopPersistence() {
	a.persistStopOnce.Do(func() { close(a.persistStop) })
	if a.persistStarted.Load() {
		<-a.persistDone
	}
	a.flush(true)
}

// clearCumulative resets counters, per-instance totals, percentile windows and
// recent errors, then persists immediately (§24.10 B1). The recent-request ring
// is intentionally untouched.
func (a *statsAggregator) clearCumulative() {
	a.mu.Lock()
	a.total = 0
	a.success = 0
	a.failure = 0
	a.byProtocol = map[string]int64{
		"openai":    0,
		"anthropic": 0,
	}
	a.inTokens = 0
	a.outTokens = 0
	a.genOut = 0
	a.genMs = 0
	a.retries = 0
	a.ttftSamples = sampleRing{}
	a.durationSamples = sampleRing{}
	a.instances = make(map[string]*instanceCounters)
	a.recentErrors = nil
	a.generation++
	a.dirty = true
	a.mu.Unlock()
	a.flush(true)
}
