package codebuddychina

import "strings"

const (
	BaseURL = "https://www.codebuddy.cn"
	ChatURL = BaseURL + "/v2/chat/completions"
)

type Model struct {
	ID          string
	Upstream    string
	DisplayName string
	Family      string
}

var Models = []Model{
	{ID: "cbc/deepseek-v3", Upstream: "deepseek-v3", DisplayName: "DeepSeek V3 (CN)", Family: "deepseek"},
	{ID: "cbc/deepseek-r1", Upstream: "deepseek-r1", DisplayName: "DeepSeek R1 (CN)", Family: "deepseek"},
	{ID: "cbc/deepseek-v3-2-volc", Upstream: "deepseek-v3-2-volc", DisplayName: "DeepSeek V3.2 Volc (CN)", Family: "deepseek"},
	{ID: "cbc/deepseek-v4-flash", Upstream: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash (CN)", Family: "deepseek"},
	{ID: "cbc/deepseek-v4-pro", Upstream: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro (CN)", Family: "deepseek"},
	{ID: "cbc/kimi-k2.5", Upstream: "kimi-k2.5", DisplayName: "Kimi K2.5 (CN)", Family: "kimi"},
	{ID: "cbc/kimi-k2.6", Upstream: "kimi-k2.6", DisplayName: "Kimi K2.6 (CN)", Family: "kimi"},
	{ID: "cbc/kimi-k2.7", Upstream: "kimi-k2.7", DisplayName: "Kimi K2.7 (CN)", Family: "kimi"},
	{ID: "cbc/glm-5.1", Upstream: "glm-5.1", DisplayName: "GLM 5.1 (CN)", Family: "glm"},
	{ID: "cbc/glm-5.2", Upstream: "glm-5.2", DisplayName: "GLM 5.2 (CN)", Family: "glm"},
	{ID: "cbc/glm-5v-turbo", Upstream: "glm-5v-turbo", DisplayName: "GLM 5V Turbo (CN)", Family: "glm"},
	{ID: "cbc/minimax-m3", Upstream: "minimax-m3", DisplayName: "MiniMax M3 (CN)", Family: "minimax"},
	{ID: "cbc/hy3-preview", Upstream: "hy3-preview", DisplayName: "Hunyuan 3 Preview (CN)", Family: "hunyuan"},
}

func ResolveModel(value string) Model {
	value = strings.TrimSpace(value)
	upstream := strings.TrimPrefix(value, "cbc/")
	for _, model := range Models {
		if strings.EqualFold(model.ID, value) || strings.EqualFold(model.Upstream, upstream) {
			return model
		}
	}
	return Model{ID: "cbc/" + upstream, Upstream: upstream, DisplayName: upstream}
}

func ModelIDs() []string {
	ids := make([]string, 0, len(Models))
	for _, model := range Models {
		ids = append(ids, model.ID)
	}
	return ids
}
