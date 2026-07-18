package opencode

import (
	"encoding/json"
	"strings"
)

func firstText(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	texts := collectText(nil, v)
	if len(texts) == 0 {
		return ""
	}
	// Return the last text — the final assistant response, not intermediate parts.
	return texts[len(texts)-1]
}

// finalAssistantAnswerText returns only completed assistant answer text from an
// OpenCode message list. It never falls back to user prompts, and skips
// reasoning/thinking/tool parts when part types are present.
//
// When requireFinished is true, the chosen assistant message must expose a
// non-empty finish field (session still running / incomplete turn → "").
func finalAssistantAnswerText(raw json.RawMessage, requireFinished bool) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	var lastAnswer string
	for _, item := range collectObjects(nil, v) {
		if messageRole(item) != "assistant" {
			continue
		}
		if requireFinished && !messageFinished(item) {
			continue
		}
		if answer := assistantAnswerText(item); answer != "" {
			lastAnswer = answer
		}
	}
	return lastAnswer
}

// shapeHandoffCheckResult centralizes handoff_check final_text/messages policy
// so status and verbosity cannot diverge across checkSession/doCheck callers.
//
// Rules:
//   - verbose=false → omit messages for every status
//   - verbose=true  → bounded raw messages (including while running)
//   - running/unknown → never set final_text
//   - done → final_text from final assistant answer text only
//   - error/canceled/timed_out → final_text only when a finished assistant answer exists
//   - untracked uses the same rules via status done|running from isFinished
func shapeHandoffCheckResult(res HandoffCheckResult, msg json.RawMessage, verbose bool, isFinished bool) HandoffCheckResult {
	res.Messages = nil
	res.FinalText = ""

	if verbose && len(msg) > 0 {
		res.Messages = summarizeMessages(msg, 6)
	}

	switch JobStatus(res.Status) {
	case JobRunning, JobUnknown:
		return res
	case JobDone:
		if len(msg) > 0 {
			// Job/session completion is authoritative; still never use user/tool/reasoning text.
			res.FinalText = truncateText(finalAssistantAnswerText(msg, false), 4000)
		}
		return res
	case JobError, JobCanceled, JobTimedOut:
		if len(msg) > 0 {
			res.FinalText = truncateText(finalAssistantAnswerText(msg, true), 4000)
		}
		return res
	default:
		// Untracked / unknown status strings: only emit final_text when finished.
		if isFinished && len(msg) > 0 {
			res.FinalText = truncateText(finalAssistantAnswerText(msg, true), 4000)
		}
		return res
	}
}

func messageFinished(m map[string]any) bool {
	if finish := stringField(m, "finish"); finish != "" {
		return true
	}
	if info, ok := m["info"].(map[string]any); ok {
		if finish := stringField(info, "finish"); finish != "" {
			return true
		}
	}
	return false
}

func assistantAnswerText(m map[string]any) string {
	parts := assistantPartValues(m)
	if len(parts) == 0 {
		// Flat/legacy shapes without parts: take direct text only, never nested tool blocks.
		if text := strings.TrimSpace(stringField(m, "text")); text != "" {
			if typ := stringField(m, "type"); typ == "" || isAnswerPartType(typ) {
				return text
			}
		}
		return ""
	}
	var texts []string
	for _, part := range parts {
		if !isAnswerPart(part) {
			continue
		}
		if text := strings.TrimSpace(stringField(part, "text")); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func assistantPartValues(m map[string]any) []map[string]any {
	for _, key := range []string{"parts", "content"} {
		raw, ok := m[key]
		if !ok {
			continue
		}
		arr, ok := raw.([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(arr))
		for _, child := range arr {
			if part, ok := child.(map[string]any); ok {
				out = append(out, part)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func isAnswerPart(part map[string]any) bool {
	typ := stringField(part, "type")
	if typ == "" {
		// Untyped part with text is treated as answer text only when it is not a tool payload.
		if stringField(part, "tool") != "" || part["state"] != nil || part["input"] != nil || part["output"] != nil {
			return false
		}
		return strings.TrimSpace(stringField(part, "text")) != ""
	}
	if !isAnswerPartType(typ) {
		return false
	}
	// Drop incomplete fragments when the API exposes a non-terminal part status.
	if status := partStatus(part); status != "" && !isTerminalPartStatus(status) {
		return false
	}
	return true
}

func isAnswerPartType(typ string) bool {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "text", "output_text", "answer":
		return true
	default:
		return false
	}
}

func partStatus(part map[string]any) string {
	if status := stringField(part, "status"); status != "" {
		return status
	}
	if state, ok := part["state"].(map[string]any); ok {
		return stringField(state, "status")
	}
	return ""
}

func isTerminalPartStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "done", "finished", "success", "succeeded":
		return true
	default:
		return false
	}
}

func summarizeMessages(raw json.RawMessage, limit int) []MessageSummary {
	if limit <= 0 {
		limit = 6
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []MessageSummary{}
	}
	// Only include role-bearing message objects, not individual parts.
	items := collectObjects(nil, v)
	var roleItems []map[string]any
	for _, item := range items {
		if messageRole(item) != "" {
			roleItems = append(roleItems, item)
		}
	}
	if len(roleItems) > limit {
		roleItems = roleItems[len(roleItems)-limit:]
	}
	out := make([]MessageSummary, 0, len(roleItems))
	for _, item := range roleItems {
		text := strings.Join(collectText(nil, item), "\n")
		out = append(out, MessageSummary{
			ID:   messageID(item),
			Role: messageRole(item),
			Text: compactText(text),
		})
	}
	return out
}

func summarizeRequests(raw json.RawMessage, kind, sessionID string) []RequestSummary {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []RequestSummary{}
	}
	items := collectObjects(nil, v)
	out := make([]RequestSummary, 0, len(items))
	for _, item := range items {
		itemSessionID := firstStringField(item, "sessionID", "session_id")
		if sessionID != "" && itemSessionID != "" && itemSessionID != sessionID {
			continue
		}
		text := strings.Join(collectText(nil, item), "\n")
		out = append(out, RequestSummary{
			Kind:    kind,
			Title:   firstStringField(item, "title", "tool", "type", "action"),
			Text:    text,
			Preview: preview(text),
		})
	}
	return out
}

func extractMessageID(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	for _, obj := range collectObjects(nil, v) {
		if id := firstStringField(obj, "messageID", "message_id", "id"); id != "" {
			return id
		}
	}
	return ""
}

func collectText(out []string, v any) []string {
	switch x := v.(type) {
	case map[string]any:
		if text, ok := x["text"].(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
		for _, key := range []string{"content", "parts", "message", "messages", "data", "tool_result", "tool_use", "input", "output"} {
			if child, ok := x[key]; ok {
				out = collectText(out, child)
			}
		}
	case []any:
		for _, child := range x {
			out = collectText(out, child)
		}
	}
	return out
}

func collectObjects(out []map[string]any, v any) []map[string]any {
	switch x := v.(type) {
	case map[string]any:
		if x["id"] != nil || x["role"] != nil ||
			x["requestID"] != nil || x["messageID"] != nil ||
			x["request_id"] != nil || x["message_id"] != nil ||
			messageRole(x) != "" {
			out = append(out, x)
		}
		for _, key := range []string{"data", "items", "messages", "requests", "permissions", "questions", "content", "parts"} {
			if child, ok := x[key]; ok {
				out = collectObjects(out, child)
			}
		}
	case []any:
		for _, child := range x {
			out = collectObjects(out, child)
		}
	}
	return out
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func firstStringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v := stringField(m, key); v != "" {
			return v
		}
	}
	return ""
}

func messageID(m map[string]any) string {
	if id := stringField(m, "id"); id != "" {
		return id
	}
	if info, ok := m["info"].(map[string]any); ok {
		return stringField(info, "id")
	}
	return ""
}

func messageRole(m map[string]any) string {
	if role := stringField(m, "role"); role != "" {
		return role
	}
	if info, ok := m["info"].(map[string]any); ok {
		return stringField(info, "role")
	}
	return ""
}

func preview(s string) string {
	return truncateFields(s, 240)
}

func compactText(s string) string {
	return truncateFields(s, 1200)
}

func truncateText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

func truncateFields(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
