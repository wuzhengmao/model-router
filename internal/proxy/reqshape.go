package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// §24.22 M63–M69: a structural summary of the client request body, recorded
// only when the request ends in failure. It never carries message text — just
// the message count, each role, each content block-type sequence and whether an
// assistant message pairs tool_use with thinking.
const (
	reqShapeLimit           = 1024
	reqShapeEllipsis        = "…"
	reqShapeUnparsableTag   = "unparsable: "
	reqShapeUnparsableRunes = 64
	reqShapeToolUseFlag     = "!!tool_use_without_thinking"
	// shapeStatsLimit bounds the counting pass so a pathological body cannot
	// make the summary quadratic (H1).
	shapeStatsLimit = 100000
)

// reqShapeMessage is the subset of a chat message the summary needs. Unknown
// fields are ignored; nothing here is ever logged as text.
type reqShapeMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ToolCalls        json.RawMessage `json:"tool_calls"`
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	Reasoning        json.RawMessage `json:"reasoning"`
}

// buildReqShape renders the §24.22 summary:
//
//	msgs=4 | 0:user[text] | 1:assistant[thinking,text,tool_use] | ...
//
// The result is capped at reqShapeLimit bytes: oversized summaries keep whole
// message segments and end with reqShapeEllipsis (M65). A body that is not JSON
// or not a chat-style {"messages": [...]} envelope yields an "unparsable:"
// prefix instead of an error (M67).
// requestMayHaveToolUse is a cheap negative check before the full stats parse:
// bodies without any tool_use (Anthropic) / tool_calls (OpenAI) marker cannot
// contain a tool_use message. False positives only cost one extra parse.
func requestMayHaveToolUse(body []byte) bool {
	return bytes.Contains(body, []byte(`"tool_use"`)) || bytes.Contains(body, []byte(`"tool_calls"`))
}

// reqShapeSummary is the whole-history summary used by both the reqShape suffix
// and the §24.26 M116 request-shape log line.
type reqShapeSummary struct {
	Messages     int
	ToolUseMsgs  int
	PairedMsgs   int
	Unpaired     []int
	UnpairedMore bool
	Declared     string
	Last         string
	Session      string
}

func buildReqShape(proto protocol, body []byte) string {
	return buildReqShapeWithStats(proto, body, computeReqShapeStats(proto, body))
}

func buildReqShapeWithStats(proto protocol, body []byte, stats reqShapeSummary) string {
	segments, ok := reqShapeSegments(proto, body)
	if !ok {
		return reqShapeUnparsable(body)
	}
	// H1: the whole-history counters are appended last but budgeted first, so
	// the total stays within reqShapeLimit.
	suffix := shapeSuffix(stats)
	var b strings.Builder
	b.WriteString("msgs=")
	b.WriteString(strconv.Itoa(len(segments)))
	// Reserve room for the ellipsis and the counter suffix.
	budget := reqShapeLimit - len(reqShapeEllipsis) - len(suffix)
	truncated := false
	for _, segment := range segments {
		piece := " | " + segment
		if b.Len()+len(piece) > budget {
			truncated = true
			break
		}
		b.WriteString(piece)
	}
	if truncated {
		b.WriteString(reqShapeEllipsis)
	}
	b.WriteString(suffix)
	return b.String()
}

// shapeSuffix renders the M114 counters. tool_use_msgs/paired_msgs count
// assistant messages (not blocks); a message paired with redacted_thinking
// counts as paired. unpaired_idx lists up to ten unpaired message indices and
// appends "+" when the list was cut.
func shapeSuffix(stats reqShapeSummary) string {
	return " || tool_use_msgs=" + strconv.Itoa(stats.ToolUseMsgs) +
		" paired_msgs=" + strconv.Itoa(stats.PairedMsgs) +
		" unpaired_idx=" + formatUnpaired(stats) +
		" thinking_declared=" + stats.Declared
}

func formatUnpaired(stats reqShapeSummary) string {
	if len(stats.Unpaired) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(stats.Unpaired)+1)
	for _, index := range stats.Unpaired {
		parts = append(parts, strconv.Itoa(index))
	}
	if stats.UnpairedMore {
		parts = append(parts, "+")
	}
	return strings.Join(parts, ",")
}

// computeReqShapeStats walks the full message array once. It is used for the
// reqShape suffix (failures) and for the §24.26 M116 log line (requests that
// contain tool_use, successful or not).
func computeReqShapeStats(proto protocol, body []byte) reqShapeSummary {
	stats := reqShapeSummary{Declared: "absent", Last: "-", Session: "-"}
	var envelope struct {
		Messages json.RawMessage `json:"messages"`
		Thinking json.RawMessage `json:"thinking"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		stats.Declared = "unknown"
		return stats
	}
	if len(envelope.Thinking) > 0 {
		stats.Declared = thinkingDeclaration(envelope.Thinking)
	}
	stats.Session = sessionHash(envelope.Metadata)
	if len(envelope.Messages) == 0 {
		return stats
	}
	var messages []reqShapeMessage
	if err := json.Unmarshal(envelope.Messages, &messages); err != nil {
		return stats
	}
	stats.Messages = len(messages)
	for i, msg := range messages {
		if i >= shapeStatsLimit {
			break
		}
		hasTool, hasThinking := assistantShapeFlags(proto, msg)
		if !hasTool {
			continue
		}
		stats.ToolUseMsgs++
		if hasThinking {
			stats.PairedMsgs++
			continue
		}
		if len(stats.Unpaired) < 10 {
			stats.Unpaired = append(stats.Unpaired, i)
		} else {
			stats.UnpairedMore = true
		}
	}
	stats.Last = lastMessageType(proto, messages)
	return stats
}

// lastMessageType names the last message by its most actionable property: a
// tool_result (Anthropic block or OpenAI role "tool"), otherwise the role.
func lastMessageType(proto protocol, messages []reqShapeMessage) string {
	if len(messages) == 0 {
		return "-"
	}
	last := messages[len(messages)-1]
	if proto == protocolOpenAI && last.Role == "tool" {
		return "tool_result"
	}
	if types, ok := reqShapeBlockTypes(proto, last); ok && contains(types, "tool_result") {
		return "tool_result"
	}
	if last.Role == "" {
		return "unknown"
	}
	return last.Role
}

// sessionHash extracts metadata.user_id (a JSON string containing JSON) and
// hashes its session_id to 8 lowercase hex characters (§24.26 M116/M120). Any
// missing or malformed layer yields "-"; it never panics.
func sessionHash(metadata json.RawMessage) string {
	if len(metadata) == 0 {
		return "-"
	}
	var meta struct {
		UserID json.RawMessage `json:"user_id"`
	}
	if err := json.Unmarshal(metadata, &meta); err != nil {
		return "-"
	}
	var userID string
	if err := json.Unmarshal(meta.UserID, &userID); err != nil || userID == "" {
		return "-"
	}
	var inner struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(userID), &inner); err != nil || inner.SessionID == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(inner.SessionID))
	return fmt.Sprintf("%x", sum[:4])
}

// thinkingDeclaration reads the request-level thinking parameter: an object
// with type "enabled"/"disabled", or a bare boolean.
func thinkingDeclaration(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "absent"
	}
	if trimmed[0] == '{' {
		var value struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "unknown"
		}
		if value.Type == "" {
			return "unknown"
		}
		return value.Type
	}
	var flag bool
	if err := json.Unmarshal(trimmed, &flag); err != nil {
		return "unknown"
	}
	if flag {
		return "enabled"
	}
	return "disabled"
}

// reqShapeSegments decodes the envelope and renders one segment per message. It
// reports false when the body does not match the expected chat message shape;
// the caller then falls back to the unparsable summary.
func reqShapeSegments(proto protocol, body []byte) ([]string, bool) {
	var envelope struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false
	}
	messages, ok := decodeJSONArray(envelope.Messages)
	if !ok {
		return nil, false
	}
	segments := make([]string, 0, len(messages))
	for i, raw := range messages {
		var msg reqShapeMessage
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Role == "" {
			return nil, false
		}
		types, ok := reqShapeBlockTypes(proto, msg)
		if !ok {
			return nil, false
		}
		segment := strconv.Itoa(i) + ":" + msg.Role + "[" + strings.Join(types, ",") + "]"
		if msg.Role == "assistant" && contains(types, "tool_use") &&
			!contains(types, "thinking") && !contains(types, "redacted_thinking") {
			segment += reqShapeToolUseFlag
		}
		segments = append(segments, segment)
	}
	return segments, true
}

// reqShapeBlockTypes returns the message's content block-type sequence. A
// string content counts as a single "text" block. For OpenAI assistant messages
// reasoning_content/reasoning count as "thinking" and a non-empty tool_calls
// array counts as "tool_use" (§24.22.2).
// assistantShapeFlags reports whether the message carries tool_use and whether
// it carries a usable thinking block. §24.26 M114 counts redacted_thinking as
// paired; otherwise a redacted block would look like the 400-triggering gap.
func assistantShapeFlags(proto protocol, msg reqShapeMessage) (hasTool, hasThinking bool) {
	if msg.Role != "assistant" {
		return false, false
	}
	types, ok := reqShapeBlockTypes(proto, msg)
	if !ok {
		return false, false
	}
	hasThinking = contains(types, "thinking") || contains(types, "redacted_thinking")
	return contains(types, "tool_use"), hasThinking
}

func reqShapeBlockTypes(proto protocol, msg reqShapeMessage) ([]string, bool) {
	types := make([]string, 0, 4)
	content := bytes.TrimSpace(msg.Content)
	switch {
	case len(content) == 0 || bytes.Equal(content, []byte("null")):
	case content[0] == '"':
		var text string
		if err := json.Unmarshal(content, &text); err != nil {
			return nil, false
		}
		types = append(types, "text")
	case content[0] == '[':
		blocks, ok := decodeJSONArray(content)
		if !ok {
			return nil, false
		}
		for _, raw := range blocks {
			var block struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &block); err != nil || block.Type == "" {
				return nil, false
			}
			types = append(types, block.Type)
		}
	default:
		return nil, false
	}

	if proto != protocolOpenAI || msg.Role != "assistant" {
		return types, true
	}
	if hasJSONValue(msg.ReasoningContent) || hasJSONValue(msg.Reasoning) {
		types = append([]string{"thinking"}, types...)
	}
	toolCalls, ok := hasOpenAIToolCalls(msg.ToolCalls)
	if !ok {
		return nil, false
	}
	if toolCalls {
		types = append(types, "tool_use")
	}
	return types, true
}

// decodeJSONArray parses raw as a JSON array. Missing, null and non-array
// values are rejected so the caller can fall back to the unparsable summary.
func decodeJSONArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, false
	}
	return items, true
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// hasOpenAIToolCalls reports whether tool_calls carries at least one call. A
// malformed non-array value is rejected.
func hasOpenAIToolCalls(raw json.RawMessage) (present bool, ok bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false, true
	}
	var calls []json.RawMessage
	if err := json.Unmarshal(trimmed, &calls); err != nil {
		return false, false
	}
	return len(calls) > 0, true
}

// reqShapeUnparsable is the M67 fallback: a short prefix of the raw body, with
// invalid UTF-8 replaced so the summary stays printable.
func reqShapeUnparsable(body []byte) string {
	runes := []rune(strings.ToValidUTF8(string(body), "�"))
	if len(runes) > reqShapeUnparsableRunes {
		runes = runes[:reqShapeUnparsableRunes]
	}
	return reqShapeUnparsableTag + string(runes)
}
