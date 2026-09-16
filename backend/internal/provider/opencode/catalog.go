package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	ModelsURL       = "https://opencode.ai/zen/v1/models"
	maxResponseSize = 2 << 20
)

var ErrCatalogueUnavailable = errors.New("OpenCode Zen model catalogue is unavailable")

type FreeModel struct {
	ID          string
	Name        string
	Description string
	OwnedBy     string
	SortOrder   int
	Remote      map[string]any
}

var fallbackModels = []FreeModel{
	{ID: "big-pickle", Name: "Big Pickle", Description: "OpenCode Zen free model.", OwnedBy: "opencode", SortOrder: 0},
	{ID: "deepseek-v4-flash-free", Name: "DeepSeek V4 Flash Free", Description: "OpenCode Zen free model.", OwnedBy: "opencode", SortOrder: 1},
	{ID: "mimo-v2.5-free", Name: "MiMo V2.5 Free", Description: "OpenCode Zen free model.", OwnedBy: "opencode", SortOrder: 2},
	{ID: "nemotron-3-super-free", Name: "Nemotron 3 Super Free", Description: "OpenCode Zen free model.", OwnedBy: "opencode", SortOrder: 3},
}

func FallbackModels() []FreeModel {
	out := make([]FreeModel, len(fallbackModels))
	copy(out, fallbackModels)
	return out
}

func FetchFreeModels(ctx context.Context, client *http.Client) ([]FreeModel, error) {
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrCatalogueUnavailable)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: request failed", ErrCatalogueUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: HTTP %d", ErrCatalogueUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil || len(body) > maxResponseSize {
		return nil, fmt.Errorf("%w: invalid response body", ErrCatalogueUnavailable)
	}
	models, err := ParseFreeModels(body)
	if err != nil || len(models) == 0 {
		return nil, fmt.Errorf("%w: no valid free models", ErrCatalogueUnavailable)
	}
	return models, nil
}

func ParseFreeModels(data []byte) ([]FreeModel, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Data == nil {
		return nil, errors.New("missing data array")
	}
	out := make([]FreeModel, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for index, raw := range payload.Data {
		id := stringValue(raw["id"])
		if id == "" || !IsFreeModel(raw, id) {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		fallback, known := fallbackByID(id)
		name := stringValue(raw["name"])
		if name == "" && known {
			name = fallback.Name
		}
		if name == "" {
			name = id
		}
		description := stringValue(raw["description"])
		if description == "" && known {
			description = fallback.Description
		}
		if description == "" {
			description = "OpenCode Zen free model."
		}
		ownedBy := stringValue(raw["owned_by"])
		if ownedBy == "" {
			ownedBy = stringValue(raw["ownedBy"])
		}
		if ownedBy == "" {
			ownedBy = "opencode"
		}
		sortOrder := index + len(fallbackModels)
		if known {
			sortOrder = fallback.SortOrder
		}
		out = append(out, FreeModel{ID: id, Name: name, Description: description, OwnedBy: ownedBy, SortOrder: sortOrder, Remote: raw})
	}
	return out, nil
}

func IsFreeModel(model map[string]any, id string) bool {
	if _, known := fallbackByID(id); known {
		return true
	}
	if strings.HasSuffix(strings.ToLower(id), "-free") || containsWordFree(stringValue(model["name"])) {
		return true
	}
	prices := make([]any, 0)
	collectPricingValues(model, false, &prices)
	if len(prices) == 0 {
		return false
	}
	for _, price := range prices {
		if !isZeroPrice(price) {
			return false
		}
	}
	return true
}

func fallbackByID(id string) (FreeModel, bool) {
	for _, model := range fallbackModels {
		if model.ID == id {
			return model, true
		}
	}
	return FreeModel{}, false
}

func stringValue(value any) string { text, _ := value.(string); return strings.TrimSpace(text) }

func containsWordFree(value string) bool {
	for _, word := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if word == "free" {
			return true
		}
	}
	return false
}

func collectPricingValues(value any, inPricing bool, out *[]any) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, child := range object {
		normalized := strings.ToLower(key)
		pricing := inPricing || strings.Contains(normalized, "price") || strings.Contains(normalized, "cost")
		if _, nested := child.(map[string]any); nested {
			collectPricingValues(child, pricing, out)
			continue
		}
		priceKey := strings.Contains(normalized, "price") || strings.Contains(normalized, "cost")
		if inPricing && (strings.Contains(normalized, "input") || strings.Contains(normalized, "output") || strings.Contains(normalized, "prompt") || strings.Contains(normalized, "completion") || strings.Contains(normalized, "cache") || strings.Contains(normalized, "read") || strings.Contains(normalized, "write")) {
			priceKey = true
		}
		if priceKey {
			*out = append(*out, child)
		}
	}
}

func isZeroPrice(value any) bool {
	switch raw := value.(type) {
	case json.Number:
		parsed, err := raw.Float64()
		return err == nil && parsed == 0
	case float64:
		return raw == 0
	case string:
		normalized := strings.ToLower(strings.TrimSpace(raw))
		if normalized == "" || normalized == "-" || normalized == "free" {
			return true
		}
		normalized = strings.NewReplacer("$", "", ",", "", " ", "").Replace(normalized)
		parsed, err := strconv.ParseFloat(normalized, 64)
		return err == nil && parsed == 0
	default:
		return false
	}
}
