package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"model-router/internal/config"
)

// §24.17 M34: an instance is only ever closed or open; protocols additionally
// render locked and shadowed ("-") rows. The old probe state is gone because
// there is no active probing subsystem any more.
// §24.31 M160 adds a fifth state: "exhausted" marks an instance whose
// quota/balance is used up. It is independent of the breaker.
const (
	stateClosed    = "closed"
	stateOpen      = "open"
	stateLocked    = "locked"
	stateShadowed  = "-"
	stateExhausted = "exhausted"
)

// reason values reported in GET /api/stats (§24.15.3). The decision table does
// not fix the vocabulary; these codes mirror the transition that opened the
// breaker.
const (
	reasonFailureThreshold = "failure_threshold"
	// reasonQuotaExhausted marks an instance disabled by its balance check
	// (§24.31 M160).
	reasonQuotaExhausted = "quota_exhausted"
)

// isOpen reports whether the breaker is open. §24.17 M35: the only input is the
// backoff deadline, so an expired deadline means the instance is closed again
// without any probe or request — recovery is purely time-based.
func (inst *instanceRuntime) isOpen() bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.isOpenAt(inst.nowTime())
}

// isOpenAt is the lock-held form of isOpen; callers must hold inst.mu.
func (inst *instanceRuntime) isOpenAt(now time.Time) bool {
	return !inst.degradedUntil.IsZero() && now.Before(inst.degradedUntil)
}

// nowTime returns the instance clock, defaulting to the wall clock so literals
// built in tests stay usable.
func (inst *instanceRuntime) nowTime() time.Time {
	if inst.now != nil {
		return inst.now()
	}
	return time.Now()
}

func (inst *instanceRuntime) state() string {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	// §24.31 M160: a used up quota outranks the breaker state; the quota
	// recovers on its own, the breaker does not.
	if inst.quotaExhausted {
		return stateExhausted
	}
	if inst.isOpenAt(inst.nowTime()) {
		return stateOpen
	}
	return stateClosed
}

func (inst *instanceRuntime) resetState() {
	inst.mu.Lock()
	inst.consecutiveFailures = 0
	inst.degradedUntil = time.Time{}
	inst.degradedSince = time.Time{}
	inst.degradeReason = ""
	inst.mu.Unlock()
}

func (inst *instanceRuntime) recordSuccess() {
	inst.mu.Lock()
	inst.consecutiveFailures = 0
	inst.degradedUntil = time.Time{}
	inst.degradedSince = time.Time{}
	inst.degradeReason = ""
	inst.mu.Unlock()
}

// markOpenLocked opens (or re-times) the breaker. degradedSince is the first
// failure of this outage; re-timing an already open instance keeps it (§24.15
// M7/M15).
func (inst *instanceRuntime) markOpenLocked(now time.Time, duration time.Duration, reason string) {
	// §24.16 M32: a protocol with a single configured instance never opens. The
	// failure is still counted by statsAggregator; only the breaker transition
	// is suppressed.
	if inst.singleInstanceProtocol {
		return
	}
	if inst.degradedUntil.IsZero() {
		inst.degradedSince = now
	}
	inst.degradedUntil = now.Add(duration)
	inst.degradeReason = reason
}

// recordFailure counts one failed real request. Reaching failure_threshold
// opens the breaker; a failure while the breaker is (or was) open re-times the
// deadline to now+duration with no backoff or exponential growth (§24.17 M38).
func (inst *instanceRuntime) recordFailure(now time.Time, threshold int, duration time.Duration) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.consecutiveFailures++
	if !inst.degradedUntil.IsZero() || inst.consecutiveFailures >= threshold {
		inst.markOpenLocked(now, duration, reasonFailureThreshold)
	}
}

func (inst *instanceRuntime) displayState() (state string, since time.Time, reason string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	// §24.31 M160: the quota state outranks the breaker state, but recovering
	// from a quota never clears an open breaker.
	if inst.quotaExhausted {
		return stateExhausted, inst.quotaExhaustedSince, reasonQuotaExhausted
	}
	if inst.isOpenAt(inst.nowTime()) {
		return stateOpen, inst.degradedSince, inst.degradeReason
	}
	return stateClosed, time.Time{}, ""
}

// protocolRuntime holds the per-protocol instance list and lock (§24.15
// M9/M10/M12). §24.17 M37 removed the current pointer: routing always starts at
// the first closed instance, so no chain head is maintained. It is memory-only
// and rebuilt with every Handler.
type protocolRuntime struct {
	mu     sync.Mutex
	insts  []*instanceRuntime
	locked int // -1 when unlocked
}

func newProtocolRuntime(insts []*instanceRuntime) *protocolRuntime {
	return &protocolRuntime{insts: insts, locked: -1}
}

func (ps *protocolRuntime) lockedIndex() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.locked
}

func (ps *protocolRuntime) isLocked() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.locked >= 0
}

func (ps *protocolRuntime) stateFor(index int) (state string, degradedSince int64, reason string, locked bool) {
	ps.mu.Lock()
	lockedIdx := ps.locked
	ps.mu.Unlock()
	if lockedIdx >= 0 {
		if index == lockedIdx {
			return stateLocked, 0, stateLocked, true
		}
		return stateShadowed, 0, "", false
	}
	inst := ps.insts[index]
	state, since, reason := inst.displayState()
	var sinceMs int64
	// §24.31 M160: an exhausted instance reports since when the quota ran out.
	if (state == stateOpen || state == stateExhausted) && !since.IsZero() {
		sinceMs = since.UnixMilli()
	}
	return state, sinceMs, reason, false
}

// lockInstance pins the protocol to name. Re-locking the same instance is
// idempotent; locking a different one moves the pin.
func (ps *protocolRuntime) lockInstance(name string) (int, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for i, inst := range ps.insts {
		if inst.name == name {
			ps.locked = i
			return i, true
		}
	}
	return 0, false
}

// unlock releases the pin and clears every breaker counter in the protocol
// (§24.15 M10). Routing then resumes at the first closed instance (§24.17 M36).
func (ps *protocolRuntime) unlock() bool {
	ps.mu.Lock()
	idx := ps.locked
	if idx < 0 {
		ps.mu.Unlock()
		return false
	}
	ps.locked = -1
	insts := append([]*instanceRuntime(nil), ps.insts...)
	ps.mu.Unlock()
	for _, inst := range insts {
		inst.resetState()
	}
	return true
}

func (h *Handler) protocolRuntime(proto protocol) *protocolRuntime {
	if h == nil {
		return nil
	}
	return h.protocols[proto]
}

func (h *Handler) isProtocolLocked(proto protocol) bool {
	ps := h.protocolRuntime(proto)
	return ps != nil && ps.isLocked()
}

// isLockedInstance reports whether inst is the protocol's pinned instance.
// §24.25 M92 exempts only that instance from breaker bookkeeping; other
// instances of a locked protocol still count their failures.
func (h *Handler) isLockedInstance(inst *instanceRuntime) bool {
	if inst == nil {
		return false
	}
	ps := h.protocolRuntime(inst.proto)
	if ps == nil {
		return false
	}
	idx := ps.lockedIndex()
	if idx < 0 || idx >= len(ps.insts) {
		return false
	}
	return ps.insts[idx] == inst
}

func (h *Handler) recordFailure(inst *instanceRuntime) {
	// §24.25 M92: a failure on the pinned instance never touches the breaker.
	if h.isLockedInstance(inst) {
		return
	}
	inst.recordFailure(h.now(), h.failureThreshold, h.degradeDuration)
}

func (h *Handler) recordSuccess(inst *instanceRuntime) {
	if h.isProtocolLocked(inst.proto) {
		return
	}
	inst.recordSuccess()
}

// lockedCandidate maps a request onto the pinned instance (§24.15 M10).
// §24.25 M99/M100: the model rewrite goes through the same resolver as
// fallbackCandidate; when the instance declares no usable mapping the client
// model stays untouched (no guessing).
func lockedCandidate(proto protocol, insts []*instanceRuntime, body []byte, index int) *candidate {
	if index < 0 || index >= len(insts) {
		return nil
	}
	return mappedCandidate(proto, insts[index], body)
}

// normalizeLockProtocol validates the protocol field of the lock APIs.
func normalizeLockProtocol(raw string) (protocol, error) {
	proto := protocol(strings.ToLower(strings.TrimSpace(raw)))
	if proto != protocolOpenAI && proto != protocolAnthropic {
		return "", fmt.Errorf("protocol must be %q or %q", config.ProtocolOpenAI, config.ProtocolAnthropic)
	}
	return proto, nil
}

func decodeInstanceRequest(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if err := dec.Decode(new(struct{})); err != io.EOF {
		return errors.New("invalid request body: unexpected trailing data")
	}
	return nil
}

// serveInstanceLock implements POST /api/instances/lock (§24.15.3/M10): the
// pinned instance serves every request of its protocol, breaker bookkeeping is
// paused and the other instances display as shadowed.
func (s *Server) serveInstanceLock(w http.ResponseWriter, r *http.Request, st *serverState) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Protocol string `json:"protocol"`
		Name     string `json:"name"`
	}
	if err := decodeInstanceRequest(r, &body); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	proto, err := normalizeLockProtocol(body.Protocol)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeAdminError(w, http.StatusBadRequest, "name is required")
		return
	}
	ps := st.handler.protocolRuntime(proto)
	if ps == nil {
		writeAdminError(w, http.StatusBadRequest, "no instances configured for protocol "+string(proto))
		return
	}
	if _, ok := ps.lockInstance(name); !ok {
		writeAdminError(w, http.StatusNotFound, "instance not found: "+name)
		return
	}
	log.Printf("instance %s (%s) locked; breaker paused", name, proto)
	writeAdminOK(w)
}

// serveInstanceUnlock implements POST /api/instances/unlock (§24.15.3/M10).
func (s *Server) serveInstanceUnlock(w http.ResponseWriter, r *http.Request, st *serverState) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Protocol string `json:"protocol"`
	}
	if err := decodeInstanceRequest(r, &body); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	proto, err := normalizeLockProtocol(body.Protocol)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	ps := st.handler.protocolRuntime(proto)
	if ps == nil {
		writeAdminError(w, http.StatusBadRequest, "no instances configured for protocol "+string(proto))
		return
	}
	if ps.unlock() {
		log.Printf("protocol %s unlocked; instance states reset", proto)
	}
	writeAdminOK(w)
}
