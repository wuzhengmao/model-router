package proxy

import (
	"bufio"
	"cmp"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"model-router/internal/config"
)

// §24 real-time request monitor: a minimal RFC 6455 server (text/ping/pong/close
// frames only, no extensions or fragmentation) plus the per-request event tracker.
const (
	// PathWS is the §24.7 D9 global endpoint and the only WebSocket path since
	// §24.11 H2 (the old /api/monitor route was removed).
	PathWS = "/api/ws"

	// PathMonitorClear empties the recent-request ring (§24.7 D10).
	PathMonitorClear = "/api/monitor/clear"

	// monitorQueueLimit is how many undelivered events a single connection may
	// accumulate before it is disconnected (§24.2 F3).
	monitorQueueLimit = 1024

	// monitorActiveLimit caps in-progress requests carried by a snapshot
	// (§24.11 H1); the oldest entry is dropped when exceeded.
	monitorActiveLimit = 200

	// Per-request chunk history kept in the recent-request ring (§24.9 Z4):
	// at most 400 entries, the head/tail preserved and the middle folded into
	// one synthetic block so 200 + 1 + 199 == 400.
	snapshotChunksLimit = 400
	snapshotChunksHead  = 200
	snapshotChunksTail  = snapshotChunksLimit - snapshotChunksHead - 1

	wsGUID         = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsPingInterval = 30 * time.Second
	wsReadTimeout  = 75 * time.Second
	wsWriteTimeout = 10 * time.Second
	wsMaxFrameSize = 1 << 20
)

// Stats push cadence (§24.7 D7): changed data at most once per 2 seconds, an
// unchanged heartbeat every 10 seconds.
const (
	statsPushInterval     = 2 * time.Second
	statsHeartbeatPeriod  = 10 * time.Second
	statsTickerResolution = time.Second
)

// monitorHub fans events out to every connected WebSocket client. The zero value
// is not usable; NewServer creates one with newMonitorHub.
type monitorHub struct {
	mu      sync.Mutex
	clients map[*monitorClient]struct{}
	count   atomic.Int32
	ids     atomic.Uint64

	// active holds in-progress request monitors for the snapshot (§24.11 H1),
	// oldest first, capped at monitorActiveLimit.
	activeMu sync.Mutex
	active   []*requestMonitor

	stats         *statsAggregator
	startedAt     time.Time
	statsSource   func(wsClients int) statsSnapshot
	statsLoopOnce sync.Once
}

func newMonitorHub() *monitorHub {
	return &monitorHub{
		clients:   make(map[*monitorClient]struct{}),
		stats:     newStatsAggregator(),
		startedAt: time.Now(),
	}
}

// hasClients gates live event broadcasts only (§24.7 D9): statistics and the
// recent-request ring keep recording without clients, and no WS write is
// attempted while this is false.
func (h *monitorHub) hasClients() bool {
	return h != nil && h.count.Load() > 0
}

func (h *monitorHub) nextID() string {
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), h.ids.Add(1))
}

// broadcast marshals the event once and queues it on every connection. The send
// is non-blocking; a client that fell behind is disconnected by enqueue.
func (h *monitorHub) broadcast(event any) {
	if !h.hasClients() {
		return
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	h.mu.Lock()
	clients := make([]*monitorClient, 0, len(h.clients))
	for client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.Unlock()
	for _, client := range clients {
		client.enqueue(payload)
	}
}

func (h *monitorHub) add(client *monitorClient) {
	h.mu.Lock()
	h.clients[client] = struct{}{}
	h.mu.Unlock()
	h.count.Add(1)
}

func (h *monitorHub) remove(client *monitorClient) {
	h.mu.Lock()
	if _, ok := h.clients[client]; ok {
		delete(h.clients, client)
		h.count.Add(-1)
	}
	h.mu.Unlock()
}

// registerActive adds an in-progress tracker to the snapshot set. The oldest
// entry is dropped beyond monitorActiveLimit (§24.11 H1); the dropped request
// keeps running and still emits events to already connected clients.
func (h *monitorHub) registerActive(m *requestMonitor) {
	h.activeMu.Lock()
	h.active = append(h.active, m)
	if len(h.active) > monitorActiveLimit {
		h.active = append(h.active[:0], h.active[1:]...)
	}
	h.activeMu.Unlock()
}

func (h *monitorHub) removeActive(m *requestMonitor) {
	h.activeMu.Lock()
	for i, candidate := range h.active {
		if candidate == m {
			h.active = append(h.active[:i], h.active[i+1:]...)
			break
		}
	}
	h.activeMu.Unlock()
}

func (h *monitorHub) activeSnapshots(now time.Time) []monitorRequestSnapshot {
	h.activeMu.Lock()
	active := make([]*requestMonitor, len(h.active))
	copy(active, h.active)
	h.activeMu.Unlock()
	out := make([]monitorRequestSnapshot, 0, len(active))
	for _, monitor := range active {
		if snapshot, ok := monitor.snapshotInProgress(now); ok {
			out = append(out, snapshot)
		}
	}
	return out
}

// snapshotRequests merges the recently ended ring with in-progress requests and
// sorts the whole array by start time descending (§24.11 H1/H3d). An id that
// appears in both sets keeps the ended record.
func (h *monitorHub) snapshotRequests() []monitorRequestSnapshot {
	ended := h.stats.recentRequests()
	active := h.activeSnapshots(time.Now())
	out := make([]monitorRequestSnapshot, 0, len(ended)+len(active))
	seen := make(map[string]bool, len(ended))
	for _, request := range ended {
		out = append(out, request)
		seen[request.ID] = true
	}
	for _, request := range active {
		if seen[request.ID] {
			continue
		}
		out = append(out, request)
	}
	slices.SortStableFunc(out, func(a, b monitorRequestSnapshot) int {
		return cmp.Compare(b.TS, a.TS)
	})
	return out
}

func (h *monitorHub) snapshotStats(wsClients int) statsSnapshot {
	if h.statsSource != nil {
		return h.statsSource(wsClients)
	}
	return h.stats.snapshot(wsClients, h.startedAt)
}

func (h *monitorHub) snapshotPayload(wsClients int) []byte {
	payload, err := json.Marshal(monitorSnapshotEvent{
		Type:     "snapshot",
		Requests: h.snapshotRequests(),
		Stats:    h.snapshotStats(wsClients),
	})
	if err != nil {
		return nil
	}
	return payload
}

func (h *monitorHub) statsPayload(wsClients int) []byte {
	payload, err := json.Marshal(monitorStatsEvent{
		Type: "stats",
		TS:   time.Now().UnixMilli(),
		Data: h.snapshotStats(wsClients),
	})
	if err != nil {
		return nil
	}
	return payload
}

func (h *monitorHub) broadcastStats(payload []byte) {
	if payload == nil || !h.hasClients() {
		return
	}
	h.mu.Lock()
	clients := make([]*monitorClient, 0, len(h.clients))
	for client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.Unlock()
	for _, client := range clients {
		client.enqueue(payload)
	}
}

// statsLoop implements §24.7 D7: push promptly after a change but no more than
// once every 2 seconds, and send an unchanged heartbeat every 10 seconds.
func (h *monitorHub) statsLoop() {
	ticker := time.NewTicker(statsTickerResolution)
	defer ticker.Stop()
	var lastPush time.Time
	var lastGeneration uint64
	for range ticker.C {
		if !h.hasClients() {
			continue
		}
		now := time.Now()
		generation := h.stats.generationValue()
		changed := generation != lastGeneration
		if changed && now.Sub(lastPush) < statsPushInterval {
			continue
		}
		if !changed && now.Sub(lastPush) < statsHeartbeatPeriod {
			continue
		}
		h.broadcastStats(h.statsPayload(int(h.count.Load())))
		lastPush, lastGeneration = now, generation
	}
}

// monitorClient is one WebSocket connection. The writer goroutine owns all frame
// writes except pong replies, which are serialized by writeMu.
type monitorClient struct {
	hub     *monitorHub
	conn    net.Conn
	reader  *bufio.Reader
	writer  *bufio.Writer
	send    chan []byte
	done    chan struct{}
	once    sync.Once
	writeMu sync.Mutex
}

func newMonitorClient(hub *monitorHub, conn net.Conn, rw *bufio.ReadWriter) *monitorClient {
	return &monitorClient{
		hub:    hub,
		conn:   conn,
		reader: rw.Reader,
		writer: rw.Writer,
		send:   make(chan []byte, monitorQueueLimit),
		done:   make(chan struct{}),
	}
}

func (c *monitorClient) enqueue(payload []byte) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.send <- payload:
	default:
		c.drop()
	}
}

func (c *monitorClient) drop() {
	c.once.Do(func() {
		close(c.done)
		c.hub.remove(c)
		_ = c.conn.Close()
	})
}

func (c *monitorClient) writeLoop() {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	for {
		select {
		case payload := <-c.send:
			if err := c.writeFrame(0x1, payload); err != nil {
				c.drop()
				return
			}
		case <-ticker.C:
			if err := c.writeFrame(0x9, nil); err != nil {
				c.drop()
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *monitorClient) readLoop() {
	defer c.drop()
	for {
		if err := c.conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
			return
		}
		opcode, payload, err := readClientFrame(c.reader)
		if err != nil {
			return
		}
		switch opcode {
		case 0x8: // close
			_ = c.writeFrame(0x8, payload)
			return
		case 0x9: // ping
			if err := c.writeFrame(0xA, payload); err != nil {
				return
			}
		}
	}
}

func (c *monitorClient) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	header := make([]byte, 0, 10)
	header = append(header, 0x80|opcode)
	switch n := len(payload); {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xffff:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}
	if _, err := c.writer.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.writer.Write(payload); err != nil {
			return err
		}
	}
	return c.writer.Flush()
}

// readClientFrame decodes one masked client frame. Fragmentation is not used by
// browsers for our traffic; continuation frames are read and ignored.
func readClientFrame(reader *bufio.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length < 0 || length > wsMaxFrameSize {
		return 0, nil, errors.New("websocket frame too large")
	}
	if !masked {
		return 0, nil, errors.New("client websocket frames must be masked")
	}
	var mask [4]byte
	if _, err := io.ReadFull(reader, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return opcode, payload, nil
}

// serveWS upgrades the request and registers the connection with the hub. The
// caller has already enforced admin authentication (cookie, header or query).
// withStats enables the §24.7 D9 snapshot/stats frames for /api/ws; the legacy
// /api/monitor endpoint keeps its original wire behavior.
func (h *monitorHub) serveWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") ||
		!strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket requires connection hijacking", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	accept := wsAccept(key)
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(response); err != nil {
		_ = conn.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return
	}
	client := newMonitorClient(h, conn, rw)
	// Snapshot, registration and the initial frames are one atomic step under
	// h.mu: no broadcast can slip in between, so a request ending during the
	// snapshot still delivers its end event to this client (§24.11 H1/H3b).
	h.mu.Lock()
	if payload := h.snapshotPayload(int(h.count.Load()) + 1); payload != nil {
		client.enqueue(payload)
	}
	if payload := h.statsPayload(int(h.count.Load()) + 1); payload != nil {
		client.enqueue(payload)
	}
	h.clients[client] = struct{}{}
	h.count.Add(1)
	h.mu.Unlock()
	h.statsLoopOnce.Do(func() { go h.statsLoop() })
	go client.writeLoop()
	go client.readLoop()
}

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// monitorQueryAuthorized accepts the admin credentials as query parameters for
// browsers, which cannot set an Authorization header on WebSocket handshakes
// (§24.2 F4). The header path is tried first by the caller.
func monitorQueryAuthorized(cfg *config.Config, r *http.Request) bool {
	query := r.URL.Query()
	user, pass := query.Get("user"), query.Get("password")
	if user == "" && pass == "" {
		return false
	}
	return adminCredentialsMatch(cfg, user, pass)
}

// Event payloads. Field names follow §24.2 F3 exactly.
type monitorStartEvent struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	TS            int64  `json:"ts"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Protocol      string `json:"protocol"`
	ReqModel      string `json:"reqModel"`
	ClientIP      string `json:"clientIP"`
	UserAgent     string `json:"userAgent"`
	InstName      string `json:"instName"`
	UpstreamModel string `json:"upstreamModel"`
}

type monitorChunkEvent struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	Seq         int    `json:"seq"`
	TSStart     int64  `json:"tsStart"`
	TSEnd       int64  `json:"tsEnd"`
	Bytes       int    `json:"bytes"`
	DeltaTokens int    `json:"deltaTokens"`
	// ContentDeltas is how many content_block_delta events this chunk carried
	// (§24.8 Y6); 0 for heartbeats, block boundaries and usage-only frames.
	ContentDeltas int  `json:"contentDeltas"`
	CumTokens     int  `json:"cumTokens"`
	TokensKnown   bool `json:"tokensKnown"`
}

type monitorRetryEvent struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	InstName      string `json:"instName"`
	UpstreamModel string `json:"upstreamModel"`
	Reason        string `json:"reason"`
}

type monitorEndEvent struct {
	Type          string  `json:"type"`
	ID            string  `json:"id"`
	Status        int     `json:"status"`
	DurationMs    int64   `json:"durationMs"`
	InTokens      int     `json:"inTokens"`
	OutTokens     int     `json:"outTokens"`
	Chunks        int     `json:"chunks"`
	TTFTMs        int64   `json:"ttftMs"`
	GenMs         int64   `json:"genMs"`
	TPS           float64 `json:"tps"`
	InstName      string  `json:"instName"`
	UpstreamModel string  `json:"upstreamModel"`
	Error         string  `json:"error,omitempty"`
	// §24.20 M53: excerpt of a non-2xx upstream body.
	UpstreamBody string `json:"upstreamBody,omitempty"`
}

// monitorChunkSnapshot is the compact per-chunk copy kept in the recent-request
// ring (§24.9 Z4); it carries just what the timeline needs to repaint.
type monitorChunkSnapshot struct {
	Seq           int   `json:"seq"`
	TSStart       int64 `json:"tsStart"`
	TSEnd         int64 `json:"tsEnd"`
	Bytes         int   `json:"bytes"`
	DeltaTokens   int   `json:"deltaTokens"`
	ContentDeltas int   `json:"contentDeltas"`
	CumTokens     int   `json:"cumTokens"`
}

// monitorRequestSnapshot is one entry of the recent-request ring (§24.7 D9):
// the start fields plus the end event, so a reconnecting page can render the
// finished row without replaying every chunk.
type monitorRequestSnapshot struct {
	ID            string                 `json:"id"`
	TS            int64                  `json:"ts"`
	Method        string                 `json:"method"`
	Path          string                 `json:"path"`
	Protocol      string                 `json:"protocol"`
	ReqModel      string                 `json:"reqModel"`
	ClientIP      string                 `json:"clientIP"`
	UserAgent     string                 `json:"userAgent"`
	InstName      string                 `json:"instName"`
	UpstreamModel string                 `json:"upstreamModel"`
	Status        int                    `json:"status"`
	DurationMs    int64                  `json:"durationMs"`
	InTokens      int                    `json:"inTokens"`
	OutTokens     int                    `json:"outTokens"`
	Chunks        int                    `json:"chunks"`
	ChunksList    []monitorChunkSnapshot `json:"chunksList"`
	TTFTMs        int64                  `json:"ttftMs"`
	GenMs         int64                  `json:"genMs"`
	TPS           float64                `json:"tps"`
	Error         string                 `json:"error,omitempty"`
	UpstreamBody  string                 `json:"upstreamBody,omitempty"`
	Ended         bool                   `json:"ended"`
}

type monitorSnapshotEvent struct {
	Type     string                   `json:"type"`
	Requests []monitorRequestSnapshot `json:"requests"`
	Stats    statsSnapshot            `json:"stats"`
}

type monitorStatsEvent struct {
	Type string        `json:"type"`
	TS   int64         `json:"ts"`
	Data statsSnapshot `json:"data"`
}

const (
	retryReasonError             = "error"
	retryReasonFirstTokenTimeout = "first_token_timeout"
	// §24.20 M54: the candidate failed because the upstream answered with a
	// status outside 2xx.
	retryReasonUpstreamStatus = "upstream_status"
	// §24.21 M60: the request entered the fallback round (instances the first
	// round never called, each with its own mapped model).
	retryReasonFallbackRetry = "fallback_retry"
)

// attemptFailure carries the §24.2 F3 retry reason across the tryCandidate
// boundary without parsing the error text.
type attemptFailure struct {
	reason string
	err    error
}

func (e *attemptFailure) Error() string { return e.err.Error() }
func (e *attemptFailure) Unwrap() error { return e.err }

func retryReason(err error) string {
	var failure *attemptFailure
	if errors.As(err, &failure) {
		return failure.reason
	}
	var upstream *upstreamResponseError
	if errors.As(err, &upstream) {
		return retryReasonUpstreamStatus
	}
	return retryReasonError
}

// requestMonitor tracks one AI request. Statistics are recorded regardless of
// connected clients (§24.7 D9); live event broadcasts stay gated on the hub.
type requestMonitor struct {
	hub       *monitorHub
	stats     *statsAggregator
	id        string
	start     time.Time
	method    string
	path      string
	protocol  string
	reqModel  string
	clientIP  string
	userAgent string

	// mu guards the mutable fields below; snapshotInProgress reads them from a
	// WebSocket handler goroutine while the request goroutine updates them.
	mu            sync.Mutex
	started       bool
	finished      bool
	instName      string
	upstreamModel string
	chunks        int
	chunksList    []monitorChunkSnapshot
	cumTokens     int
	firstChunkAt  time.Time
	lastChunkAt   time.Time
}

// trackRequest always returns a tracker so cumulative statistics and the recent
// request ring keep working with no WebSocket client connected. A handler built
// directly (without a Server) has no hub and keeps the original nil tracker.
func (h *monitorHub) trackRequest(r *http.Request, proto protocol, reqModel string) *requestMonitor {
	if h == nil {
		return nil
	}
	now := time.Now()
	h.stats.begin(now)
	monitor := &requestMonitor{
		hub:       h,
		stats:     h.stats,
		id:        h.nextID(),
		start:     now,
		method:    r.Method,
		path:      r.URL.Path,
		protocol:  string(proto),
		reqModel:  reqModel,
		clientIP:  clientIP(r),
		userAgent: r.Header.Get("User-Agent"),
	}
	h.registerActive(monitor)
	return monitor
}

// newRequest keeps the §24.2 contract: no tracker is created while no monitor
// client is connected. The proxy itself uses trackRequest (§24.7 D9).
func (h *monitorHub) newRequest(r *http.Request, proto protocol, reqModel string) *requestMonitor {
	if !h.hasClients() {
		return nil
	}
	return h.trackRequest(r, proto, reqModel)
}

func (m *requestMonitor) begin(cand *candidate) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if cand != nil {
		m.instName = cand.inst.name
		m.upstreamModel = cand.model
	}
	first := !m.started
	m.started = true
	event := monitorStartEvent{
		Type:          "start",
		ID:            m.id,
		TS:            m.start.UnixMilli(),
		Method:        m.method,
		Path:          m.path,
		Protocol:      m.protocol,
		ReqModel:      m.reqModel,
		ClientIP:      m.clientIP,
		UserAgent:     m.userAgent,
		InstName:      m.instName,
		UpstreamModel: m.upstreamModel,
	}
	m.mu.Unlock()
	if !first || !m.hub.hasClients() {
		return
	}
	m.hub.broadcast(&event)
}

// chunk records one upstream trunk read. tsStart is taken just before the
// blocking read, tsEnd right after it returned, so gaps and TTFT stay visible.
// deltaTokens is the increase of the cumulative token counter across this read
// (X6), not the difference against the previous event. contentDeltas is the
// increase of the content_block_delta counter (§24.8 Y6).
func (m *requestMonitor) chunk(tsStart, tsEnd time.Time, n, deltaTokens, contentDeltas, cumTokens int, tokensKnown bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.firstChunkAt.IsZero() {
		m.firstChunkAt = tsEnd
	}
	m.lastChunkAt = tsEnd
	m.cumTokens = cumTokens
	seq := m.chunks
	m.chunks++
	// The ring copy is built regardless of connected clients (§24.9 Z4): the
	// snapshot must keep green-content chunks even for requests observed only
	// while no WebSocket client was attached.
	m.chunksList = append(m.chunksList, monitorChunkSnapshot{
		Seq:           seq,
		TSStart:       tsStart.UnixMilli(),
		TSEnd:         tsEnd.UnixMilli(),
		Bytes:         n,
		DeltaTokens:   deltaTokens,
		ContentDeltas: contentDeltas,
		CumTokens:     cumTokens,
	})
	m.mu.Unlock()
	if !m.hub.hasClients() {
		return
	}
	m.hub.broadcast(&monitorChunkEvent{
		Type:          "chunk",
		ID:            m.id,
		Seq:           seq,
		TSStart:       tsStart.UnixMilli(),
		TSEnd:         tsEnd.UnixMilli(),
		Bytes:         n,
		DeltaTokens:   deltaTokens,
		ContentDeltas: contentDeltas,
		CumTokens:     cumTokens,
		TokensKnown:   tokensKnown,
	})
}

// compactChunkSnapshots enforces the §24.9 Z4 ring budget: keep the first 200
// and last 199 chunks and fold the skipped middle into one synthetic block so
// the total is exactly 400. The synthetic block keeps the time span, the summed
// byte/token/content counters and the midpoint sequence number.
func compactChunkSnapshots(chunks []monitorChunkSnapshot) []monitorChunkSnapshot {
	out := make([]monitorChunkSnapshot, 0, snapshotChunksLimit)
	if len(chunks) <= snapshotChunksLimit {
		return append(out, chunks...)
	}
	head := chunks[:snapshotChunksHead]
	tail := chunks[len(chunks)-snapshotChunksTail:]
	skipped := chunks[snapshotChunksHead : len(chunks)-snapshotChunksTail]
	synthetic := monitorChunkSnapshot{
		Seq:       (skipped[0].Seq + skipped[len(skipped)-1].Seq) / 2,
		TSStart:   skipped[0].TSStart,
		TSEnd:     skipped[len(skipped)-1].TSEnd,
		CumTokens: skipped[len(skipped)-1].CumTokens,
	}
	for _, chunk := range skipped {
		synthetic.Bytes += chunk.Bytes
		synthetic.DeltaTokens += chunk.DeltaTokens
		synthetic.ContentDeltas += chunk.ContentDeltas
	}
	out = append(out, head...)
	out = append(out, synthetic)
	return append(out, tail...)
}

func (m *requestMonitor) retry(next *candidate, reason string) {
	if m == nil || next == nil {
		return
	}
	m.stats.retry()
	if !m.hub.hasClients() {
		return
	}
	m.hub.broadcast(&monitorRetryEvent{
		Type:          "retry",
		ID:            m.id,
		InstName:      next.inst.name,
		UpstreamModel: next.model,
		Reason:        reason,
	})
}

// timingLocked returns the per-request TTFT and generation window. Callers must
// hold m.mu.
func (m *requestMonitor) timingLocked() (ttftMs, genMs int64) {
	if m.firstChunkAt.IsZero() {
		return 0, 0
	}
	return m.firstChunkAt.Sub(m.start).Milliseconds(),
		m.lastChunkAt.Sub(m.firstChunkAt).Milliseconds()
}

// snapshotInProgress builds the snapshot row for a request that has not ended
// (§24.11 H1). It returns false after end() has taken the final snapshot so a
// finished request never appears as in-progress.
func (m *requestMonitor) snapshotInProgress(now time.Time) (monitorRequestSnapshot, bool) {
	if m == nil {
		return monitorRequestSnapshot{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.finished {
		return monitorRequestSnapshot{}, false
	}
	ttftMs, genMs := m.timingLocked()
	tps := 0.0
	if genMs > 0 && m.cumTokens > 0 {
		tps = math.Round(float64(m.cumTokens)*1000/float64(genMs)*10) / 10
	}
	return monitorRequestSnapshot{
		ID:            m.id,
		TS:            m.start.UnixMilli(),
		Method:        m.method,
		Path:          m.path,
		Protocol:      m.protocol,
		ReqModel:      m.reqModel,
		ClientIP:      m.clientIP,
		UserAgent:     m.userAgent,
		InstName:      m.instName,
		UpstreamModel: m.upstreamModel,
		Status:        0,
		DurationMs:    now.Sub(m.start).Milliseconds(),
		InTokens:      0,
		OutTokens:     m.cumTokens,
		Chunks:        m.chunks,
		ChunksList:    compactChunkSnapshots(m.chunksList),
		TTFTMs:        ttftMs,
		GenMs:         genMs,
		TPS:           tps,
		Ended:         false,
	}, true
}

func (m *requestMonitor) end(status int, err error, metrics attemptMetrics) {
	if m == nil {
		return
	}
	m.mu.Lock()
	started := m.started
	m.mu.Unlock()
	if !started {
		m.begin(nil)
	}
	inTokens, outTokens := 0, 0
	if metrics.hasIn {
		inTokens = metrics.inTokens
	}
	if metrics.hasOut {
		outTokens = metrics.outTokens
	}
	message := ""
	if err != nil {
		message = err.Error()
	}

	m.mu.Lock()
	ttftMs, genMs := m.timingLocked()
	tps := 0.0
	if genMs > 0 && outTokens > 0 {
		tps = math.Round(float64(outTokens)*1000/float64(genMs)*10) / 10
	}
	m.finished = true
	snapshot := monitorRequestSnapshot{
		ID:            m.id,
		TS:            m.start.UnixMilli(),
		Method:        m.method,
		Path:          m.path,
		Protocol:      m.protocol,
		ReqModel:      m.reqModel,
		ClientIP:      m.clientIP,
		UserAgent:     m.userAgent,
		InstName:      m.instName,
		UpstreamModel: m.upstreamModel,
		Status:        status,
		DurationMs:    time.Since(m.start).Milliseconds(),
		InTokens:      inTokens,
		OutTokens:     outTokens,
		Chunks:        m.chunks,
		ChunksList:    compactChunkSnapshots(m.chunksList),
		TTFTMs:        ttftMs,
		GenMs:         genMs,
		TPS:           tps,
		Error:         message,
		UpstreamBody:  metrics.upstreamBody,
		Ended:         true,
	}
	m.mu.Unlock()

	// Finish the ring before unregistering: a snapshot taken in between then
	// sees the ended record (dedup prefers it) instead of neither set.
	m.stats.finish(snapshot, status >= 200 && status < 400 && message == "")
	m.hub.removeActive(m)

	if !m.hub.hasClients() {
		return
	}
	m.hub.broadcast(&monitorEndEvent{
		Type:          "end",
		ID:            snapshot.ID,
		Status:        snapshot.Status,
		DurationMs:    snapshot.DurationMs,
		InTokens:      snapshot.InTokens,
		OutTokens:     snapshot.OutTokens,
		Chunks:        snapshot.Chunks,
		TTFTMs:        snapshot.TTFTMs,
		GenMs:         snapshot.GenMs,
		TPS:           snapshot.TPS,
		InstName:      snapshot.InstName,
		UpstreamModel: snapshot.UpstreamModel,
		Error:         snapshot.Error,
		UpstreamBody:  snapshot.UpstreamBody,
	})
}
