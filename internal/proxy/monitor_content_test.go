package proxy_test

import (
	"io"
	"net/http"
	"testing"
	"time"

	"model-router/internal/config"
)

// TestChunkEventContentDeltasOverWS covers §24.8 Y4 end to end: the chunk event
// carries contentDeltas, the per-chunk values add up to the number of
// content_block_delta events, and deltaTokens keeps its usage-based meaning.
func TestChunkEventContentDeltasOverWS(t *testing.T) {
	chunks := []string{
		`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"alpha"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" beta"}}

`,
		`event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`,
	}
	backend := monitorSSEBackend(t, chunks)
	server, _ := newAdminServer(t, monitorAnthropicConfig(backend.URL, "MTI"))

	client, resp := dialMonitor(t, server.URL, monitorAdminQuery(), nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket handshake status = %d, want 101", resp.StatusCode)
	}

	stream := send(t, http.MethodPost, server.URL+"/v1/messages",
		`{"model":"sonnet","messages":[],"stream":true}`, withAuth(config.ProtocolAnthropic, nil))
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", stream.StatusCode)
	}
	_, _ = io.Copy(io.Discard, stream.Body)

	events := readEventsUntilEnd(t, client, 10*time.Second)
	var chunkEvents []map[string]any
	for _, event := range events {
		if eventType(t, event) == "chunk" {
			chunkEvents = append(chunkEvents, event)
		}
	}
	if len(chunkEvents) != len(chunks) {
		t.Fatalf("chunk events = %d, want %d (upstream trunks)", len(chunkEvents), len(chunks))
	}

	wantContent := []int64{0, 2, 0}
	var contentTotal, deltaTotal int64
	for i, event := range chunkEvents {
		content := eventInt(t, event, "contentDeltas")
		if content != wantContent[i] {
			t.Errorf("chunk %d contentDeltas = %d, want %d", i, content, wantContent[i])
		}
		contentTotal += content
		deltaTotal += eventInt(t, event, "deltaTokens")
	}
	if contentTotal != 2 {
		t.Errorf("Σ contentDeltas = %d, want 2 (content_block_delta lines)", contentTotal)
	}
	// §23/§24.6 semantics unchanged: tokens still come from usage.
	if deltaTotal != 2 {
		t.Errorf("Σ deltaTokens = %d, want 2 (usage.output_tokens)", deltaTotal)
	}
}

// TestChunkEventContentDeltasOpenAIOverWS covers §24.9 Z5/Z5b end to end:
// role-only, finish_reason and [DONE] chunks carry no content, while content
// chunks do.
func TestChunkEventContentDeltasOpenAIOverWS(t *testing.T) {
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	}
	backend := monitorSSEBackend(t, chunks)
	server, _ := newAdminServer(t, monitorOpenAIConfig(backend.URL, serviceToken, "gpt-x"))

	client, resp := dialMonitor(t, server.URL, monitorAdminQuery(), nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket handshake status = %d, want 101", resp.StatusCode)
	}

	stream := send(t, http.MethodPost, server.URL+"/v1/chat/completions",
		`{"model":"gpt-x","messages":[],"stream":true}`, withAuth(config.ProtocolOpenAI, nil))
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", stream.StatusCode)
	}
	_, _ = io.Copy(io.Discard, stream.Body)

	events := readEventsUntilEnd(t, client, 10*time.Second)
	var chunkEvents []map[string]any
	for _, event := range events {
		if eventType(t, event) == "chunk" {
			chunkEvents = append(chunkEvents, event)
		}
	}
	if len(chunkEvents) != len(chunks) {
		t.Fatalf("chunk events = %d, want %d (upstream trunks)", len(chunkEvents), len(chunks))
	}
	want := []int64{0, 1, 1, 0, 0}
	for i, event := range chunkEvents {
		if got := eventInt(t, event, "contentDeltas"); got != want[i] {
			t.Errorf("chunk %d contentDeltas = %d, want %d", i, got, want[i])
		}
	}
}
