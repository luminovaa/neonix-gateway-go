package workbuddy

import "testing"

func TestNormalizePayloadRepairsIncompleteToolRound(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call-1"}}},
	}}
	NormalizePayload(payload)
	messages := payload["messages"].([]any)
	if len(messages) != 2 || roleOf(messages[0]) != "system" || roleOf(messages[1]) != "user" {
		t.Fatalf("unexpected normalized messages: %#v", messages)
	}
}
