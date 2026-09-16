package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
)

// providerChatOutputProtocol identifies the public protocol a native provider
// adapter must emit after consuming its upstream chat stream. Keeping this
// bridge provider-neutral prevents adapters from depending on one another.
type providerChatOutputProtocol uint8

const (
	providerChatOutputChat providerChatOutputProtocol = iota
	providerChatOutputAnthropic
	providerChatOutputResponses
)

type providerChatResponsesBridge struct {
	customTools    map[string]bool
	functionTools  map[string]bool
	toolSearch     bool
	namespaceTools map[string]apicompat.NamespacedToolName
}

type providerChatUsage struct {
	InputTokens  int
	OutputTokens int
}

type providerChatEvent struct {
	Delta        apicompat.ChatDelta
	FinishReason string
	Usage        providerChatUsage
}

func normalizeProviderToolCalls(calls []apicompat.ChatToolCall, collected *[]apicompat.ChatToolCall) []apicompat.ChatToolCall {
	normalized := make([]apicompat.ChatToolCall, 0, len(calls))
	for i := range calls {
		call := &calls[i]
		idx := i
		if call.Index != nil {
			idx = *call.Index
		}
		for len(*collected) <= idx {
			n := len(*collected)
			(*collected) = append(*collected, apicompat.ChatToolCall{Index: &n, Type: "function"})
		}
		target := &(*collected)[idx]
		if call.ID != "" {
			target.ID = call.ID
		}
		if target.ID == "" {
			target.ID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		if call.Type != "" {
			target.Type = call.Type
		}
		if call.Function.Name != "" {
			target.Function.Name = call.Function.Name
		}
		target.Function.Arguments += call.Function.Arguments
		chunk := *call
		chunk.Index = &idx
		chunk.ID = target.ID
		if chunk.Type == "" {
			chunk.Type = "function"
		}
		normalized = append(normalized, chunk)
	}
	return normalized
}

func writeProviderChatEvent(c *gin.Context, id string, created int64, model string, event providerChatEvent, output providerChatOutputProtocol, anthropicState *apicompat.ChatCompletionsToAnthropicStreamState, responsesState *apicompat.ChatCompletionsToResponsesStreamState) error {
	finish := event.FinishReason
	if len(event.Delta.ToolCalls) > 0 && finish != "" {
		finish = "tool_calls"
	}
	var finishPtr *string
	if finish != "" {
		finishPtr = &finish
	}
	chunk := apicompat.ChatCompletionsChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: event.Delta, FinishReason: finishPtr}}}
	if event.Usage.InputTokens > 0 || event.Usage.OutputTokens > 0 {
		chunk.Usage = &apicompat.ChatUsage{PromptTokens: event.Usage.InputTokens, CompletionTokens: event.Usage.OutputTokens, TotalTokens: event.Usage.InputTokens + event.Usage.OutputTokens}
	}
	switch output {
	case providerChatOutputAnthropic:
		for _, converted := range apicompat.ChatCompletionsChunkToAnthropicEvents(&chunk, anthropicState) {
			if err := writeProviderChatAnthropicWire(c, converted); err != nil {
				return err
			}
		}
		return nil
	case providerChatOutputResponses:
		for _, converted := range apicompat.ChatCompletionsChunkToResponsesEvents(&chunk, responsesState) {
			if err := writeProviderChatResponsesWire(c, converted); err != nil {
				return err
			}
		}
		return nil
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.Writer, "data: %s\n\n", raw)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
	return err
}

func writeProviderChatAnthropicWire(c *gin.Context, event apicompat.AnthropicStreamEvent) error {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event.Type, raw)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
	return err
}

func writeProviderChatResponsesWire(c *gin.Context, event apicompat.ResponsesStreamEvent) error {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	wire, err := apicompat.ResponsesEventToSSE(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(c.Writer, wire)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
	return err
}

func providerJSONString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
