package qoder

import "strings"

type Model struct {
	ID, Upstream, DisplayName       string
	MaxInputTokens, MaxOutputTokens int
	Vision, Reasoning               bool
	PriceFactor                     float64
}

var Models = []Model{
	{"qr/Auto", "auto", "Auto", 180000, 64000, true, false, 1},
	{"qr/Ultimate", "ultimate", "Ultimate", 180000, 64000, true, true, 1.6},
	{"qr/Performance", "performance", "Performance", 272000, 64000, true, false, 1.1},
	{"qr/Efficient", "efficient", "Efficient", 180000, 64000, true, false, .3},
	{"qr/Qwen3.7-Max", "qmodel_latest", "Qwen3.7-Max", 180000, 64000, true, false, .5},
	{"qr/Qwen3.8-Preview", "qmodel_preview", "Qwen 3.8 Preview", 1000000, 128000, true, true, .5},
	{"qr/Qwen3.7-Plus", "qmodel", "Qwen3.7-Plus", 180000, 64000, true, false, .1},
	{"qr/DeepSeek-V4-Pro", "dmodel", "DeepSeek-V4-Pro", 180000, 64000, true, true, .5},
	{"qr/DeepSeek-V4-Flash", "dfmodel", "DeepSeek-V4-Flash", 180000, 64000, true, true, .1},
	{"qr/GLM-5.2", "gm52model", "GLM-5.2", 180000, 64000, true, true, .6},
	{"qr/Kimi-K2.7-Code", "kmodel", "Kimi-K2.7-Code", 256000, 64000, true, false, .3},
	{"qr/Kimi-K3", "kmodel_latest", "Kimi K3", 1000000, 128000, true, true, .3},
	{"qr/MiniMax-M3", "mmodel", "MiniMax-M3", 180000, 64000, true, false, .2},
	{"qr/Lite", "lite", "Lite", 180000, 64000, false, false, 0},
}

func Lookup(value string) Model {
	value = strings.TrimSpace(value)
	for _, model := range Models {
		if strings.EqualFold(model.ID, value) || strings.EqualFold(model.Upstream, value) {
			return model
		}
	}
	return Models[0]
}

func Resolve(value string) Model {
	value = strings.TrimSpace(value)
	for _, model := range Models {
		if strings.EqualFold(model.ID, value) || strings.EqualFold(model.Upstream, value) {
			return model
		}
	}
	upstream := strings.TrimSpace(strings.TrimPrefix(value, "qr/"))
	if upstream == "" {
		return Models[0]
	}
	return Model{ID: "qr/" + upstream, Upstream: upstream, DisplayName: upstream, MaxInputTokens: 180000, MaxOutputTokens: 64000}
}

func ActualModel(value string) string { return Resolve(value).Upstream }
