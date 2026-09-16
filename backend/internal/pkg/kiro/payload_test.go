package kiro

import (
	"encoding/json"
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestBuildChatPayloadPreservesToolsAndAlternation(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Model: "claude-sonnet",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: json.RawMessage(`"be concise"`)},
			{Role: "user", Content: json.RawMessage(`"inspect it"`)},
			{Role: "assistant", Content: json.RawMessage(`null`), ToolCalls: []apicompat.ChatToolCall{{ID: "call_1", Type: "function", Function: apicompat.ChatFunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"package main"`)},
		},
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	payload, err := BuildChatPayload(req, "actual-model", BuilderIDProfileARN, "AI_EDITOR")
	require.NoError(t, err)
	require.Equal(t, "actual-model", payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)
	require.Contains(t, payload.ConversationState.History[0].UserInputMessage.Content, "be concise")
	require.Len(t, payload.ConversationState.History[1].AssistantResponseMessage.ToolUses, 1)
	require.Len(t, payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults, 1)
	require.Len(t, payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools, 1)
}

func TestBuildChatPayloadSupportsDataImage(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{Model: "m", Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]`)}}}
	payload, err := BuildChatPayload(req, "m", "", "AI_EDITOR")
	require.NoError(t, err)
	require.Equal(t, "AAAA", payload.ConversationState.CurrentMessage.UserInputMessage.Images[0].Source.Bytes)
}

func BenchmarkBuildChatPayload(b *testing.B) {
	req := &apicompat.ChatCompletionsRequest{
		Model: "claude-sonnet",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: json.RawMessage(`"Answer with concise code."`)},
			{Role: "user", Content: json.RawMessage(`"Inspect the implementation and propose a fix."`)},
			{Role: "assistant", Content: json.RawMessage(`"I will inspect it."`)},
			{Role: "user", Content: json.RawMessage(`"Continue."`)},
		},
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name: "read_file", Description: "Read one file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}}},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := BuildChatPayload(req, "CLAUDE_SONNET_4_20250514_V1_0", BuilderIDProfileARN, "AI_EDITOR"); err != nil {
			b.Fatal(err)
		}
	}
}
