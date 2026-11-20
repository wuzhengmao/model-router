package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
)

// §24.26 M103–M109: when an Anthropic streaming response contains tool_use but
// no thinking block, an empty thinking block is injected before the final
// message_delta/message_stop. Existing events are forwarded byte-for-byte;
// only new events are inserted.
const maxThinkingEventBytes = 32 * 1024

type sseThinkingInjector struct {
	pending  []byte
	passthru bool
	injected bool
	instName string
}

// feed appends chunk and returns the bytes ready for the client. The decision
// uses sseState, which has already consumed the original chunk (§24.26 M109).
func (n *sseThinkingInjector) feed(chunk []byte, state *sseState) []byte {
	n.pending = append(n.pending, chunk...)
	var out []byte
	for {
		if n.passthru {
			end, ok := findSSEEventEnd(n.pending)
			if ok {
				out = append(out, n.pending[:end]...)
				n.pending = append(n.pending[:0], n.pending[end:]...)
				n.passthru = false
				continue
			}
			const keep = 3
			if len(n.pending) <= keep {
				return out
			}
			out = append(out, n.pending[:len(n.pending)-keep]...)
			n.pending = append(n.pending[:0], n.pending[len(n.pending)-keep:]...)
			return out
		}

		end, ok := findSSEEventEnd(n.pending)
		if ok {
			event := n.pending[:end]
			if n.shouldInject(event, state) {
				index := state.nextBlockIndex()
				out = append(out, thinkingInjectionBytes(index)...)
				n.injected = true
				log.Printf("thinking block injected: inst=%s tool_use=%d index=%d",
					n.instName, state.toolUseBlocks, index)
			}
			out = append(out, event...)
			n.pending = append(n.pending[:0], n.pending[end:]...)
			continue
		}
		if len(n.pending) <= maxThinkingEventBytes {
			return out
		}

		// Oversized event: forward it verbatim and resume at the next event
		// boundary. Tail events (message_delta/message_stop) are tiny, so this
		// only skips inspection of large content events.
		const keep = 3
		out = append(out, n.pending[:len(n.pending)-keep]...)
		n.pending = append(n.pending[:0], n.pending[len(n.pending)-keep:]...)
		n.passthru = true
		return out
	}
}

// flush returns the final incomplete event unchanged. No injection happens at
// EOF without a message_delta/message_stop boundary.
func (n *sseThinkingInjector) flush() []byte {
	out := n.pending
	n.pending = nil
	return out
}

func (n *sseThinkingInjector) shouldInject(event []byte, state *sseState) bool {
	if n.injected || state == nil {
		return false
	}
	if state.toolUseBlocks == 0 || state.thinkingBlocks > 0 {
		return false
	}
	switch sseEventJSONType(event) {
	case "message_delta", "message_stop":
		return true
	default:
		return false
	}
}

// thinkingInjectionBytes builds a paired thinking content_block_start/stop
// (§24.26 M104). The signature is the empty string by specification; §24.24
// normalization runs before injection and does not rewrite these bytes.
func thinkingInjectionBytes(index int) []byte {
	return []byte(fmt.Sprintf(
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n",
		index, index))
}

// sseEventJSONType returns the JSON "type" of the event's single data: line.
func sseEventJSONType(event []byte) string {
	payload, ok := sseDataPayload(event)
	if !ok {
		return ""
	}
	var value struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &value); err != nil {
		return ""
	}
	return value.Type
}

func sseDataPayload(event []byte) ([]byte, bool) {
	dataStart, dataEnd := -1, -1
	dataLines := 0
	lineStart := 0
	for lineStart < len(event) {
		lineEnd := bytes.IndexByte(event[lineStart:], '\n')
		next := len(event)
		if lineEnd < 0 {
			lineEnd = len(event)
		} else {
			lineEnd += lineStart
			next = lineEnd + 1
		}
		line := event[lineStart:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		switch {
		case bytes.HasPrefix(line, []byte("data:")):
			dataLines++
			if dataLines > 1 {
				return nil, false
			}
			dataStart = lineStart + len("data:")
			dataEnd = lineStart + len(line)
		case bytes.Equal(line, []byte("data")):
			dataLines++
			if dataLines > 1 {
				return nil, false
			}
			dataStart = lineStart + len("data")
			dataEnd = lineStart + len(line)
		}
		if next <= lineStart {
			break
		}
		lineStart = next
	}
	if dataLines != 1 {
		return nil, false
	}
	valStart := dataStart
	if valStart < dataEnd && event[valStart] == ' ' {
		valStart++
	}
	if valStart >= dataEnd {
		return nil, false
	}
	return event[valStart:dataEnd], true
}
