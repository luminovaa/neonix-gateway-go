package qoder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
)

func BuildPayload(request *apicompat.ChatCompletionsRequest, actualModel, userType string) (map[string]any, error) {
	if request == nil {
		return nil, fmt.Errorf("request is required")
	}
	model := Resolve(actualModel)
	messages, err := buildMessages(request)
	if err != nil {
		return nil, err
	}
	prompt, images := latestUserInput(request.Messages)
	maxTokens := 32768
	if request.MaxTokens != nil {
		maxTokens = *request.MaxTokens
	} else if request.MaxCompletionTokens != nil {
		maxTokens = *request.MaxCompletionTokens
	}
	requestID := uuid.NewString()
	modelConfig := map[string]any{"key": model.Upstream, "display_name": model.DisplayName, "model": "", "format": "openai", "is_vl": model.Vision, "is_reasoning": model.Reasoning, "api_key": "", "url": "", "source": "system", "max_input_tokens": model.MaxInputTokens}
	context := map[string]any{"text": map[string]any{"type": "text", "text": prompt}, "extra": map[string]any{"context": []any{}, "modelConfig": map[string]any{"key": model.Upstream, "is_reasoning": model.Reasoning}, "originalContent": map[string]any{"type": "text", "text": prompt}}, "features": []any{}}
	if len(images) > 0 {
		context["images"] = images
		context["imageUrls"] = imageURLs(images)
	} else {
		context["imageUrls"] = nil
	}
	if userType == "" {
		userType = "personal_standard"
	}
	payload := map[string]any{
		"request_id": requestID, "request_set_id": uuid.NewString(), "chat_record_id": requestID, "stream": true,
		"chat_task": "FREE_INPUT", "chat_context": context, "image_urls": imageURLs(images), "is_reply": true, "is_retry": false,
		"session_id": sessionID(request.Messages), "code_language": "", "source": 1, "version": "3", "chat_prompt": "",
		"parameters": map[string]any{"max_tokens": maxTokens}, "aliyun_user_type": userType, "session_type": "qodercli",
		"agent_id": "agent_common", "task_id": "common", "model_config": modelConfig, "messages": messages,
		"business": map[string]any{"id": uuid.NewString(), "begin_at": time.Now().UnixMilli(), "name": truncateRunes(prompt, 30)},
	}
	if len(request.Tools) > 0 {
		payload["tools"] = request.Tools
	}
	return payload, nil
}

func buildMessages(request *apicompat.ChatCompletionsRequest) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(request.Messages)+1)
	if len(request.Tools) > 0 {
		guidance := toolGuidance(request.Tools)
		result = append(result, map[string]any{"role": "system", "content": guidance, "contents": []map[string]any{{"type": "text", "text": guidance}}})
	}
	for _, message := range request.Messages {
		entry := map[string]any{"role": message.Role}
		var stringContent string
		if len(message.Content) > 0 && string(message.Content) != "null" {
			if err := json.Unmarshal(message.Content, &stringContent); err != nil {
				var parts []map[string]any
				if err := json.Unmarshal(message.Content, &parts); err != nil {
					return nil, fmt.Errorf("invalid message content: %w", err)
				}
				texts := make([]string, 0)
				contents := make([]map[string]any, 0)
				for _, part := range parts {
					switch part["type"] {
					case "text":
						if text, ok := part["text"].(string); ok {
							texts = append(texts, text)
							contents = append(contents, map[string]any{"type": "text", "text": text})
						}
					case "image_url":
						contents = append(contents, part)
					case "tool_result":
						if id, ok := part["tool_use_id"].(string); ok {
							result = append(result, map[string]any{"role": "tool", "tool_call_id": id, "content": fmt.Sprint(part["content"])})
						}
					}
				}
				stringContent = strings.Join(texts, "\n")
				entry["contents"] = contents
			}
		}
		entry["content"] = stringContent
		if _, ok := entry["contents"]; !ok {
			entry["contents"] = []map[string]any{{"type": "text", "text": stringContent}}
		}
		if len(message.ToolCalls) > 0 {
			entry["tool_calls"] = message.ToolCalls
		}
		if message.ToolCallID != "" {
			entry["tool_call_id"] = message.ToolCallID
		}
		result = append(result, entry)
	}
	return result, nil
}

func toolGuidance(tools []apicompat.ChatTool) string {
	lines := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Function == nil || strings.TrimSpace(tool.Function.Name) == "" {
			continue
		}
		line := "- " + tool.Function.Name
		if description := strings.TrimSpace(tool.Function.Description); description != "" {
			line += ": " + description
		}
		lines = append(lines, line)
	}
	return "Use the available tools when the request requires them. Return a tool call with valid arguments instead of describing the action. Trust tool results in later messages.\n\nAvailable tools:\n" + strings.Join(lines, "\n")
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func latestUserInput(messages []apicompat.ChatMessage) (string, []map[string]any) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(messages[i].Content, &text) == nil {
			return text, nil
		}
		var parts []map[string]any
		_ = json.Unmarshal(messages[i].Content, &parts)
		var texts []string
		var images []map[string]any
		for _, p := range parts {
			if p["type"] == "text" {
				texts = append(texts, fmt.Sprint(p["text"]))
			}
			if p["type"] == "image_url" {
				images = append(images, p)
			}
		}
		return strings.Join(texts, "\n"), images
	}
	return "", nil
}
func imageURLs(images []map[string]any) []string {
	out := make([]string, 0, len(images))
	for _, image := range images {
		if nested, ok := image["image_url"].(map[string]any); ok {
			if value, ok := nested["url"].(string); ok {
				out = append(out, value)
			}
		}
	}
	return out
}
func sessionID(messages []apicompat.ChatMessage) string {
	h := sha256.New()
	found := false
	for _, m := range messages {
		if m.Role == "system" {
			h.Write([]byte("system:"))
			h.Write(m.Content)
		}
		if m.Role == "user" && !found {
			h.Write([]byte("user:"))
			h.Write(m.Content)
			found = true
			break
		}
	}
	if !found {
		h.Write([]byte("__no_user__"))
	}
	x := hex.EncodeToString(h.Sum(nil))[:32]
	return x[:8] + "-" + x[8:12] + "-4" + x[13:16] + "-a" + x[17:20] + "-" + x[20:32]
}
