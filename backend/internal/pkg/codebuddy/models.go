package codebuddy

import "strings"

type Model struct {
	ID            string
	Upstream      string
	DisplayName   string
	Reasoning     bool
	Vision        bool
	ContextWindow int
}

var Models = []Model{
	{ID: "cb/", Upstream: "cb/", DisplayName: "CodeBuddy Default", Reasoning: true, Vision: true, ContextWindow: 200000},
	{ID: "cb/gpt-5.5", Upstream: "gpt-5.5", DisplayName: "GPT-5.5", Reasoning: true, Vision: true, ContextWindow: 200000},
	{ID: "cb/gpt-5.4", Upstream: "gpt-5.4", DisplayName: "GPT-5.4", Reasoning: true, Vision: true, ContextWindow: 200000},
	{ID: "cb/gpt-5.3-codex", Upstream: "gpt-5.3-codex", DisplayName: "GPT-5.3 Codex", Reasoning: true, Vision: true, ContextWindow: 200000},
	{ID: "cb/gpt-5.2-codex", Upstream: "gpt-5.2-codex", DisplayName: "GPT-5.2 Codex", Reasoning: true, Vision: true, ContextWindow: 200000},
	{ID: "cb/gpt-5.2", Upstream: "gpt-5.2", DisplayName: "GPT-5.2", Reasoning: true, Vision: true, ContextWindow: 200000},
	{ID: "cb/gpt-5.1", Upstream: "gpt-5.1", DisplayName: "GPT-5.1", Reasoning: true, Vision: true, ContextWindow: 200000},
	{ID: "cb/gpt-5.1-codex", Upstream: "gpt-5.1-codex", DisplayName: "GPT-5.1 Codex", Reasoning: true, Vision: true, ContextWindow: 200000},
	{ID: "cb/gpt-5.1-codex-max", Upstream: "gpt-5.1-codex-max", DisplayName: "GPT-5.1 Codex Max", Reasoning: true, Vision: true, ContextWindow: 200000},
	{ID: "cb/gpt-5.1-codex-mini", Upstream: "gpt-5.1-codex-mini", DisplayName: "GPT-5.1 Codex Mini", Reasoning: true, Vision: true, ContextWindow: 200000},
	{ID: "cb/gemini-3.1-pro", Upstream: "gemini-3.1-pro", DisplayName: "Gemini 3.1 Pro", Reasoning: true, Vision: true, ContextWindow: 1000000},
	{ID: "cb/gemini-3.0-flash", Upstream: "gemini-3.0-flash", DisplayName: "Gemini 3.0 Flash", Reasoning: true, Vision: true, ContextWindow: 1000000},
	{ID: "cb/gemini-3.1-flash-lite", Upstream: "gemini-3.1-flash-lite", DisplayName: "Gemini 3.1 Flash Lite", Reasoning: true, Vision: true, ContextWindow: 1000000},
	{ID: "cb/gemini-2.5-pro", Upstream: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", Reasoning: true, Vision: true, ContextWindow: 1000000},
	{ID: "cb/gemini-2.5-flash", Upstream: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash", Reasoning: true, Vision: true, ContextWindow: 1000000},
	{ID: "cb/kimi-k2.5", Upstream: "kimi-k2.5", DisplayName: "Kimi K2.5", Reasoning: true, Vision: true, ContextWindow: 262144},
	{ID: "cb/deepseek-v3-2-volc", Upstream: "deepseek-v3-2-volc", DisplayName: "DeepSeek V3.2", Reasoning: true, Vision: false, ContextWindow: 128000},
}

func ResolveModel(value string) Model {
	value = strings.TrimSpace(value)
	thinking := strings.HasSuffix(value, "-thinking")
	value = strings.TrimSuffix(value, "-thinking")
	value = strings.TrimPrefix(value, "cb/")
	for _, model := range Models {
		if model.Upstream == value || model.ID == "cb/"+value {
			if thinking {
				model.Reasoning = true
			}
			return model
		}
	}
	return Model{ID: "cb/" + value, Upstream: value, DisplayName: value, Reasoning: thinking, ContextWindow: 200000}
}

func ModelIDs() []string {
	ids := make([]string, 0, len(Models))
	for _, model := range Models {
		ids = append(ids, model.ID)
	}
	return ids
}
