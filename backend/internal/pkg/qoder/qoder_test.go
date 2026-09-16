package qoder

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func BenchmarkBuildPayload(b *testing.B) {
	request := &apicompat.ChatCompletionsRequest{Model: "qr/Lite", Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage("\"hello\"")}}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := BuildPayload(request, "lite", "personal_standard"); err != nil {
			b.Fatal(err)
		}
	}
}

func TestBuildPayloadUsesActualModelAndStableSession(t *testing.T) {
	request := &apicompat.ChatCompletionsRequest{Model: "qr/Lite", Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage("\"hello\"")}}}
	first, err := BuildPayload(request, "lite", "personal_standard")
	require.NoError(t, err)
	second, err := BuildPayload(request, "lite", "personal_standard")
	require.NoError(t, err)
	require.Equal(t, "lite", first["model_config"].(map[string]any)["key"])
	require.Equal(t, first["session_id"], second["session_id"])
	require.NotEqual(t, first["request_id"], second["request_id"])
}

func TestBuildPayloadPreservesConversationToolsImagesAndMaxTokens(t *testing.T) {
	maxTokens := 1234
	toolIndex := 0
	request := &apicompat.ChatCompletionsRequest{
		Model:      "qr/Ultimate",
		MaxTokens:  &maxTokens,
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"read_file"}}`),
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name: "read_file", Description: "Read one file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}}},
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: json.RawMessage(`"follow the project rules"`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"inspect this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]`)},
			{Role: "assistant", ToolCalls: []apicompat.ChatToolCall{{Index: &toolIndex, ID: "call_1", Type: "function", Function: apicompat.ChatFunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"package main"`)},
		},
	}
	payload, err := BuildPayload(request, "ultimate", "personal_standard")
	require.NoError(t, err)
	require.Equal(t, 1234, payload["parameters"].(map[string]any)["max_tokens"])
	require.NotContains(t, payload, "tool_choice")
	require.Len(t, payload["tools"], 1)
	messages := payload["messages"].([]map[string]any)
	require.Equal(t, "system", messages[0]["role"])
	require.Contains(t, messages[0]["content"], "read_file: Read one file")
	require.Equal(t, "system", messages[1]["role"])
	require.Equal(t, "assistant", messages[3]["role"])
	require.Equal(t, "tool", messages[4]["role"])
	require.Equal(t, "call_1", messages[4]["tool_call_id"])
	require.Equal(t, []string{"data:image/png;base64,aGVsbG8="}, payload["image_urls"])
}

func TestBuildPayloadAcceptsAnthropicBase64ImageAndToolResult(t *testing.T) {
	anthropic := &apicompat.AnthropicRequest{
		Model: "qr/Ultimate", MaxTokens: 77,
		Messages: []apicompat.AnthropicMessage{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_1","name":"inspect","input":{"path":"x"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"YWJj"}}]`)},
		},
	}
	chat, err := apicompat.AnthropicToChatCompletionsRequest(anthropic)
	require.NoError(t, err)
	payload, err := BuildPayload(chat, "ultimate", "personal_standard")
	require.NoError(t, err)
	// The shared Anthropic bridge enforces its protocol minimum before Qoder sees it.
	require.Equal(t, 128, payload["parameters"].(map[string]any)["max_tokens"])
	require.Equal(t, []string{"data:image/jpeg;base64,YWJj"}, payload["image_urls"])
	messages := payload["messages"].([]map[string]any)
	require.Equal(t, "assistant", messages[0]["role"])
	require.Equal(t, "tool", messages[1]["role"])
	require.Equal(t, "user", messages[2]["role"])
}

func TestDecodeStreamSeparatesThinkingAndToolCalls(t *testing.T) {
	inner := "{\"choices\":[{\"delta\":{\"content\":\"<think>plan</think>answer\",\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"run\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}"
	wrapper, _ := json.Marshal(map[string]any{"body": inner})
	var events []Event
	err := DecodeStream(bytes.NewBufferString("data: "+string(wrapper)+"\n\n"), func(event Event) error { events = append(events, event); return nil })
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "answer", *events[0].Delta.Content)
	require.Equal(t, "plan", *events[0].Delta.ReasoningContent)
	require.Equal(t, "tool_calls", events[0].FinishReason)
	require.Equal(t, 3, events[0].Usage.InputTokens)
}

func TestDecodeStreamReturnsStructuredInlineError(t *testing.T) {
	var got *StreamError
	err := DecodeStream(bytes.NewBufferString("data: {\"statusCodeValue\":429,\"message\":\"quota\"}\n"), func(Event) error { return nil })
	require.ErrorAs(t, err, &got)
	require.Equal(t, 429, got.Status)
}
