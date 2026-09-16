package codebuddychina

import (
	"encoding/json"
	"errors"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
)

// BuildPayload preserves the OpenAI-compatible request contract spoken by
// codebuddy.cn while forcing streaming, which is the only reliable upstream
// response mode. Protocol conversion remains at the gateway boundary.
func BuildPayload(request *apicompat.ChatCompletionsRequest, actualModel string) (map[string]any, error) {
	if request == nil {
		return nil, errors.New("request is required")
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	payload["model"] = ResolveModel(actualModel).Upstream
	payload["stream"] = true
	return payload, nil
}
