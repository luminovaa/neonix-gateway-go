package codebuddy

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
)

func BuildPayload(request *apicompat.ChatCompletionsRequest, actualModel string) (map[string]any, error) {
	if request == nil {
		return nil, errors.New("request is required")
	}
	model := ResolveModel(actualModel)
	messages := make([]any, 0, len(request.Messages)+1)
	hasSystem := false
	for _, message := range request.Messages {
		if message.Role == "system" {
			hasSystem = true
		}
		raw, err := json.Marshal(message)
		if err != nil {
			return nil, err
		}
		var normalized map[string]any
		if err := json.Unmarshal(raw, &normalized); err != nil {
			return nil, err
		}
		messages = append(messages, normalized)
	}
	if !hasSystem {
		messages = append([]any{map[string]any{"role": "system", "content": "You are a helpful AI assistant."}}, messages...)
	}
	payload := map[string]any{"model": model.Upstream, "messages": messages, "stream": true}
	if request.MaxTokens != nil && *request.MaxTokens > 0 {
		payload["max_tokens"] = min(*request.MaxTokens, 32000)
	} else if request.MaxCompletionTokens != nil && *request.MaxCompletionTokens > 0 {
		payload["max_tokens"] = min(*request.MaxCompletionTokens, 32000)
	}
	if request.Temperature != nil {
		payload["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		payload["top_p"] = *request.TopP
	}
	if len(request.Tools) > 0 {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			raw, err := json.Marshal(tool)
			if err != nil {
				return nil, err
			}
			var value map[string]any
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, err
			}
			if function, ok := value["function"].(map[string]any); ok {
				function["parameters"] = sanitizeSchema(function["parameters"], nil, map[string]bool{})
			}
			tools = append(tools, value)
		}
		payload["tools"] = tools
	}
	if len(request.ToolChoice) > 0 && string(request.ToolChoice) != "null" {
		var choice any
		if json.Unmarshal(request.ToolChoice, &choice) == nil {
			payload["tool_choice"] = choice
		}
	}
	if strings.HasSuffix(strings.TrimSpace(actualModel), "-thinking") || strings.EqualFold(request.ReasoningEffort, "high") {
		payload["reasoning"] = map[string]any{"effort": "high"}
	}
	return payload, nil
}

func sanitizeSchema(value any, inheritedDefs map[string]any, seen map[string]bool) any {
	object, ok := value.(map[string]any)
	if !ok {
		if list, listOK := value.([]any); listOK {
			out := make([]any, len(list))
			for i := range list {
				out[i] = sanitizeSchema(list[i], inheritedDefs, seen)
			}
			return out
		}
		return value
	}
	defs := inheritedDefs
	if local, ok := object["$defs"].(map[string]any); ok {
		defs = local
	} else if local, ok := object["definitions"].(map[string]any); ok {
		defs = local
	}
	if ref, ok := object["$ref"].(string); ok && defs != nil {
		name := strings.TrimPrefix(strings.TrimPrefix(ref, "#/$defs/"), "#/definitions/")
		if target, exists := defs[name]; exists && !seen[name] {
			nextSeen := make(map[string]bool, len(seen)+1)
			for key, present := range seen {
				nextSeen[key] = present
			}
			nextSeen[name] = true
			return sanitizeSchema(target, defs, nextSeen)
		}
		return map[string]any{"type": "object"}
	}
	out := make(map[string]any, len(object))
	for key, item := range object {
		switch key {
		case "$schema", "$id", "$comment", "$defs", "definitions":
			continue
		}
		out[key] = sanitizeSchema(item, defs, seen)
	}
	if _, exists := out["type"]; !exists {
		out["type"] = "object"
	}
	if out["type"] == "object" {
		if _, exists := out["properties"]; !exists {
			out["properties"] = map[string]any{}
		}
	}
	return out
}
