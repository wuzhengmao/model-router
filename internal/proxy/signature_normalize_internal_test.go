package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

// §24.24 M15cg–M15co: empty thinking signatures in Anthropic SSE are
// normalized to a fixed placeholder in the client-facing bytes only.

func sigSSEEvent(name, data string) string {
	if name == "" {
		return "data: " + data + "\n\n"
	}
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func sigThinkingStart(block string) string {
	return sigSSEEvent("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":`+block+`}`)
}

// sigUpstream serves payload either in one write or in fixed-size pieces so
// event-boundary buffering is exercised across reads.
func sigUpstream(t *testing.T, payload string, chunkSize int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if chunkSize <= 0 {
			_, _ = io.WriteString(w, payload)
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		for i := 0; i < len(payload); i += chunkSize {
			end := i + chunkSize
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := io.WriteString(w, payload[i:end]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func sigProxy(t *testing.T, proto protocol, payload string, chunkSize int) (string, *Handler) {
	t.Helper()
	upstream := sigUpstream(t, payload, chunkSize)
	var instance config.Instance
	var requestBody string
	if proto == protocolAnthropic {
		instance = streamAnthropicInstance(upstream.URL, "sig-inst", 5*time.Second)
		requestBody = `{"model":"sonnet","messages":[]}`
	} else {
		instance = streamOpenAIInstance(upstream.URL, "sig-inst", 5*time.Second, "gpt-x")
		requestBody = `{"model":"gpt-x"}`
	}
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, instance)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	status, body, err := doStreamRequest(t, server.URL, proto, requestBody)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	return body, handler
}

// TestSignatureNormalizeEmptySignature covers M15cg (signature "") and also
// feeds the stream in 7-byte pieces so the event spans reads.
func TestSignatureNormalizeEmptySignature(t *testing.T) {
	tail := sigSSEEvent("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`) +
		sigSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sigSSEEvent("message_stop", `{"type":"message_stop"}`)
	payload := sigThinkingStart(`{"type":"thinking","thinking":"","signature":""}`) + tail
	want := sigThinkingStart(`{"type":"thinking","thinking":"","signature":"model-router-placeholder"}`) + tail

	for _, chunkSize := range []int{0, 7} {
		chunkSize := chunkSize
		t.Run("chunk", func(t *testing.T) {
			got, _ := sigProxy(t, protocolAnthropic, payload, chunkSize)
			if got != want {
				t.Errorf("client body mismatch (chunk=%d):\n got: %q\nwant: %q", chunkSize, got, want)
			}
		})
	}

	t.Run("crlf", func(t *testing.T) {
		crlfPayload := strings.ReplaceAll(payload, "\n", "\r\n")
		crlfWant := strings.ReplaceAll(want, "\n", "\r\n")
		got, _ := sigProxy(t, protocolAnthropic, crlfPayload, 9)
		if got != crlfWant {
			t.Errorf("CRLF body mismatch:\n got: %q\nwant: %q", got, crlfWant)
		}
	})
}

// TestSignatureNormalizeMissingSignature covers M15cp: a thinking
// content_block_start without a signature field gets one inserted.
func TestSignatureNormalizeMissingSignature(t *testing.T) {
	payload := sigThinkingStart(`{"type":"thinking","thinking":""}`) +
		sigSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)
	want := sigThinkingStart(`{"signature":"model-router-placeholder","type":"thinking","thinking":""}`) +
		sigSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)

	got, _ := sigProxy(t, protocolAnthropic, payload, 0)
	if got != want {
		t.Errorf("missing signature body:\n got: %q\nwant: %q", got, want)
	}

	// Whitespace and field order inside content_block must survive.
	spaced := sigThinkingStart(`{ "thinking" : "" , "type" : "thinking" }`)
	spacedWant := sigThinkingStart(`{"signature":"model-router-placeholder", "thinking" : "" , "type" : "thinking" }`)
	got, _ = sigProxy(t, protocolAnthropic, spaced, 3)
	if got != spacedWant {
		t.Errorf("spaced missing signature body:\n got: %q\nwant: %q", got, spacedWant)
	}
}

// TestSignatureNormalizeNonEmptySignature covers M15ch: a non-empty signature
// is forwarded byte-for-byte.
func TestSignatureNormalizeNonEmptySignature(t *testing.T) {
	payload := sigThinkingStart(`{"type":"thinking","thinking":"","signature":"sig-abc+/="}`) +
		sigSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)
	got, _ := sigProxy(t, protocolAnthropic, payload, 0)
	if got != payload {
		t.Errorf("non-empty signature changed:\n got: %q\nwant: %q", got, payload)
	}
}

// TestSignatureNormalizeNonThinkingStart covers M15ci: content_block_start
// events whose block type is not thinking are untouched.
func TestSignatureNormalizeNonThinkingStart(t *testing.T) {
	payload := sigThinkingStart(`{"type":"text","text":"hi"}`) +
		sigThinkingStart(`{"type":"text","text":"","signature":""}`) +
		sigThinkingStart(`{"type":"redacted_thinking","data":"abc"}`)
	got, _ := sigProxy(t, protocolAnthropic, payload, 0)
	if got != payload {
		t.Errorf("non-thinking start changed:\n got: %q\nwant: %q", got, payload)
	}
}

// TestSignatureNormalizeOtherEvents covers M15cj: every non-thinking event is
// forwarded byte-for-byte, including signature_delta.
func TestSignatureNormalizeOtherEvents(t *testing.T) {
	payload := sigSSEEvent("message_start",
		`{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":1}}}`) +
		sigSSEEvent("ping", `{"type":"ping"}`) +
		sigSSEEvent("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`) +
		sigSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sigSSEEvent("message_stop", `{"type":"message_stop"}`)
	got, _ := sigProxy(t, protocolAnthropic, payload, 0)
	if got != payload {
		t.Errorf("non-thinking events changed:\n got: %q\nwant: %q", got, payload)
	}
}

// TestSignatureNormalizeUnparsablePassthrough covers M15ck: unparsable,
// multi-line-data and incomplete events are forwarded verbatim.
func TestSignatureNormalizeUnparsablePassthrough(t *testing.T) {
	thinking := sigThinkingStart(`{"type":"thinking","signature":""}`)
	thinkingWant := sigThinkingStart(`{"type":"thinking","signature":"model-router-placeholder"}`)

	t.Run("non-json", func(t *testing.T) {
		payload := sigSSEEvent("content_block_start", "not-json") + thinking
		want := sigSSEEvent("content_block_start", "not-json") + thinkingWant
		got, _ := sigProxy(t, protocolAnthropic, payload, 5)
		if got != want {
			t.Errorf("non-JSON event changed:\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("multi-line data", func(t *testing.T) {
		multi := "event: content_block_start\n" +
			`data: {"type":"content_block_start",` + "\n" +
			`data: "index":0,"content_block":{"type":"thinking","signature":""}}` + "\n\n"
		payload := multi + thinking
		want := multi + thinkingWant
		got, _ := sigProxy(t, protocolAnthropic, payload, 0)
		if got != want {
			t.Errorf("multi-line data event changed:\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("incomplete trailing event", func(t *testing.T) {
		payload := thinking[:len(thinking)-1] // drop the final newline of the blank line
		got, _ := sigProxy(t, protocolAnthropic, payload, 4)
		if got != payload {
			t.Errorf("incomplete event was not forwarded verbatim:\n got: %q\nwant: %q", got, payload)
		}
	})

	t.Run("oversized event", func(t *testing.T) {
		huge := sigSSEEvent("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+
				strings.Repeat("x", 80*1024)+`"}}`)
		if len(huge) <= maxSignatureEventBytes*2 {
			t.Fatalf("fixture event = %d bytes, want > %d", len(huge), maxSignatureEventBytes*2)
		}
		payload := huge + thinking
		want := huge + thinkingWant
		got, _ := sigProxy(t, protocolAnthropic, payload, 0)
		if got != want {
			t.Errorf("oversized event handling changed bytes (got %d bytes, want %d)", len(got), len(want))
		}
	})
}

// TestSignatureNormalizeStatsUnaffected covers M15cl: the parser and the stats
// numbers see the original bytes. The same stream with a pre-signed block
// yields the same sseState, and the handler still records usage from the raw
// message_start/message_delta events.
func TestSignatureNormalizeStatsUnaffected(t *testing.T) {
	payloadUnsigned := sigSSEEvent("message_start",
		`{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":11,"output_tokens":1}}}`) +
		sigThinkingStart(`{"type":"thinking","signature":""}`) +
		sigSSEEvent("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`) +
		sigSSEEvent("message_delta",
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":22}}`) +
		sigSSEEvent("message_stop", `{"type":"message_stop"}`)
	payloadSigned := strings.Replace(payloadUnsigned, `"signature":""`, `"signature":"sig"`, 1)

	run := func(payload string) (sseState, string) {
		rec := newFlushRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		tracker := newMonitorHub().trackRequest(req, protocolAnthropic, "sonnet")
		outcome, err := (&Handler{}).copySSE(rec, io.NopCloser(strings.NewReader(payload)),
			0, 0, 0, 0, true, true, "sig-inst", true, false, false, false, func() {}, tracker)
		if err != nil {
			t.Fatalf("copySSE: %v", err)
		}
		return outcome.state, rec.body.String()
	}

	unsignedState, unsignedBody := run(payloadUnsigned)
	signedState, _ := run(payloadSigned)
	if !strings.Contains(unsignedBody, `"signature":"model-router-placeholder"`) {
		t.Errorf("client body was not normalized:\n%s", unsignedBody)
	}
	if unsignedState.inTokens != 11 || !unsignedState.hasIn || unsignedState.tokens != 22 || !unsignedState.hasTokens {
		t.Errorf("state usage = in %d (known=%v) out %d (known=%v), want 11/22",
			unsignedState.inTokens, unsignedState.hasIn, unsignedState.tokens, unsignedState.hasTokens)
	}
	// sseState contains a byte slice, so compare the numeric/bool fields.
	if unsignedState.inTokens != signedState.inTokens || unsignedState.hasIn != signedState.hasIn ||
		unsignedState.tokens != signedState.tokens || unsignedState.hasTokens != signedState.hasTokens ||
		unsignedState.contentDeltas != signedState.contentDeltas || unsignedState.events != signedState.events ||
		unsignedState.inBlock != signedState.inBlock || unsignedState.blockIndex != signedState.blockIndex ||
		unsignedState.finished != signedState.finished {
		t.Errorf("rewrite changed parser state:\n unsigned: %+v\n   signed: %+v", unsignedState, signedState)
	}

	// Handler-level accounting uses the same values: rewritten bytes do not
	// leak into the stats aggregation. The handler built by NewWithOptions has
	// no monitor, so attach one (as the request-forensics tests do).
	upstream := sigUpstream(t, payloadUnsigned, 0)
	handler := newStreamHandler(t, Options{
		IdleTimeout:       5 * time.Second,
		FirstTokenTimeout: 5 * time.Second,
	}, streamAnthropicInstance(upstream.URL, "sig-inst", 5*time.Second))
	handler.monitor = newMonitorHub()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	status, _, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	snapshot := handler.monitor.stats.snapshot(0, time.Now())
	if snapshot.InTokens != 11 || snapshot.OutTokens != 22 {
		t.Errorf("stats in/out = %d/%d, want 11/22", snapshot.InTokens, snapshot.OutTokens)
	}
}

// TestSignatureNormalizeLogsOnce covers M15cm: one line per request with the
// block count, and no line when nothing was rewritten.
func TestSignatureNormalizeLogsOnce(t *testing.T) {
	t.Run("two blocks one line", func(t *testing.T) {
		logs := captureStreamLogs(t)
		payload := sigThinkingStart(`{"type":"thinking","signature":""}`) +
			sigThinkingStart(`{"type":"thinking","signature":""}`) +
			sigSSEEvent("message_stop", `{"type":"message_stop"}`)
		_, _ = sigProxy(t, protocolAnthropic, payload, 0)
		output := logs()
		if got := strings.Count(output, "thinking signature normalized:"); got != 1 {
			t.Errorf("normalization log lines = %d, want 1:\n%s", got, output)
		}
		if !strings.Contains(output, "thinking signature normalized: blocks=2 inst=sig-inst") {
			t.Errorf("log missing the block count/instance:\n%s", output)
		}
	})

	t.Run("no rewrite no line", func(t *testing.T) {
		logs := captureStreamLogs(t)
		payload := sigThinkingStart(`{"type":"thinking","signature":"sig"}`) +
			sigSSEEvent("message_stop", `{"type":"message_stop"}`)
		_, _ = sigProxy(t, protocolAnthropic, payload, 0)
		if output := logs(); strings.Contains(output, "thinking signature normalized:") {
			t.Errorf("unexpected normalization log:\n%s", output)
		}
	})
}

// TestSignatureNormalizeOpenAIAndNonStreamUnaffected covers M15co: OpenAI SSE
// and Anthropic non-streaming responses are byte-identical to the upstream.
func TestSignatureNormalizeOpenAIAndNonStreamUnaffected(t *testing.T) {
	t.Run("openai streaming", func(t *testing.T) {
		// Even an Anthropic-shaped event stays untouched on the OpenAI path.
		payload := sigThinkingStart(`{"type":"thinking","signature":""}`) +
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"a"}}]}` + "\n\n" +
			"data: [DONE]\n\n"
		got, _ := sigProxy(t, protocolOpenAI, payload, 0)
		if got != payload {
			t.Errorf("OpenAI stream changed:\n got: %q\nwant: %q", got, payload)
		}
	})

	t.Run("anthropic non-streaming", func(t *testing.T) {
		payload := `{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"","signature":""},{"type":"text","text":"ok"}]}`
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, payload)
		}))
		t.Cleanup(upstream.Close)
		handler := newStreamHandler(t, Options{}, streamAnthropicInstance(upstream.URL, "sig-inst", 5*time.Second))
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)

		status, body, err := doStreamRequest(t, server.URL, protocolAnthropic, `{"model":"sonnet","messages":[]}`)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if body != payload {
			t.Errorf("non-streaming body changed:\n got: %q\nwant: %q", body, payload)
		}
	})
}
