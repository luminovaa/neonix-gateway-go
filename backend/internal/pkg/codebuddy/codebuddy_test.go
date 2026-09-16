package codebuddy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestProtocolHostsURLsAndCLIHeaders(t *testing.T) {
	t.Parallel()

	require.Equal(t, AIHosts, ResolveHosts(""))
	require.Equal(t, AIHosts, ResolveHosts("www.codebuddy.ai"))
	require.Equal(t, WorkBuddyHosts, ResolveHosts("www.workbuddy.ai"))
	require.Equal(t, CNHosts, ResolveHosts("copilot.tencent.com"))
	require.True(t, IsChinaRealm("www.codebuddy.cn"))
	require.False(t, IsChinaRealm("www.codebuddy.ai"))
	require.Equal(t, "https://www.codebuddy.ai/v2/plugin/auth/state?platform=CLI", DeviceStartURL(AIHosts))
	require.Equal(t, "https://www.codebuddy.ai/v2/plugin/auth/token?state=a+b%26c", DevicePollURL(AIHosts, "a b&c"))
	require.Equal(t, "https://www.codebuddy.ai/v2/plugin/login/account?state=a+b%26c", DeviceAccountURL(AIHosts, "a b&c"))
	require.Equal(t, "https://www.codebuddy.ai/v2/plugin/auth/token/refresh", RefreshURL(AIHosts))
	require.Equal(t, "https://www.codebuddy.ai/v2/chat/completions", ChatURL(AIHosts))

	headers := CLIHeaders(Identity{AccessToken: "access-secret", RefreshToken: "refresh-must-not-leak", UserID: "user-1", EnterpriseID: "enterprise-1", Domain: "www.codebuddy.ai", AuthMark: CLIAuthMark})
	require.Equal(t, "Bearer access-secret", headers.Get("Authorization"))
	require.Equal(t, "user-1", headers.Get("X-User-Id"))
	require.Equal(t, "enterprise-1", headers.Get("X-Enterprise-Id"))
	require.Equal(t, "www.codebuddy.ai", headers.Get("X-Domain"))
	require.NotContains(t, headers, "X-Refresh-Token")
	for _, values := range headers {
		require.NotContains(t, strings.Join(values, " "), "refresh-must-not-leak")
	}
}

func TestEnvelopeTokensAndAccountParsing(t *testing.T) {
	t.Parallel()

	envelope, err := ParseEnvelope([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"access","refreshToken":"refresh","expiresIn":1800,"domain":"www.codebuddy.ai"}}`))
	require.NoError(t, err)
	tokens, err := ParseTokens(envelope.Data)
	require.NoError(t, err)
	require.Equal(t, "access", tokens.AccessToken)
	require.Equal(t, "refresh", tokens.RefreshToken)
	require.Equal(t, int64(1800), tokens.ExpiresIn)

	_, err = ParseEnvelope([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	require.Error(t, err)
	_, err = ParseTokens(json.RawMessage(`{"refreshToken":"refresh-only"}`))
	require.Error(t, err)

	info := ParseAccountInfo(json.RawMessage(`{"uid":"uid-1","enterpriseId":"ent-1","nickname":"Owner"}`))
	require.Equal(t, AccountInfo{UserID: "uid-1", EnterpriseID: "ent-1", Nickname: "Owner"}, info)
}

func TestBuildPayloadPreservesMultimodalToolsReasoningAndResolvesSchema(t *testing.T) {
	t.Parallel()

	maxTokens := 64000
	temperature := 0.2
	topP := 0.8
	request := &apicompat.ChatCompletionsRequest{
		Messages:  []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"high"}}]`)}},
		MaxTokens: &maxTokens, Temperature: &temperature, TopP: &topP, ReasoningEffort: "high",
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name: "inspect", Parameters: json.RawMessage(`{"$defs":{"Input":{"type":"object","properties":{"path":{"type":"string"}}}},"$ref":"#/$defs/Input"}`),
		}}},
	}
	payload, err := BuildPayload(request, "cb/gemini-3.1-pro-thinking")
	require.NoError(t, err)
	require.Equal(t, "gemini-3.1-pro", payload["model"])
	require.Equal(t, 32000, payload["max_tokens"])
	require.Equal(t, true, payload["stream"])
	require.Equal(t, map[string]any{"effort": "high"}, payload["reasoning"])

	messages := payload["messages"].([]any)
	require.Len(t, messages, 2)
	require.Equal(t, "system", messages[0].(map[string]any)["role"])
	userContent := messages[1].(map[string]any)["content"].([]any)
	require.Equal(t, "image_url", userContent[1].(map[string]any)["type"])

	tools := payload["tools"].([]any)
	parameters := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	require.Equal(t, "object", parameters["type"])
	require.NotContains(t, parameters, "$ref")
	require.NotContains(t, parameters, "$defs")
	require.Contains(t, parameters["properties"].(map[string]any), "path")
}

func TestDecodeStreamHandlesContentReasoningToolsUsageAndNoise(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		"event: ping",
		"data: not-json",
		`data: {"id":"chunk-1","choices":[{"delta":{"reasoning_content":"think"}}]}`,
		`data: {"id":"chunk-1","choices":[{"delta":{"content":"answer","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"run","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`,
		"data: [DONE]",
	}, "\n\n")
	var chunks []apicompat.ChatCompletionsChunk
	require.NoError(t, DecodeStream(strings.NewReader(stream), func(chunk apicompat.ChatCompletionsChunk) error {
		chunks = append(chunks, chunk)
		return nil
	}))
	require.Len(t, chunks, 2)
	require.Equal(t, "think", *chunks[0].Choices[0].Delta.ReasoningContent)
	require.Equal(t, "answer", *chunks[1].Choices[0].Delta.Content)
	require.Equal(t, "call-1", chunks[1].Choices[0].Delta.ToolCalls[0].ID)
	require.Equal(t, 4, chunks[1].Usage.PromptTokens)
}

func TestDecodeStreamRejectsFinishOnlyAndPropagatesEmitterError(t *testing.T) {
	t.Parallel()

	err := DecodeStream(strings.NewReader("data: {\"id\":\"chunk\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"), func(apicompat.ChatCompletionsChunk) error { return nil })
	require.ErrorIs(t, err, ErrEmptyStream)

	want := errors.New("stop emission")
	err = DecodeStream(strings.NewReader("data: {\"id\":\"chunk\",\"choices\":[{\"delta\":{\"content\":\"visible\"}}]}\n\n"), func(apicompat.ChatCompletionsChunk) error { return want })
	require.ErrorIs(t, err, want)
}

func BenchmarkBuildPayload(b *testing.B) {
	request := &apicompat.ChatCompletionsRequest{
		Messages:        []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"Explain the current code and propose a safe refactor."`)}},
		Tools:           []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{Name: "read_file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}}},
		ReasoningEffort: "high",
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := BuildPayload(request, "cb/gemini-3.1-pro-thinking"); err != nil {
			b.Fatal(err)
		}
	}
}
