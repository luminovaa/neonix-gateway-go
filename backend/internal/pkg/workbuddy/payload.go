package workbuddy

import "strings"

// NormalizePayload applies only the upstream quirks known for WorkBuddy. It
// never removes a non-empty caller message and does not alter CodeBuddy data.
func NormalizePayload(payload map[string]any) {
	if payload == nil {
		return
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return
	}
	messages = dropEmptyMessages(messages)
	messages = repairToolRounds(messages)
	if len(messages) == 0 || roleOf(messages[0]) != "system" {
		messages = append([]any{map[string]any{"role": "system", "content": "You are a helpful AI assistant."}}, messages...)
	}
	payload["messages"] = messages
}

func dropEmptyMessages(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || !emptyContent(message) || hasToolCalls(message) || roleOf(raw) == "tool" {
			out = append(out, raw)
		}
	}
	return out
}

func repairToolRounds(messages []any) []any {
	out := make([]any, 0, len(messages))
	for index := 0; index < len(messages); index++ {
		message, ok := messages[index].(map[string]any)
		if !ok || roleOf(message) != "assistant" || !hasToolCalls(message) {
			out = append(out, messages[index])
			continue
		}
		calls, ok := message["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			out = append(out, message)
			continue
		}
		end := index + 1
		results := make([]map[string]any, 0, len(calls))
		for end < len(messages) {
			next, nextOK := messages[end].(map[string]any)
			if !nextOK || roleOf(next) != "tool" {
				break
			}
			results = append(results, next)
			end++
		}
		if completeToolRound(calls, results) {
			out = append(out, message)
			for _, result := range results {
				out = append(out, result)
			}
			index = end - 1
			continue
		}
		delete(message, "tool_calls")
		if !emptyContent(message) {
			out = append(out, message)
		}
		index = end - 1
	}
	return out
}

func completeToolRound(calls []any, results []map[string]any) bool {
	if len(calls) == 0 || len(calls) != len(results) {
		return false
	}
	want := make(map[string]struct{}, len(calls))
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		id := toolID(call, "id", "tool_call_id")
		if !ok || id == "" {
			return false
		}
		want[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		id := toolID(result, "tool_call_id", "id")
		if id == "" {
			return false
		}
		if _, ok := want[id]; !ok {
			return false
		}
		if _, duplicate := seen[id]; duplicate {
			return false
		}
		seen[id] = struct{}{}
	}
	return len(want) == len(seen)
}

func toolID(message map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := message[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func roleOf(raw any) string {
	message, _ := raw.(map[string]any)
	role, _ := message["role"].(string)
	return strings.ToLower(strings.TrimSpace(role))
}

func hasToolCalls(message map[string]any) bool {
	value, found := message["tool_calls"]
	if !found || value == nil {
		return false
	}
	if calls, ok := value.([]any); ok {
		return len(calls) > 0
	}
	return true
}

func emptyContent(message map[string]any) bool {
	value, found := message["content"]
	if !found || value == nil {
		return true
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) == ""
	}
	if list, ok := value.([]any); ok {
		return len(list) == 0
	}
	return false
}
