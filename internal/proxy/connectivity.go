package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"model-router/internal/config"
)

// §24.32 connectivity test: one minimal streaming probe sent directly to one
// instance, bypassing the candidate chain entirely.

// errNoTestModel is the §24.32 M181 error when neither models nor roles can
// name a model to probe.
const errNoTestModel = "instance has no model to test"

// instanceTestResult is the §24.32 M175 response body.
type instanceTestResult struct {
	Name       string       `json:"name"`
	Protocol   string       `json:"protocol"`
	OK         bool         `json:"ok"`
	Status     int          `json:"status"`
	DurationMs int64        `json:"durationMs"`
	State      string       `json:"state"`
	Balance    *balanceInfo `json:"balance,omitempty"`
	Error      string       `json:"error"`
}

// instanceTestBody is the §24.32 M177 minimal streaming probe.
type instanceTestBody struct {
	Model     string               `json:"model"`
	MaxTokens int                  `json:"max_tokens"`
	Stream    bool                 `json:"stream"`
	Messages  []instanceTestPrompt `json:"messages"`
}

type instanceTestPrompt struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// serveInstanceTest implements POST /api/instances/test (§24.32 M174): the test
// runs synchronously and its result is returned in the response body. Admin
// authentication is the same gate as lock/unlock because the path lives under
// /api/.
func (s *Server) serveInstanceTest(w http.ResponseWriter, r *http.Request, st *serverState) {
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
	var inst *instanceRuntime
	for _, candidate := range st.handler.byProtocol[proto] {
		if candidate.name == name {
			inst = candidate
			break
		}
	}
	if inst == nil {
		writeAdminError(w, http.StatusNotFound, "instance not found: "+name)
		return
	}
	writeJSONError(w, http.StatusOK, s.runInstanceTest(st.handler, inst))
}

// testInstanceState reports the same state vocabulary as /api/stats
// (§24.32 M175): an operator pin outranks the quota state, which outranks the
// breaker. displayState already applies exhausted > open > closed.
func testInstanceState(inst *instanceRuntime, locked bool) string {
	if locked {
		return stateLocked
	}
	state, _, _ := inst.displayState()
	return state
}

// runInstanceTest performs the probe of one instance and rewrites its breaker
// state per §24.32 M180.
func (s *Server) runInstanceTest(h *Handler, inst *instanceRuntime) instanceTestResult {
	start := time.Now()
	result := instanceTestResult{Name: inst.name, Protocol: string(inst.proto)}
	locked := h.isLockedInstance(inst)
	before := testInstanceState(inst, locked)

	model, ok := instanceTestModel(inst)
	if !ok {
		result.Error = errNoTestModel
		result.State = testInstanceState(inst, locked)
		result.DurationMs = time.Since(start).Milliseconds()
		logInstanceTest(inst, locked, time.Since(start), result.Error, before, result.State)
		return result
	}

	// §24.32 M183: the monitor row shows the same inbound path as a real request
	// of the protocol; the upstream URL (with any base_url prefix) is only used
	// to send the probe.
	target, err := instanceTestURL(inst)
	if err != nil {
		result.Error = err.Error()
		result.State = testInstanceState(inst, locked)
		result.DurationMs = time.Since(start).Milliseconds()
		logInstanceTest(inst, locked, time.Since(start), result.Error, before, result.State)
		return result
	}
	tracker := s.monitor.trackTestRequest(inst.proto, instanceTestMonitorPath(inst.proto), model)
	tracker.begin(&candidate{inst: inst, model: model})

	status, usage, testErr := probeInstance(inst, model, target.String(), tracker)
	tracker.end(status, testErr, attemptMetrics{
		instName:  inst.name,
		model:     model,
		reqModel:  model,
		hasModel:  true,
		inTokens:  usage.in,
		hasIn:     usage.hasIn,
		outTokens: usage.out,
		hasOut:    usage.hasOut,
	})
	result.OK = testErr == nil

	if !locked {
		// §24.32 M180: the verdict is written back to the breaker. A locked
		// instance is never touched, and markTestFailure keeps single-instance
		// protocols closed (§24.16 M32).
		if testErr == nil {
			inst.resetTestSuccess()
		} else {
			inst.markTestFailure(inst.nowTime(), h.degradeDuration)
		}
	}

	result.Status = status
	result.DurationMs = time.Since(start).Milliseconds()
	result.State = testInstanceState(inst, locked)
	if testErr != nil {
		result.Error = testErr.Error()
	}
	if info, ok := inst.balanceSnapshot(); ok {
		result.Balance = &info
	}
	logInstanceTest(inst, locked, time.Since(start), result.Error, before, result.State)
	return result
}

// instanceTestModel resolves the probe model per §24.32 M181: the first
// declared model, else the first role in the fixed priority, else any role key
// (sorted for determinism).
func instanceTestModel(inst *instanceRuntime) (string, bool) {
	if len(inst.models) > 0 {
		if model := strings.TrimSpace(inst.models[0]); model != "" {
			return model, true
		}
	}
	for _, role := range []string{
		config.RoleWildcard, config.RoleSonnet, config.RoleFable, config.RoleOpus, config.RoleHaiku,
	} {
		if model := strings.TrimSpace(inst.roles[role]); model != "" {
			return model, true
		}
	}
	roles := make([]string, 0, len(inst.roles))
	for role := range inst.roles {
		roles = append(roles, role)
	}
	slices.Sort(roles)
	for _, role := range roles {
		if model := strings.TrimSpace(inst.roles[role]); model != "" {
			return model, true
		}
	}
	return "", false
}

// §24.32 M177: a fixed ~10K-token context. The filler is a constant repeated
// deterministically (no randomness, no timestamps), so two tests against the
// same instance send byte-identical bodies: 45 chars × 900 = 40,500 chars plus
// the trailing instruction.
const (
	instanceTestFillerChunk   = "The quick brown fox jumps over the lazy dog. "
	instanceTestFillerRepeats = 900
	instanceTestInstruction   = "\n\nreply with OK"
	instanceTestMaxTokens     = 32
)

var instanceTestFiller = strings.Repeat(instanceTestFillerChunk, instanceTestFillerRepeats)

// instanceTestMonitorPath returns the inbound path a real request of the
// protocol would show in the monitor (§24.32 M183). It is independent of the
// instance base_url, so a test row and a real row of the same instance agree.
func instanceTestMonitorPath(proto protocol) string {
	if proto == protocolAnthropic {
		return PathAnthropicMessages
	}
	return PathOpenAI
}

// instanceTestURL builds the direct upstream target (§24.32 M176). The monitor
// row does not use its Path: it displays the protocol's inbound path instead.
func instanceTestURL(inst *instanceRuntime) (*url.URL, error) {
	entry := PathOpenAI
	if inst.proto == protocolAnthropic {
		entry = PathAnthropic
	}
	joined, err := url.JoinPath(inst.baseURL.String(), upstreamPath(entry))
	if err != nil {
		return nil, fmt.Errorf("join upstream URL: %w", err)
	}
	target, err := url.Parse(joined)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL: %w", err)
	}
	return target, nil
}

// probeInstance sends the minimal streaming request straight to inst and
// reports the upstream status, the best-effort usage and the §24.32 M178
// verdict. It never enters the candidate chain: no rewrite, no fallback, no
// retry, no lock handoff.
func probeInstance(inst *instanceRuntime, model, target string, tracker *requestMonitor) (int, usageTokens, error) {
	timeout := inst.timeout
	if timeout <= 0 {
		timeout = config.DefaultTimeout
	}
	// The test is bounded by the instance timeout from connect through the last
	// stream byte (§24.32 M179). It deliberately does not inherit the caller's
	// request context: the admin page navigates away as soon as the test starts,
	// and the probe must still complete and rewrite the breaker.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	payload, err := json.Marshal(instanceTestBody{
		Model:     model,
		MaxTokens: instanceTestMaxTokens,
		Stream:    true,
		Messages: []instanceTestPrompt{{
			Role:    "user",
			Content: instanceTestFiller + instanceTestInstruction,
		}},
	})
	if err != nil {
		return 0, usageTokens{}, fmt.Errorf("encode test request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, usageTokens{}, fmt.Errorf("build test request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if inst.proto == protocolAnthropic {
		req.Header.Set("x-api-key", inst.apiKey)
	} else {
		req.Header.Set("Authorization", bearerPrefix+inst.apiKey)
	}

	client := newClient(timeout)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if instanceTestTimedOut(ctx, err) {
			return 0, usageTokens{}, fmt.Errorf("test timeout after %s", formatProfileDuration(timeout))
		}
		return 0, usageTokens{}, fmt.Errorf("test request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, statsUpstreamBodyRune))
		return resp.StatusCode, usageTokens{}, fmt.Errorf("upstream status %d: %s", resp.StatusCode,
			truncateRunes(strings.TrimSpace(string(body)), statsErrorSummaryRune))
	}

	stream := io.Reader(resp.Body)
	if tracker != nil {
		stream = &testStreamReader{body: resp.Body, tracker: tracker}
	}
	usage, streamErr := consumeTestStream(stream)
	if streamErr != nil {
		if instanceTestTimedOut(ctx, streamErr) {
			return resp.StatusCode, usage, fmt.Errorf("test timeout after %s", formatProfileDuration(timeout))
		}
		return resp.StatusCode, usage, streamErr
	}
	return resp.StatusCode, usage, nil
}

// instanceTestTimedOut reports whether err came from the §24.32 M179 budget,
// either as the context deadline or as a transport timeout.
func instanceTestTimedOut(ctx context.Context, err error) bool {
	if ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// testStreamReader records every upstream read as one monitor chunk, so the
// TEST row gets a real timeline (§24.32 M183). The per-chunk token counters
// stay zero; stream usage is parsed separately by consumeTestStream.
type testStreamReader struct {
	body    io.Reader
	tracker *requestMonitor
}

func (r *testStreamReader) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := r.body.Read(p)
	if n > 0 {
		r.tracker.chunk(start, time.Now(), n, 0, 0, 0, false)
	}
	return n, err
}

// parseTestUsageLine extracts token usage from one SSE data line (§24.32
// M182). Anthropic message_start nests usage under message; message_delta and
// OpenAI chunks carry it at the top level, and a later frame wins. Malformed
// JSON, missing fields and wrong types are ignored: usage is best-effort and
// never changes the M178 verdict.
func (u *usageTokens) parseTestUsageLine(line string) {
	payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
	if !ok {
		return
	}
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return
	}
	var event struct {
		Usage   sseUsage `json:"usage"`
		Message struct {
			Usage sseUsage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return
	}
	if tokens, known := event.Message.Usage.inputTokens(); known {
		u.in, u.hasIn = tokens, true
	}
	if tokens, known := event.Message.Usage.outputTokens(); known {
		u.out, u.hasOut = tokens, true
	}
	if tokens, known := event.Usage.inputTokens(); known {
		u.in, u.hasIn = tokens, true
	}
	if tokens, known := event.Usage.outputTokens(); known {
		u.out, u.hasOut = tokens, true
	}
}

// consumeTestStream applies the §24.32 M178 success rule: the stream must end
// with message_stop or [DONE], or reach a clean EOF after at least one parsed
// event. A truncated read, a read error or an EOF with no event is a failure.
// It also returns whatever usage was parsed before the stream ended; usage
// never affects the verdict.
func consumeTestStream(body io.Reader) (usageTokens, error) {
	reader := bufio.NewReader(body)
	var usage usageTokens
	events := 0
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			usage.parseTestUsageLine(line)
			payload := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(payload, "event:"):
				if strings.TrimSpace(strings.TrimPrefix(payload, "event:")) == "message_stop" {
					return usage, nil
				}
			case strings.HasPrefix(payload, "data:"):
				data := strings.TrimSpace(strings.TrimPrefix(payload, "data:"))
				if data != "" {
					if data == "[DONE]" || strings.Contains(data, "message_stop") {
						return usage, nil
					}
					events++
				}
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			if events > 0 {
				return usage, nil
			}
			return usage, errors.New("upstream stream ended without events")
		}
		return usage, fmt.Errorf("read upstream stream: %w", readErr)
	}
}

// logInstanceTest prints the §24.32 M184 result line.
func logInstanceTest(inst *instanceRuntime, locked bool, duration time.Duration, errText, before, after string) {
	suffix := ""
	switch {
	case locked:
		suffix = " (locked, state unchanged)"
	case before != after:
		suffix = " → " + after
	}
	if errText == "" {
		log.Printf("instance %s test ok in %ss%s", inst.name, formatSeconds(duration), suffix)
		return
	}
	log.Printf("instance %s test failed in %ss: %s%s", inst.name, formatSeconds(duration), errText, suffix)
}
