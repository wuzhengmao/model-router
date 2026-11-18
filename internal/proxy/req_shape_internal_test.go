package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// §24.22 M63–M69: reqShape forensics. These tests live in package proxy so they
// can read the stats aggregator and the recent-request ring directly.

func reqShapeStats(t *testing.T, handler *Handler) ([]statsError, []monitorRequestSnapshot) {
	t.Helper()
	snapshot := handler.monitor.stats.snapshot(0, time.Now())
	return snapshot.RecentErrors, handler.monitor.snapshotRequests()
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// TestReqShapeAnthropicFormat covers M15bo and the §24.22.2 text format: the
// summary lists the message count and each message's role and block types, and
// never carries message text.
func TestReqShapeAnthropicFormat(t *testing.T) {
	body := `{"model":"claude-sonnet-4-5","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"secret-thought"},{"type":"text","text":"secret-text"},{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"secret-input"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"secret-result"}]},` +
		`{"role":"user","content":[{"type":"text","text":"again"}]}]}`

	const want = "msgs=4 | 0:user[text] | 1:assistant[thinking,text,tool_use] | 2:user[tool_result] | 3:user[text]"
	if got := buildReqShape(protocolAnthropic, []byte(body)); !strings.HasPrefix(got, want+" || ") {
		t.Errorf("reqShape = %q, want prefix %q", got, want)
	}
	if shape := buildReqShape(protocolAnthropic, []byte(body)); strings.Contains(shape, "secret") {
		t.Errorf("reqShape leaked message text: %q", shape)
	}
	for _, leaked := range []string{"secret-thought", "secret-text", "secret-input", "secret-result"} {
		if strings.Contains(buildReqShape(protocolAnthropic, []byte(body)), leaked) {
			t.Errorf("reqShape leaked %q", leaked)
		}
	}

	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	backend := newCBBackend(t, http.StatusBadRequest)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbAnthropicInstance(backend, "anthropic", map[string]string{"sonnet": "model-a"}, 2*time.Second))
	handler.monitor = newMonitorHub()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, _ := sendProxyRequest(t, server.URL, protocolAnthropic, body, true)
	if status != http.StatusBadRequest {
		t.Fatalf("client status = %d, want 400", status)
	}
	recent, _ := reqShapeStats(t, handler)
	if len(recent) != 1 {
		t.Fatalf("recentErrors = %d, want 1", len(recent))
	}
	if !strings.HasPrefix(recent[0].ReqShape, want+" || ") {
		t.Errorf("recentErrors[0].reqShape = %q, want prefix %q", recent[0].ReqShape, want)
	}
	if !strings.HasPrefix(recent[0].ReqShape, "msgs=4") {
		t.Errorf("reqShape does not report the 4 messages of the request: %q", recent[0].ReqShape)
	}
}

// TestReqShapeToolUseWithoutThinking covers M15bp: an assistant message with
// tool_use but no thinking block carries the marker right after "]".
func TestReqShapeToolUseWithoutThinking(t *testing.T) {
	anthropicBody := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"tool_use","id":"t1","name":"f","input":{}}]}]}`
	const wantAnthropic = "msgs=2 | 0:user[text] | 1:assistant[text,tool_use]!!tool_use_without_thinking"
	if got := buildReqShape(protocolAnthropic, []byte(anthropicBody)); !strings.HasPrefix(got, wantAnthropic+" || ") {
		t.Errorf("anthropic reqShape = %q, want prefix %q", got, wantAnthropic)
	}

	// OpenAI: a non-empty tool_calls array is the tool call, content is null.
	openAIBody := `{"model":"gpt-x","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`
	const wantOpenAI = "msgs=2 | 0:user[text] | 1:assistant[tool_use]!!tool_use_without_thinking"
	if got := buildReqShape(protocolOpenAI, []byte(openAIBody)); !strings.HasPrefix(got, wantOpenAI+" || ") {
		t.Errorf("openai reqShape = %q, want prefix %q", got, wantOpenAI)
	}
}

// TestReqShapeToolUseWithThinking covers M15bq: tool_use paired with thinking
// never carries the marker.
func TestReqShapeToolUseWithThinking(t *testing.T) {
	anthropicBody := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t"},{"type":"text","text":"a"},{"type":"tool_use","id":"t1","name":"f","input":{}}]}]}`
	const wantAnthropicThinking = "msgs=2 | 0:user[text] | 1:assistant[thinking,text,tool_use]"
	if got := buildReqShape(protocolAnthropic, []byte(anthropicBody)); !strings.HasPrefix(got, wantAnthropicThinking+" || ") {
		t.Errorf("anthropic reqShape = %q, want prefix %q", got, wantAnthropicThinking)
	}
	if strings.Contains(buildReqShape(protocolAnthropic, []byte(anthropicBody)), reqShapeToolUseFlag) {
		t.Error("anthropic reqShape carries the marker although thinking is present")
	}

	// OpenAI: reasoning_content counts as thinking.
	openAIBody := `{"model":"gpt-x","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"done","reasoning_content":"t","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`
	const wantOpenAI = "msgs=2 | 0:user[text] | 1:assistant[thinking,text,tool_use]"
	if got := buildReqShape(protocolOpenAI, []byte(openAIBody)); !strings.HasPrefix(got, wantOpenAI+" || ") {
		t.Errorf("openai reqShape = %q, want prefix %q", got, wantOpenAI)
	}

	// reasoning is an accepted alias of reasoning_content.
	aliasBody := `{"model":"gpt-x","messages":[{"role":"assistant","content":"done","reasoning":"t","tool_calls":[{"id":"c1"}]}]}`
	if got := buildReqShape(protocolOpenAI, []byte(aliasBody)); strings.Contains(got, reqShapeToolUseFlag) {
		t.Errorf("openai reqShape = %q, want no marker for reasoning+tool_calls", got)
	}
}

// TestReqShapeLargeBodyLimit covers M15br: a 1 MB body with 20 messages still
// yields at most reqShapeLimit characters, and an oversized summary is cut with
// the ellipsis marker.
func TestReqShapeLargeBodyLimit(t *testing.T) {
	type message struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	messages := make([]message, 20)
	payload := strings.Repeat("x", 55*1024)
	for i := range messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages[i] = message{Role: role, Content: payload}
	}
	largeBody := mustJSON(t, map[string]any{"model": "claude-sonnet-4-5", "messages": messages})
	if len(largeBody) < 1<<20 {
		t.Fatalf("test body = %d bytes, want >= 1 MiB", len(largeBody))
	}
	shape := buildReqShape(protocolAnthropic, []byte(largeBody))
	if len(shape) > reqShapeLimit || utf8.RuneCountInString(shape) > reqShapeLimit {
		t.Errorf("reqShape length = %d bytes / %d runes, want <= %d", len(shape), utf8.RuneCountInString(shape), reqShapeLimit)
	}
	if !strings.HasPrefix(shape, "msgs=20") {
		t.Errorf("reqShape = %q, want the msgs=20 prefix", shape)
	}

	many := make([]message, 0, 400)
	for i := 0; i < 400; i++ {
		many = append(many, message{Role: "user", Content: `{"type":"text"}`})
	}
	manyBody := mustJSON(t, map[string]any{"messages": many})
	truncated := buildReqShape(protocolOpenAI, []byte(manyBody))
	if len(truncated) > reqShapeLimit {
		t.Errorf("truncated reqShape length = %d, want <= %d", len(truncated), reqShapeLimit)
	}
	if !strings.HasPrefix(truncated, "msgs=400") {
		t.Errorf("truncated reqShape = %q, want the msgs=400 prefix", truncated)
	}
	if !strings.Contains(truncated, reqShapeEllipsis) {
		t.Errorf("truncated reqShape = %q, want the ellipsis marker", truncated)
	}
}

// TestReqShapeUnparsable covers M15bs: a non-JSON body keeps the forwarding
// behaviour untouched and is summarized as an "unparsable:" prefix.
func TestReqShapeUnparsable(t *testing.T) {
	const raw = "not-json"
	if got := buildReqShape(protocolOpenAI, []byte(raw)); got != "unparsable: "+raw {
		t.Errorf("reqShape = %q, want the unparsable summary", got)
	}

	// Valid JSON without a messages array is not a chat body either.
	if got := buildReqShape(protocolOpenAI, []byte(`{"model":"gpt-x"}`)); !strings.HasPrefix(got, "unparsable: ") {
		t.Errorf("reqShape = %q, want the unparsable summary", got)
	}

	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	backend := newCBBackend(t, http.StatusBadRequest)
	handler := newCBHandler(t, clock, 1, time.Minute, cbOpenAIInstance(backend, "openai", "gpt-x"))
	handler.monitor = newMonitorHub()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, _ := sendProxyRequest(t, server.URL, protocolOpenAI, raw, true)
	if status != http.StatusBadRequest {
		t.Fatalf("client status = %d, want the upstream 400", status)
	}
	if got := backend.last().body; got != raw {
		t.Errorf("upstream body = %q, want the raw client body %q", got, raw)
	}
	recent, _ := reqShapeStats(t, handler)
	if len(recent) != 1 {
		t.Fatalf("recentErrors = %d, want 1", len(recent))
	}
	if recent[0].ReqShape != "unparsable: "+raw {
		t.Errorf("recentErrors[0].reqShape = %q, want the unparsable summary", recent[0].ReqShape)
	}
}

// TestReqShapeSuccessNotRecorded covers M15bt/M69: successful requests carry no
// reqShape in recentErrors or in the monitor snapshot.
func TestReqShapeSuccessNotRecorded(t *testing.T) {
	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	backend := newCBBackend(t, http.StatusOK)
	handler := newCBHandler(t, clock, 1, time.Minute, cbOpenAIInstance(backend, "openai", "gpt-x"))
	handler.monitor = newMonitorHub()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	body := `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`
	if status, _ := sendProxyRequest(t, server.URL, protocolOpenAI, body, true); status != http.StatusOK {
		t.Fatalf("client status = %d, want 200", status)
	}
	recent, requests := reqShapeStats(t, handler)
	if len(recent) != 0 {
		t.Errorf("recentErrors = %d, want 0 for a successful request", len(recent))
	}
	if len(requests) != 1 {
		t.Fatalf("monitor requests = %d, want 1", len(requests))
	}
	if requests[0].ReqShape != "" {
		t.Errorf("successful request reqShape = %q, want empty", requests[0].ReqShape)
	}
	payload := mustJSON(t, requests[0])
	if strings.Contains(payload, "reqShape") {
		t.Errorf("successful snapshot serialized reqShape: %s", payload)
	}
}

// TestReqShapeDoesNotChangeForwardedBody covers M15bu/M68: reqShape is derived
// from a read-only copy, so the bytes sent upstream are unchanged.
func TestReqShapeDoesNotChangeForwardedBody(t *testing.T) {
	clientBody := `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`

	// A direct call must not mutate the caller's slice.
	original := append([]byte(nil), clientBody...)
	_ = buildReqShape(protocolOpenAI, original)
	if string(original) != clientBody {
		t.Fatalf("buildReqShape mutated its input: %q", string(original))
	}

	clock := &cbClock{now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	first := newCBBackend(t, http.StatusInternalServerError)
	second := newCBBackend(t, http.StatusBadRequest)
	handler := newCBHandler(t, clock, 1, time.Minute,
		cbOpenAIInstance(first, "first", "gpt-x"),
		cbOpenAIInstance(second, "second", "gpt-y"),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	body := []byte(clientBody)
	if status, _ := sendProxyRequest(t, server.URL, protocolOpenAI, clientBody, true); status != http.StatusBadRequest {
		t.Fatalf("client status = %d, want 400", status)
	}
	if got := string(body); got != clientBody {
		t.Errorf("client body was mutated during the request: %q", got)
	}
	// No rewrite needed on the exact-match candidate: byte-for-byte unchanged.
	if got := first.last().body; got != clientBody {
		t.Errorf("exact candidate body = %q, want verbatim %q", got, clientBody)
	}
	// Rewritten candidate: exactly rewriteModel's output, nothing more.
	wantRewritten := string(rewriteModel([]byte(clientBody), "gpt-y"))
	if got := second.last().body; got != wantRewritten {
		t.Errorf("rewritten candidate body = %q, want %q", got, wantRewritten)
	}
	if !strings.Contains(wantRewritten, `"model":"gpt-y"`) {
		t.Errorf("rewrite target missing from %q", wantRewritten)
	}
}

// TestReqShapeJSONField covers §24.22.3: the field is named reqShape and is
// omitted when empty on all three serialized shapes.
func TestReqShapeJSONField(t *testing.T) {
	const shape = "msgs=1 | 0:user[text]"

	statsPayload := mustJSON(t, statsError{ReqShape: shape})
	if !strings.Contains(statsPayload, `"reqShape":"`+shape+`"`) {
		t.Errorf("statsError JSON = %s, want the reqShape field", statsPayload)
	}
	snapshotPayload := mustJSON(t, monitorRequestSnapshot{ReqShape: shape})
	if !strings.Contains(snapshotPayload, `"reqShape":"`+shape+`"`) {
		t.Errorf("monitorRequestSnapshot JSON = %s, want the reqShape field", snapshotPayload)
	}
	endPayload := mustJSON(t, monitorEndEvent{ReqShape: shape})
	if !strings.Contains(endPayload, `"reqShape":"`+shape+`"`) {
		t.Errorf("monitorEndEvent JSON = %s, want the reqShape field", endPayload)
	}

	for name, payload := range map[string]string{
		"statsError":             mustJSON(t, statsError{}),
		"monitorRequestSnapshot": mustJSON(t, monitorRequestSnapshot{}),
		"monitorEndEvent":        mustJSON(t, monitorEndEvent{}),
	} {
		if strings.Contains(payload, "reqShape") {
			t.Errorf("%s JSON = %s, want reqShape omitted when empty", name, payload)
		}
	}
}

// TestReqShapeCountSuffix covers §24.22 H1: whole-history counters appended
// after the truncated segment list.
func TestReqShapeCountSuffix(t *testing.T) {
	body := []byte(`{"thinking":{"type":"disabled"},"messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"t"},{"type":"tool_use","id":"a","name":"n","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"x"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"b","name":"n","input":{}}]}
	]}`)
	shape := buildReqShape(protocolAnthropic, body)
	if !strings.Contains(shape, "|| tool_use_msgs=2 paired_msgs=1 unpaired_idx=3 thinking_declared=disabled") {
		t.Fatalf("unexpected suffix: %s", shape)
	}
}

// TestReqShapeCountSuffixBeyondTruncation proves the counters cover messages
// that the 1024-byte segment list drops.
func TestReqShapeCountSuffixBeyondTruncation(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"messages":[{"role":"user","content":[{"type":"text","text":"go"}]}`)
	for i := 0; i < 40; i++ {
		b.WriteString(`,{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"n","input":{}}]}`)
		b.WriteString(`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"y"}]}`)
	}
	b.WriteString(`]}`)
	shape := buildReqShape(protocolAnthropic, []byte(b.String()))
	if !strings.Contains(shape, reqShapeEllipsis) {
		t.Fatal("expected the segment list to be truncated")
	}
	if !strings.Contains(shape, "|| tool_use_msgs=40 paired_msgs=0 unpaired_idx=1,3,5,7,9,11,13,15,17,19,+ thinking_declared=absent") {
		t.Fatalf("counters must cover the whole history: %s", shape[len(shape)-120:])
	}
}

// TestReqShapeThinkingDeclaredEnabled covers the enabled declaration.
func TestReqShapeThinkingDeclaredEnabled(t *testing.T) {
	body := []byte(`{"thinking":{"type":"enabled","budget_tokens":1024},"messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	shape := buildReqShape(protocolAnthropic, body)
	if !strings.Contains(shape, "thinking_declared=enabled") {
		t.Fatalf("unexpected shape: %s", shape)
	}
}
