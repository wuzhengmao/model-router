package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// §24.26 M15dh–M15do/M15dv: thinking-block injection and response shape.

func tiEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func tiStart(index int, block string) string {
	return tiEvent("content_block_start",
		fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, index, block))
}

func tiDelta(index int, delta string) string {
	return tiEvent("content_block_delta",
		fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, index, delta))
}

func tiStop(index int) string {
	return tiEvent("content_block_stop",
		fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index))
}

func tiMessageDelta(outputTokens int) string {
	return tiEvent("message_delta",
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, outputTokens))
}

func tiMessageStop() string {
	return tiEvent("message_stop", `{"type":"message_stop"}`)
}

func tiMessageStart() string {
	return tiEvent("message_start",
		`{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":3,"output_tokens":1}}}`)
}

func tiToolUse(index int, id string) string {
	return tiStart(index, fmt.Sprintf(`{"type":"tool_use","id":%q,"name":"Read","input":{}}`, id)) +
		tiDelta(index, `{"type":"input_json_delta","partial_json":"{}"}`) +
		tiStop(index)
}

func tiThinking(index int, signature string) string {
	return tiStart(index, fmt.Sprintf(`{"type":"thinking","thinking":"","signature":%q}`, signature))
}

func tiText(index int, text string) string {
	return tiStart(index, `{"type":"text","text":""}`) +
		tiDelta(index, fmt.Sprintf(`{"type":"text_delta","text":%q}`, text)) +
		tiStop(index)
}

func tiRun(t *testing.T, payload string, parse, normalize, inject bool) (string, sseOutcome) {
	t.Helper()
	rec := newFlushRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	tracker := newMonitorHub().trackRequest(req, protocolAnthropic, "sonnet")
	outcome, err := (&Handler{}).copySSE(rec, io.NopCloser(strings.NewReader(payload)),
		0, 0, 0, 0, false, parse, "ti-inst", normalize, inject, false, false, func() {}, tracker)
	if err != nil {
		t.Fatalf("copySSE: %v", err)
	}
	return rec.body.String(), outcome
}

func tiHandlerBackend(t *testing.T, payload string, contentType string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(server.Close)
	return server
}

func tiHandler(t *testing.T, backendURL, name string) *Handler {
	t.Helper()
	return newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, streamAnthropicInstance(backendURL, name, 5*time.Second))
}

// TestThinkingInjectionAddsBlock covers M15dh/M15dk/M15dl: the injected block
// goes before message_delta/message_stop, uses max index+1, is a paired
// start/stop, and leaves every existing byte unchanged.
func TestThinkingInjectionAddsBlock(t *testing.T) {
	t.Run("before_message_delta", func(t *testing.T) {
		payload := tiMessageStart() + tiToolUse(0, "toolu_1") + tiMessageDelta(7) + tiMessageStop()
		got, outcome := tiRun(t, payload, true, false, true)
		injected := string(thinkingInjectionBytes(1))
		if !outcome.injected {
			t.Fatal("outcome.injected = false")
		}
		if !strings.Contains(got, injected) {
			t.Fatalf("client body missing injected block:\n%s", got)
		}
		if strings.Index(got, injected) > strings.Index(got, `"type":"message_delta"`) {
			t.Error("injected block is after message_delta")
		}
		if stripped := strings.Replace(got, injected, "", 1); stripped != payload {
			t.Errorf("existing bytes changed:\n got: %q\nwant: %q", stripped, payload)
		}
		if outcome.state.tokens != 7 || !outcome.state.hasTokens {
			t.Errorf("usage tokens = %d (known=%v), want 7", outcome.state.tokens, outcome.state.hasTokens)
		}
		if outcome.toolUseBlocks != 1 || outcome.thinkingBlocks != 0 {
			t.Errorf("shape = thinking %d tool_use %d, want 0/1", outcome.thinkingBlocks, outcome.toolUseBlocks)
		}
	})

	t.Run("before_message_stop", func(t *testing.T) {
		payload := tiMessageStart() + tiToolUse(0, "toolu_1") + tiMessageStop()
		got, outcome := tiRun(t, payload, true, false, true)
		injected := string(thinkingInjectionBytes(1))
		if !outcome.injected || !strings.Contains(got, injected) {
			t.Fatalf("missing injection:\n%s", got)
		}
		if strings.Index(got, injected) > strings.Index(got, `"type":"message_stop"`) {
			t.Error("injected block is after message_stop")
		}
		if stripped := strings.Replace(got, injected, "", 1); stripped != payload {
			t.Errorf("existing bytes changed:\n got: %q\nwant: %q", stripped, payload)
		}
	})

	t.Run("max_index_plus_one", func(t *testing.T) {
		payload := tiMessageStart() + tiToolUse(0, "toolu_1") + tiText(2, "hi") +
			tiMessageDelta(4) + tiMessageStop()
		got, _ := tiRun(t, payload, true, false, true)
		injected := string(thinkingInjectionBytes(3))
		if !strings.Contains(got, injected) {
			t.Fatalf("injected index should be max+1=3:\n%s", got)
		}
		if strings.Count(got, `"type":"content_block_start"`) != 3 {
			t.Errorf("content_block_start count = %d, want 3 (tool_use, text, injected thinking)",
				strings.Count(got, `"type":"content_block_start"`))
		}
		if strings.Count(got, `"type":"content_block_stop"`) != 3 {
			t.Errorf("content_block_stop count = %d, want 3", strings.Count(got, `"type":"content_block_stop"`))
		}
	})
}

// TestThinkingInjectionSkipped covers M15di/M15dj: thinking+tool_use and
// thinking-only streams are byte-identical.
func TestThinkingInjectionSkipped(t *testing.T) {
	cases := map[string]string{
		"thinking_and_tool_use": tiMessageStart() + tiThinking(0, "") +
			tiDelta(0, `{"type":"thinking_delta","thinking":"t"}`) +
			tiDelta(0, `{"type":"signature_delta","signature":"sig"}`) + tiStop(0) +
			tiToolUse(1, "toolu_1") + tiMessageDelta(5) + tiMessageStop(),
		"thinking_only": tiMessageStart() + tiThinking(0, "") +
			tiDelta(0, `{"type":"thinking_delta","thinking":"t"}`) + tiStop(0) +
			tiMessageDelta(2) + tiMessageStop(),
		"text_only": tiMessageStart() + tiText(0, "hi") + tiMessageDelta(1) + tiMessageStop(),
	}
	for name, payload := range cases {
		name, payload := name, payload
		t.Run(name, func(t *testing.T) {
			got, outcome := tiRun(t, payload, true, false, true)
			if got != payload {
				t.Errorf("client body changed:\n got: %q\nwant: %q", got, payload)
			}
			if outcome.injected {
				t.Error("injected = true, want false")
			}
		})
	}
}

// TestThinkingInjectionSkippedNonStreaming covers M15dm.
func TestThinkingInjectionSkippedNonStreaming(t *testing.T) {
	payload := `{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{}}]}`
	backend := tiHandlerBackend(t, payload, "application/json")
	handler := tiHandler(t, backend.URL, "plain-inst")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK || body != payload {
		t.Fatalf("response = %d %q, want 200 and the original bytes", status, body)
	}
	if strings.Contains(logs(), "thinking block injected:") {
		t.Errorf("non-streaming response was injected:\n%s", logs())
	}
}

// TestThinkingInjectionSkippedUnparseable covers M15dn: parse=false disables
// both signature normalization and injection.
func TestThinkingInjectionSkippedUnparseable(t *testing.T) {
	payload := tiMessageStart() + tiToolUse(0, "toolu_1") + tiMessageStop()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(backend.Close)
	handler := tiHandler(t, backend.URL, "br-inst")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	logs := captureStreamLogs(t)

	status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK || body != payload {
		t.Fatalf("response = %d %q, want the original bytes", status, body)
	}
	if strings.Contains(logs(), "thinking block injected:") {
		t.Errorf("parse=false stream was injected:\n%s", logs())
	}
}

// TestThinkingInjectionLogOnce covers M15do.
func TestThinkingInjectionLogOnce(t *testing.T) {
	payload := tiMessageStart() + tiToolUse(0, "t1") + tiToolUse(1, "t2") + tiToolUse(2, "t3") +
		tiMessageDelta(9) + tiMessageStop()
	logs := captureStreamLogs(t)
	_, outcome := tiRun(t, payload, true, false, true)
	if !outcome.injected {
		t.Fatal("injected = false")
	}
	out := logs()
	if got := strings.Count(out, "thinking block injected:"); got != 1 {
		t.Errorf("injection log lines = %d, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, "thinking block injected: inst=ti-inst tool_use=3 index=3") {
		t.Errorf("unexpected injection log:\n%s", out)
	}

	logs2 := captureStreamLogs(t)
	_, outcome2 := tiRun(t, tiMessageStart()+tiText(0, "hi")+tiMessageDelta(1)+tiMessageStop(), true, false, true)
	if outcome2.injected {
		t.Fatal("text-only stream was injected")
	}
	if strings.Contains(logs2(), "thinking block injected:") {
		t.Errorf("no-injection stream logged an injection:\n%s", logs2())
	}
}

// TestThinkingInjectionIndependentOfNormalization covers M113: the two switches
// do not depend on each other.
func TestThinkingInjectionIndependentOfNormalization(t *testing.T) {
	payload := tiMessageStart() + tiThinking(0, "") +
		tiDelta(0, `{"type":"thinking_delta","thinking":"t"}`) + tiStop(0) +
		tiToolUse(1, "toolu_1") + tiMessageDelta(3) + tiMessageStop()

	normalized, outcome := tiRun(t, payload, true, true, false)
	if !strings.Contains(normalized, `"signature":"model-router-placeholder"`) {
		t.Errorf("normalization did not run:\n%s", normalized)
	}
	if outcome.injected {
		t.Error("normalization alone injected a block")
	}

	injected, outcome := tiRun(t, payload, true, false, true)
	if outcome.injected {
		t.Error("a stream with thinking and tool_use must not be injected")
	}
	if injected != payload {
		t.Errorf("normalization=false, injection=true changed bytes:\n got: %q\nwant: %q", injected, payload)
	}

	// A tool_use-only stream with normalization=false must still inject, and
	// the injected signature stays empty (not the §24.24 placeholder).
	payload = tiMessageStart() + tiToolUse(0, "toolu_1") + tiMessageDelta(3) + tiMessageStop()
	got, outcome := tiRun(t, payload, true, false, true)
	if !outcome.injected || !strings.Contains(got, `"signature":""`) {
		t.Errorf("injection alone did not run or changed the signature:\n%s", got)
	}
}

// TestRespShapeSignatureEmpty covers M15dv plus M117's injected flag.
func TestRespShapeSignatureEmpty(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantSig int
		wantInj int
	}{
		{
			name: "empty_start_empty_delta",
			payload: tiMessageStart() + tiThinking(0, "") +
				tiDelta(0, `{"type":"signature_delta","signature":""}`) + tiStop(0) +
				tiMessageStop(),
			wantSig: 1,
		},
		{
			name: "empty_start_signed_delta",
			payload: tiMessageStart() + tiThinking(0, "") +
				tiDelta(0, `{"type":"signature_delta","signature":"sig"}`) + tiStop(0) +
				tiMessageStop(),
			wantSig: 0,
		},
		{
			name: "signed_start_empty_delta",
			payload: tiMessageStart() + tiThinking(0, "sig") +
				tiDelta(0, `{"type":"signature_delta","signature":""}`) + tiStop(0) +
				tiMessageStop(),
			wantSig: 0,
		},
		{
			name:    "tool_use_only",
			payload: tiMessageStart() + tiToolUse(0, "toolu_1") + tiMessageDelta(1) + tiMessageStop(),
			wantInj: 1,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			backend := tiHandlerBackend(t, tc.payload, "text/event-stream")
			handler := tiHandler(t, backend.URL, "shape-inst")
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			logs := captureStreamLogs(t)
			if status, _, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`); err != nil || status != http.StatusOK {
				t.Fatalf("request = %d, %v; want 200", status, err)
			}
			out := logs()
			want := fmt.Sprintf("resp_shape thinking_blocks=1 tool_use_blocks=0 signature_empty=%d injected=%d", tc.wantSig, tc.wantInj)
			if tc.wantInj == 1 {
				want = "resp_shape thinking_blocks=0 tool_use_blocks=1 signature_empty=0 injected=1"
			}
			if !strings.Contains(out, want) {
				t.Errorf("missing %q:\n%s", want, out)
			}
		})
	}
}
