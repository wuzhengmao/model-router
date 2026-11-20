package proxy

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// §24.26 M15dt/M15du/M15dx: reqShape counters, unpaired indices, session hash
// and the log-only success path.

func shapeSessionHash(session string) string {
	sum := sha256.Sum256([]byte(session))
	return fmt.Sprintf("%x", sum[:4])
}

func shapeJSON(metadata string) string {
	if metadata == "" {
		return `{"model":"claude-sonnet-5","messages":[
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"a","name":"n","input":{}}]},
			{"role":"assistant","content":[{"type":"redacted_thinking","data":"r"},{"type":"tool_use","id":"b","name":"n","input":{}}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"n","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"x"}]}
		]}`
	}
	return `{"model":"claude-sonnet-5","metadata":{"user_id":` + metadata + `},"messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"a","name":"n","input":{}}]},
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"r"},{"type":"tool_use","id":"b","name":"n","input":{}}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"n","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"x"}]}
	]}`
}

// TestReqShapeDiagnosticsLog covers M15dt/M15du/M15dx: successful tool_use
// requests log the shape with a stable session hash and never enter
// recentErrors.
func TestReqShapeDiagnosticsLog(t *testing.T) {
	backend := newLXJSONBackend(t, http.StatusOK, `{"ok":true}`)
	handler := newStreamHandler(t, Options{},
		lxAnthropicInstance(backend.server.URL, "shape", 5*time.Second, map[string]string{"sonnet": "model-a"}))
	handler.monitor = newMonitorHub()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	userID := `"{\"session_id\":\"sess-abc\"}"`
	body := shapeJSON(userID)
	for i := 0; i < 2; i++ {
		status, _, err := doStreamRequest(t, server.URL, protocolAnthropic, body)
		if err != nil || status != http.StatusOK {
			t.Fatalf("request %d = %d, %v; want 200", i, status, err)
		}
	}
	want := "reqshape msgs=5 tool_use_msgs=3 paired_msgs=2 unpaired_idx=3 thinking=absent last=tool_result session=" + shapeSessionHash("sess-abc")
	if got := strings.Count(logs(), want); got != 2 {
		t.Errorf("reqshape lines = %d, want 2:\n%s", got, logs())
	}
	if recent, _ := reqShapeStats(t, handler); len(recent) != 0 {
		t.Errorf("recentErrors = %d, want 0 for successful requests", len(recent))
	}
}

// TestReqShapeSessionMissing covers M15dx's "-" fallbacks.
func TestReqShapeSessionMissing(t *testing.T) {
	cases := map[string]string{
		"no_metadata": "",
		"not_json":    `"not-json"`,
		"no_session":  `"{\"device_id\":\"x\"}"`,
	}
	for name, metadata := range cases {
		name, metadata := name, metadata
		t.Run(name, func(t *testing.T) {
			backend := newLXJSONBackend(t, http.StatusOK, `{"ok":true}`)
			handler := newStreamHandler(t, Options{},
				lxAnthropicInstance(backend.server.URL, "shape", 5*time.Second, map[string]string{"sonnet": "model-a"}))
			handler.monitor = newMonitorHub()
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			logs := captureStreamLogs(t)
			if status, _, err := doStreamRequest(t, server.URL, protocolAnthropic, shapeJSON(metadata)); err != nil || status != http.StatusOK {
				t.Fatalf("request = %d, %v; want 200", status, err)
			}
			if !strings.Contains(logs(), "session=-") {
				t.Errorf("missing session=- fallback:\n%s", logs())
			}
		})
	}
}

// TestReqShapeRedactedPairedAndUnpairedCap covers M15dt: redacted_thinking
// counts as paired, the marker disappears, and the index list is capped.
func TestReqShapeRedactedPairedAndUnpairedCap(t *testing.T) {
	redacted := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"r"},{"type":"tool_use","id":"a","name":"n","input":{}}]}
	]}`)
	shape := buildReqShape(protocolAnthropic, redacted)
	if !strings.Contains(shape, "tool_use_msgs=1 paired_msgs=1 unpaired_idx=-") {
		t.Errorf("redacted_thinking must count as paired: %s", shape)
	}
	if strings.Contains(shape, reqShapeToolUseFlag) {
		t.Errorf("redacted-thinking message was marked unpaired: %s", shape)
	}

	var b strings.Builder
	b.WriteString(`{"messages":[{"role":"user","content":[{"type":"text","text":"go"}]}`)
	for i := 0; i < 12; i++ {
		b.WriteString(fmt.Sprintf(`,{"role":"assistant","content":[{"type":"tool_use","id":"t%d","name":"n","input":{}}]}`, i))
	}
	b.WriteString(`]}`)
	shape = buildReqShape(protocolAnthropic, []byte(b.String()))
	if !strings.Contains(shape, "tool_use_msgs=12 paired_msgs=0 unpaired_idx=1,2,3,4,5,6,7,8,9,10,+") {
		t.Errorf("unpaired index cap is wrong: %s", shape[len(shape)-120:])
	}
}

// TestNoToolUseRequestUnchanged covers M15dy: a normal request is not injected
// and does not produce the reqshape log line.
func TestNoToolUseRequestUnchanged(t *testing.T) {
	payload := tiMessageStart() + tiText(0, "hello") + tiMessageDelta(2) + tiMessageStop()
	backend := tiHandlerBackend(t, payload, "text/event-stream")
	handler := tiHandler(t, backend.URL, "plain")
	handler.monitor = newMonitorHub()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if err != nil || status != http.StatusOK {
		t.Fatalf("request = %d, %v; want 200", status, err)
	}
	if body != payload {
		t.Errorf("plain stream changed:\n got: %q\nwant: %q", body, payload)
	}
	out := logs()
	if strings.Contains(out, "reqshape ") {
		t.Errorf("plain request logged reqshape:\n%s", out)
	}
	if strings.Contains(out, "thinking block injected:") {
		t.Errorf("plain request was injected:\n%s", out)
	}
	if recent, _ := reqShapeStats(t, handler); len(recent) != 0 {
		t.Errorf("recentErrors = %d, want 0", len(recent))
	}
}
