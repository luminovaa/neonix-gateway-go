package kiro

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
)

const maxToolDescriptionLength = 10237

func BuildChatPayload(req *apicompat.ChatCompletionsRequest, modelID, profileARN, origin string) (*Payload, error) {
	if req == nil {
		return nil, errors.New("kiro request is required")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, errors.New("kiro model is required")
	}
	if origin == "" {
		origin = "AI_EDITOR"
	}

	var systemParts []string
	messages := make([]HistoryMessage, 0, len(req.Messages)+1)
	systemMerged := false
	for _, message := range req.Messages {
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system", "developer":
			text, _, _, err := decodeContent(message.Content)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(text) != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			text, images, documents, err := decodeContent(message.Content)
			if err != nil {
				return nil, err
			}
			if !systemMerged && len(systemParts) > 0 {
				text = strings.Join(systemParts, "\n\n") + "\n\n" + text
				systemMerged = true
			}
			appendConversationMessage(&messages, HistoryMessage{UserInputMessage: &UserInputMessage{Content: fallbackText(text, "Continue"), ModelID: modelID, Origin: origin, Images: images, Documents: documents}})
		case "assistant":
			text, _, _, err := decodeContent(message.Content)
			if err != nil {
				return nil, err
			}
			toolUses := make([]ToolUse, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				input := map[string]any{}
				if strings.TrimSpace(call.Function.Arguments) != "" {
					_ = json.Unmarshal([]byte(call.Function.Arguments), &input)
				}
				toolUses = append(toolUses, ToolUse{ToolUseID: call.ID, Name: call.Function.Name, Input: input})
			}
			assistant := &AssistantResponseMessage{Content: fallbackText(text, " "), ToolUses: toolUses}
			if reasoning := strings.TrimSpace(message.ReasoningContent); reasoning != "" {
				assistant.ReasoningContent = &ReasoningContent{ReasoningText: ReasoningText{Text: reasoning}}
			}
			appendConversationMessage(&messages, HistoryMessage{AssistantResponseMessage: assistant})
		case "tool", "function":
			text, _, _, err := decodeContent(message.Content)
			if err != nil {
				return nil, err
			}
			result := ToolResult{ToolUseID: message.ToolCallID, Status: "success", Content: []ToolResultContent{{Text: text}}}
			appendConversationMessage(&messages, HistoryMessage{UserInputMessage: &UserInputMessage{Content: "Tool results provided.", ModelID: modelID, Origin: origin, UserInputMessageContext: &UserInputMessageContext{ToolResults: []ToolResult{result}}}})
		}
	}
	if len(messages) == 0 {
		content := "Continue"
		if len(systemParts) > 0 {
			content = strings.Join(systemParts, "\n\n") + "\n\nContinue"
		}
		messages = append(messages, HistoryMessage{UserInputMessage: &UserInputMessage{Content: content, ModelID: modelID, Origin: origin}})
	}
	if messages[0].AssistantResponseMessage != nil {
		messages = append([]HistoryMessage{{UserInputMessage: &UserInputMessage{Content: "Continue", ModelID: modelID, Origin: origin}}}, messages...)
	}
	if messages[len(messages)-1].UserInputMessage == nil {
		messages = append(messages, HistoryMessage{UserInputMessage: &UserInputMessage{Content: "Continue", ModelID: modelID, Origin: origin}})
	}
	current := messages[len(messages)-1]
	history := messages[:len(messages)-1]
	tools := convertTools(req.Tools)
	if len(tools) > 0 {
		if current.UserInputMessage.UserInputMessageContext == nil {
			current.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{}
		}
		current.UserInputMessage.UserInputMessageContext.Tools = tools
	}
	maxTokens := 0
	if req.MaxCompletionTokens != nil {
		maxTokens = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	var inference *InferenceConfig
	if maxTokens > 0 || req.Temperature != nil || req.TopP != nil {
		inference = &InferenceConfig{MaxTokens: maxTokens, Temperature: req.Temperature, TopP: req.TopP}
	}
	return &Payload{
		ConversationState: ConversationState{AgentContinuationID: newUUID(), AgentTaskType: "vibe", ChatTriggerType: "MANUAL", ConversationID: newUUID(), CurrentMessage: current, History: history},
		ProfileARN:        profileARN, InferenceConfig: inference,
	}, nil
}

func appendConversationMessage(messages *[]HistoryMessage, next HistoryMessage) {
	if len(*messages) == 0 {
		*messages = append(*messages, next)
		return
	}
	last := &(*messages)[len(*messages)-1]
	if last.UserInputMessage != nil && next.UserInputMessage != nil {
		last.UserInputMessage.Content = strings.TrimSpace(last.UserInputMessage.Content + "\n\n" + next.UserInputMessage.Content)
		last.UserInputMessage.Images = append(last.UserInputMessage.Images, next.UserInputMessage.Images...)
		last.UserInputMessage.Documents = append(last.UserInputMessage.Documents, next.UserInputMessage.Documents...)
		if next.UserInputMessage.UserInputMessageContext != nil {
			if last.UserInputMessage.UserInputMessageContext == nil {
				last.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{}
			}
			last.UserInputMessage.UserInputMessageContext.ToolResults = append(last.UserInputMessage.UserInputMessageContext.ToolResults, next.UserInputMessage.UserInputMessageContext.ToolResults...)
		}
		return
	}
	if last.AssistantResponseMessage != nil && next.AssistantResponseMessage != nil {
		last.AssistantResponseMessage.Content = strings.TrimSpace(last.AssistantResponseMessage.Content + "\n\n" + next.AssistantResponseMessage.Content)
		last.AssistantResponseMessage.ToolUses = append(last.AssistantResponseMessage.ToolUses, next.AssistantResponseMessage.ToolUses...)
		return
	}
	*messages = append(*messages, next)
}

func convertTools(input []apicompat.ChatTool) []Tool {
	out := make([]Tool, 0, len(input))
	for _, item := range input {
		if item.Function == nil || strings.TrimSpace(item.Function.Name) == "" {
			continue
		}
		description := item.Function.Description
		if description == "" {
			description = "Tool: " + item.Function.Name
		}
		if len(description) > maxToolDescriptionLength {
			description = description[:maxToolDescriptionLength] + "..."
		}
		schema := item.Function.Parameters
		if len(schema) == 0 || !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, Tool{ToolSpecification: ToolSpecification{Name: item.Function.Name, Description: description, InputSchema: ToolInputSchema{JSON: schema}}})
	}
	return out
}

func decodeContent(raw json.RawMessage) (string, []Image, []Document, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil, nil
	}
	var parts []apicompat.ChatContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, nil, errors.New("unsupported Kiro message content")
	}
	var texts []string
	var images []Image
	var documents []Document
	for _, part := range parts {
		switch part.Type {
		case "text":
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		case "image_url":
			if part.ImageURL != nil {
				if image, ok := parseImageDataURI(part.ImageURL.URL); ok {
					images = append(images, image)
				}
			}
		case "file", "document":
			if part.File != nil && part.File.FileData != "" {
				documents = append(documents, parseDocument(part.File.Filename, part.File.FileData))
			}
		}
	}
	return strings.Join(texts, ""), images, documents, nil
}

func parseImageDataURI(value string) (Image, bool) {
	const marker = ";base64,"
	if !strings.HasPrefix(value, "data:image/") {
		return Image{}, false
	}
	i := strings.Index(value, marker)
	if i < 0 {
		return Image{}, false
	}
	format := strings.TrimPrefix(value[:i], "data:image/")
	if format == "jpg" {
		format = "jpeg"
	}
	switch format {
	case "jpeg", "png", "gif", "webp":
	default:
		return Image{}, false
	}
	return Image{Format: format, Source: ByteSource{Bytes: value[i+len(marker):]}}, true
}

func parseDocument(name, value string) Document {
	if name == "" {
		name = "document.txt"
	}
	format := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	switch format {
	case "pdf", "md", "csv", "html", "txt":
	default:
		format = "txt"
	}
	if i := strings.Index(value, ";base64,"); strings.HasPrefix(value, "data:") && i >= 0 {
		value = value[i+8:]
	}
	return Document{Format: format, Name: name, Source: ByteSource{Bytes: value}}
}

func fallbackText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("kiro UUID entropy: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	buf := make([]byte, 36)
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf)
}
