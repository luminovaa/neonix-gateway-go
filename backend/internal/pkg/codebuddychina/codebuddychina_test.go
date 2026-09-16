package codebuddychina

import (
	"encoding/json"
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestResolveModelSupportsCatalogRawAndUnknownIDs(t *testing.T) {
	require.Equal(t, "glm-5.2", ResolveModel("cbc/glm-5.2").Upstream)
	require.Equal(t, "deepseek-v4-pro", ResolveModel("deepseek-v4-pro").Upstream)
	require.Equal(t, "future-model", ResolveModel("cbc/future-model").Upstream)
	require.Contains(t, ModelIDs(), "cbc/deepseek-v3")
}

func TestBuildPayloadPreservesOpenAIFieldsAndForcesStream(t *testing.T) {
	temperature := 0.3
	request := &apicompat.ChatCompletionsRequest{
		Model: "cbc/glm-5.2", Stream: false, Temperature: &temperature,
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)}},
		Tools:    []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	payload, err := BuildPayload(request, request.Model)
	require.NoError(t, err)
	require.Equal(t, "glm-5.2", payload["model"])
	require.Equal(t, true, payload["stream"])
	require.Equal(t, 0.3, payload["temperature"])
	require.Len(t, payload["messages"], 1)
	require.Len(t, payload["tools"], 1)
}

func BenchmarkBuildPayload(b *testing.B) {
	temperature := 0.3
	request := &apicompat.ChatCompletionsRequest{
		Model: "cbc/glm-5.2", Temperature: &temperature,
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"benchmark prompt"}]`)}},
		Tools:    []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := BuildPayload(request, request.Model); err != nil {
			b.Fatal(err)
		}
	}
}
