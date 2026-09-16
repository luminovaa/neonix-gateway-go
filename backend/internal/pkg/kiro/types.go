package kiro

import "encoding/json"

const (
	BuilderIDProfileARN = "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"
	SocialProfileARN    = "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK"
)

type Payload struct {
	ConversationState            ConversationState `json:"conversationState"`
	ProfileARN                   string            `json:"profileArn,omitempty"`
	InferenceConfig              *InferenceConfig  `json:"inferenceConfig,omitempty"`
	AdditionalModelRequestFields map[string]any    `json:"additionalModelRequestFields,omitempty"`
}

type ConversationState struct {
	AgentContinuationID string           `json:"agentContinuationId,omitempty"`
	AgentTaskType       string           `json:"agentTaskType,omitempty"`
	ChatTriggerType     string           `json:"chatTriggerType"`
	ConversationID      string           `json:"conversationId"`
	CurrentMessage      HistoryMessage   `json:"currentMessage"`
	History             []HistoryMessage `json:"history,omitempty"`
}

type HistoryMessage struct {
	UserInputMessage         *UserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *AssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type UserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []Image                  `json:"images,omitempty"`
	Documents               []Document               `json:"documents,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type AssistantResponseMessage struct {
	Content          string            `json:"content"`
	ReasoningContent *ReasoningContent `json:"reasoningContent,omitempty"`
	ToolUses         []ToolUse         `json:"toolUses,omitempty"`
}

type ReasoningContent struct {
	ReasoningText ReasoningText `json:"reasoningText"`
}

type ReasoningText struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []Tool       `json:"tools,omitempty"`
	ToolResults []ToolResult `json:"toolResults,omitempty"`
}

type Tool struct {
	ToolSpecification ToolSpecification `json:"toolSpecification"`
}

type ToolSpecification struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema ToolInputSchema `json:"inputSchema"`
}

type ToolInputSchema struct {
	JSON json.RawMessage `json:"json"`
}

type ToolUse struct {
	ToolUseID string         `json:"toolUseId"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
}

type ToolResult struct {
	Content   []ToolResultContent `json:"content"`
	Status    string              `json:"status"`
	ToolUseID string              `json:"toolUseId"`
}

type ToolResultContent struct {
	Text string `json:"text"`
}

type Image struct {
	Format string     `json:"format"`
	Source ByteSource `json:"source"`
}

type Document struct {
	Format string     `json:"format"`
	Name   string     `json:"name"`
	Source ByteSource `json:"source"`
}

type ByteSource struct {
	Bytes string `json:"bytes"`
}

type InferenceConfig struct {
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
}

type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
}

type Event struct {
	Type      string
	Content   string
	Reasoning string
	Signature string
	ToolUse   *ToolUse
	Usage     *Usage
}
