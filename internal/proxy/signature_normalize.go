package proxy

import (
	"bytes"
	"encoding/json"
)

// §24.24 M83: fixed placeholder for a thinking content_block_start whose
// upstream signature is empty or missing. It is deliberately constant so the
// same history carries the same signature on every replay.
const placeholderThinkingSignature = "model-router-placeholder"

// maxSignatureEventBytes bounds how much of one SSE event the normalizer
// buffers before passing it through unchanged (§24.24 M88). It matches the
// 32 KiB chunk size copySSE already reads with.
const maxSignatureEventBytes = 32 * 1024

// sseSignatureNormalizer rewrites empty/missing thinking signatures in the
// bytes forwarded to the client. It buffers at most one event at a time; a
// feed returns the bytes that are ready to send. Nothing here touches the
// upstream body or the sseState parser (§24.24 M85).
type sseSignatureNormalizer struct {
	pending  []byte
	passthru bool
	blocks   int
}

// feed appends chunk and returns the bytes that can be forwarded now. Events
// are forwarded whole; an incomplete event stays pending. A complete event
// that cannot be parsed or is oversized is forwarded verbatim (M86/M88).
func (n *sseSignatureNormalizer) feed(chunk []byte) []byte {
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
			// Keep a short tail so a blank line split across reads is still
			// recognised; bytes are forwarded in order either way.
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
			if rewritten, hit := normalizeThinkingSignatureEvent(event); hit {
				n.blocks++
				out = append(out, rewritten...)
			} else {
				out = append(out, event...)
			}
			n.pending = append(n.pending[:0], n.pending[end:]...)
			continue
		}
		if len(n.pending) <= maxSignatureEventBytes {
			return out
		}

		// Oversized event: stop inspecting it, forward verbatim, and resume
		// at the next event boundary.
		const keep = 3
		out = append(out, n.pending[:len(n.pending)-keep]...)
		n.pending = append(n.pending[:0], n.pending[len(n.pending)-keep:]...)
		n.passthru = true
		return out
	}
}

// flush returns the bytes of the final incomplete event so copySSE can forward
// them verbatim before returning (M86: no byte may be dropped).
func (n *sseSignatureNormalizer) flush() []byte {
	out := n.pending
	n.pending = nil
	return out
}

// findSSEEventEnd returns the offset just past the first blank line in b.
// Lines end with LF or CRLF; a lone CR is not treated as a terminator, matching
// sseState's line splitting.
func findSSEEventEnd(b []byte) (int, bool) {
	lineStart := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		lineEnd := i
		if lineEnd > lineStart && b[lineEnd-1] == '\r' {
			lineEnd--
		}
		if lineEnd == lineStart {
			return i + 1, true
		}
		lineStart = i + 1
	}
	return 0, false
}

// normalizeThinkingSignatureEvent rewrites a single complete SSE event. Only
// events with exactly one data: line are candidates; everything else is left
// untouched (M86). The returned slice is a new event with only the data JSON's
// signature bytes changed.
func normalizeThinkingSignatureEvent(event []byte) ([]byte, bool) {
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

	// SSE strips at most one leading space after the field colon.
	valStart := dataStart
	if valStart < dataEnd && event[valStart] == ' ' {
		valStart++
	}
	if valStart >= dataEnd {
		return nil, false
	}
	payload, ok := normalizeThinkingSignaturePayload(event[valStart:dataEnd])
	if !ok {
		return nil, false
	}
	out := make([]byte, 0, len(event)-(dataEnd-valStart)+len(payload))
	out = append(out, event[:valStart]...)
	out = append(out, payload...)
	out = append(out, event[dataEnd:]...)
	return out, true
}

// normalizeThinkingSignaturePayload rewrites the signature inside a
// content_block_start JSON payload. The bytes outside the signature value are
// preserved exactly, so field order and every other field survive (M87).
func normalizeThinkingSignaturePayload(payload []byte) ([]byte, bool) {
	if !json.Valid(payload) {
		return nil, false
	}
	start := skipJSONSpace(payload, 0)
	if start >= len(payload) || payload[start] != '{' {
		return nil, false
	}
	top, _, ok := scanJSONObject(payload, start)
	if !ok {
		return nil, false
	}

	eventType, haveEventType := "", false
	blockStart, blockEnd, haveBlock := 0, 0, false
	for _, member := range top {
		switch member.key {
		case "type":
			if value, ok := jsonStringValue(payload[member.valStart:member.valEnd]); ok {
				eventType, haveEventType = value, true
			}
		case "content_block":
			blockStart, blockEnd, haveBlock = member.valStart, member.valEnd, true
		}
	}
	if !haveEventType || eventType != "content_block_start" || !haveBlock {
		return nil, false
	}
	if blockStart >= blockEnd || payload[blockStart] != '{' {
		return nil, false
	}
	members, _, ok := scanJSONObject(payload, blockStart)
	if !ok {
		return nil, false
	}

	blockType, haveBlockType := "", false
	signature := -1
	for i, member := range members {
		switch member.key {
		case "type":
			if value, ok := jsonStringValue(payload[member.valStart:member.valEnd]); ok {
				blockType, haveBlockType = value, true
			}
		case "signature":
			signature = i
		}
	}
	if !haveBlockType || blockType != "thinking" {
		return nil, false
	}

	if signature >= 0 {
		member := members[signature]
		if member.valEnd-member.valStart != 2 ||
			payload[member.valStart] != '"' || payload[member.valStart+1] != '"' {
			// Non-empty or non-string signature: leave the event untouched.
			return nil, false
		}
		out := make([]byte, 0, len(payload)+len(placeholderThinkingSignature))
		out = append(out, payload[:member.valStart]...)
		out = append(out, '"')
		out = append(out, placeholderThinkingSignature...)
		out = append(out, '"')
		out = append(out, payload[member.valEnd:]...)
		return out, true
	}

	// Missing signature: insert it as the first member of content_block. The
	// block already has at least "type", so the trailing comma is valid.
	insert := []byte(`"signature":"` + placeholderThinkingSignature + `",`)
	out := make([]byte, 0, len(payload)+len(insert))
	out = append(out, payload[:blockStart+1]...)
	out = append(out, insert...)
	out = append(out, payload[blockStart+1:]...)
	return out, true
}

type jsonMemberSpan struct {
	key      string
	valStart int
	valEnd   int
}

// scanJSONObject returns the members of the object starting at start. Values are
// skipped iteratively, so deeply nested payloads cannot exhaust the stack.
func scanJSONObject(b []byte, start int) ([]jsonMemberSpan, int, bool) {
	i := skipJSONSpace(b, start)
	if i >= len(b) || b[i] != '{' {
		return nil, 0, false
	}
	i++
	var members []jsonMemberSpan
	i = skipJSONSpace(b, i)
	if i < len(b) && b[i] == '}' {
		return members, i + 1, true
	}
	for {
		i = skipJSONSpace(b, i)
		if i >= len(b) || b[i] != '"' {
			return nil, 0, false
		}
		keyEnd, ok := scanJSONString(b, i)
		if !ok {
			return nil, 0, false
		}
		var key string
		if err := json.Unmarshal(b[i:keyEnd], &key); err != nil {
			return nil, 0, false
		}
		i = skipJSONSpace(b, keyEnd)
		if i >= len(b) || b[i] != ':' {
			return nil, 0, false
		}
		i = skipJSONSpace(b, i+1)
		valStart := i
		valEnd, ok := scanJSONValue(b, i)
		if !ok {
			return nil, 0, false
		}
		members = append(members, jsonMemberSpan{key: key, valStart: valStart, valEnd: valEnd})
		i = skipJSONSpace(b, valEnd)
		if i >= len(b) {
			return nil, 0, false
		}
		switch b[i] {
		case ',':
			i++
		case '}':
			return members, i + 1, true
		default:
			return nil, 0, false
		}
	}
}

func scanJSONValue(b []byte, i int) (int, bool) {
	if i >= len(b) {
		return 0, false
	}
	switch b[i] {
	case '"':
		return scanJSONString(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				end, ok := scanJSONString(b, j)
				if !ok {
					return 0, false
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
		}
		return 0, false
	default:
		j := i
		for j < len(b) && !isJSONDelimiter(b[j]) {
			j++
		}
		if j == i {
			return 0, false
		}
		return j, true
	}
}

func scanJSONString(b []byte, i int) (int, bool) {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

func isJSONDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', ',', '}', ']':
		return true
	}
	return false
}

func jsonStringValue(raw []byte) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}
