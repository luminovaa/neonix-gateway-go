package codebuddy

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
)

var ErrEmptyStream = errors.New("CodeBuddy upstream returned no semantic output")

// DecodeStream parses CodeBuddy's [OI]-compatible SSE stream. Unknown
// keepalive events are ignored and a syntactically valid but empty stream is
// rejected so the gateway may fail over before committing output.
func DecodeStream(reader io.Reader, emit func(apicompat.ChatCompletionsChunk) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	seen := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk apicompat.ChatCompletionsChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.ID == "" && len(chunk.Choices) == 0 && chunk.Usage == nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			if nonEmptyString(choice.Delta.Content) || nonEmptyString(choice.Delta.ReasoningContent) || nonEmptyString(choice.Delta.Reasoning) || len(choice.Delta.ToolCalls) > 0 {
				seen = true
			}
		}
		if err := emit(chunk); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !seen {
		return ErrEmptyStream
	}
	return nil
}

func nonEmptyString(value *string) bool {
	return value != nil && *value != ""
}
