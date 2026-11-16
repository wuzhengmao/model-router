package proxy

import (
	"strings"
	"testing"
)

// TestChunkContentDeltasCountsContentBlockDelta covers §24.8 Y6: contentDeltas
// counts content_block_delta events per feed window; heartbeats, block
// boundaries and usage-only frames must stay at 0.
func TestChunkContentDeltasCountsContentBlockDelta(t *testing.T) {
	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"usage\":{\"input_tokens\":7}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: ping\ndata: {\"type\":\"ping\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		"data: [DONE]\n\n",
	}
	wantPerChunk := []int{0, 0, 0, 1, 2, 0, 0, 0, 0}

	var state sseState
	total := 0
	for i, chunk := range chunks {
		before := state.contentDeltas
		state.feed([]byte(chunk))
		got := state.contentDeltas - before
		if got != wantPerChunk[i] {
			t.Errorf("chunk %d contentDeltas = %d, want %d", i, got, wantPerChunk[i])
		}
		total += got
	}

	wantTotal := strings.Count(strings.Join(chunks, ""), `"type":"content_block_delta"`)
	if total != wantTotal {
		t.Errorf("Σ contentDeltas = %d, want %d (actual content_block_delta lines)", total, wantTotal)
	}
	if wantTotal != 3 {
		t.Fatalf("test fixture has %d content_block_delta lines, want 3", wantTotal)
	}

	// Usage is still reported only by message_delta (§23 logging semantics).
	if !state.hasTokens || state.tokens != 5 {
		t.Errorf("tokens = %d (known=%v), want 5 (usage.output_tokens)", state.tokens, state.hasTokens)
	}
	if !state.hasIn || state.inTokens != 7 {
		t.Errorf("inTokens = %d (known=%v), want 7", state.inTokens, state.hasIn)
	}
}

// TestContentDeltasOpenAICriteria covers §24.9 Z5: a chunk counts once when it
// carries content, reasoning_content or non-empty tool-call arguments; role,
// finish_reason, empty deltas, [DONE] and keep-alive comments do not count.
func TestContentDeltasOpenAICriteria(t *testing.T) {
	cases := []struct {
		name string
		line string
		want int
	}{
		{"role only", `data: {"choices":[{"delta":{"role":"assistant"}}]}`, 0},
		{"empty content", `data: {"choices":[{"delta":{"content":""}}]}`, 0},
		{"empty reasoning", `data: {"choices":[{"delta":{"reasoning_content":""}}]}`, 0},
		{"content", `data: {"choices":[{"delta":{"content":"hello"}}]}`, 1},
		{"reasoning", `data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, 1},
		{"tool arguments", `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"x\"}"}}]}}]}`, 1},
		{"empty tool arguments", `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":""}}]}}]}`, 0},
		{"content plus reasoning plus tool", `data: {"choices":[{"delta":{"content":"a","reasoning_content":"b","tool_calls":[{"function":{"arguments":"{}"}}]}}]}`, 1},
		{"finish reason", `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, 0},
		{"empty choices", `data: {"choices":[],"usage":{"completion_tokens":9}}`, 0},
		{"done", `data: [DONE]`, 0},
		{"keep alive", `: keep-alive`, 0},
	}
	var state sseState
	for _, tc := range cases {
		before := state.contentDeltas
		state.feed([]byte(tc.line + "\n\n"))
		if got := state.contentDeltas - before; got != tc.want {
			t.Errorf("%s: contentDeltas = %d, want %d", tc.name, got, tc.want)
		}
	}
	// Usage from an OpenAI chunk still feeds tokens without counting content.
	if !state.hasTokens || state.tokens != 9 {
		t.Errorf("tokens = %d (known=%v), want 9 (usage.completion_tokens)", state.tokens, state.hasTokens)
	}
}
