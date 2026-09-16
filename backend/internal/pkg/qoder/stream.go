package qoder

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
)

type Usage struct{ InputTokens, OutputTokens int }
type Event struct {
	Delta        apicompat.ChatDelta
	FinishReason string
	Usage        Usage
}
type StreamError struct {
	Status  int
	Message string
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("Qoder upstream rejected the stream with HTTP %d", e.Status)
}

type thinkState struct {
	active  bool
	pending string
}

func splitThink(text string, state *thinkState) (content, reasoning string) {
	remaining := state.pending + text
	state.pending = ""
	for remaining != "" {
		tag := "<think>"
		if state.active {
			tag = "</think>"
		}
		lower := strings.ToLower(remaining)
		i := strings.Index(lower, tag)
		if i < 0 {
			suffix := ""
			for n := min(7, len(remaining)); n > 0; n-- {
				if strings.HasPrefix(tag, strings.ToLower(remaining[len(remaining)-n:])) {
					suffix = remaining[len(remaining)-n:]
					break
				}
			}
			safe := strings.TrimSuffix(remaining, suffix)
			if state.active {
				reasoning += safe
			} else {
				content += safe
			}
			state.pending = suffix
			break
		}
		before := remaining[:i]
		if state.active {
			reasoning += before
		} else {
			content += before
		}
		remaining = remaining[i+len(tag):]
		state.active = !state.active
	}
	content = strings.ReplaceAll(strings.ReplaceAll(content, "</think>", ""), "</THINK>", "")
	return
}

func DecodeStream(reader io.Reader, emit func(Event) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	state := thinkState{}
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var wrapper struct {
			Body            string `json:"body"`
			StatusCodeValue int    `json:"statusCodeValue"`
			Message         string `json:"message"`
		}
		if json.Unmarshal([]byte(data), &wrapper) != nil {
			continue
		}
		if wrapper.StatusCodeValue >= 400 {
			return &StreamError{Status: wrapper.StatusCodeValue, Message: wrapper.Message}
		}
		if wrapper.Body == "" || wrapper.Body == "[DONE]" {
			continue
		}
		var inner struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Role      string                   `json:"role"`
					Content   string                   `json:"content"`
					Reasoning string                   `json:"reasoning_content"`
					ToolCalls []apicompat.ChatToolCall `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(wrapper.Body), &inner) != nil {
			continue
		}
		event := Event{Usage: Usage{inner.Usage.PromptTokens, inner.Usage.CompletionTokens}}
		if len(inner.Choices) > 0 {
			choice := inner.Choices[0]
			event.FinishReason = choice.FinishReason
			event.Delta.Role = choice.Delta.Role
			content, reasoning := splitThink(choice.Delta.Content, &state)
			if content != "" {
				event.Delta.Content = &content
			}
			combined := strings.ReplaceAll(strings.ReplaceAll(choice.Delta.Reasoning, "<think>", ""), "</think>", "") + reasoning
			if combined != "" {
				event.Delta.ReasoningContent = &combined
			}
			event.Delta.ToolCalls = choice.Delta.ToolCalls
		}
		if event.Delta.Role != "" || event.Delta.Content != nil || event.Delta.ReasoningContent != nil || len(event.Delta.ToolCalls) > 0 || event.FinishReason != "" || event.Usage.InputTokens > 0 || event.Usage.OutputTokens > 0 {
			if err := emit(event); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
