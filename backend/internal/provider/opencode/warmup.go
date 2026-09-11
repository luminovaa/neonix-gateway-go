// Package opencode contains the provider-specific policy that is safe to share
// between model sync and account warmup.
package opencode

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
)

type CachedModel struct {
	ModelID       string
	ActualModelID string
	Status        string
	SortOrder     *int
	Deleted       bool
}

type Candidate struct {
	ModelID       string
	ActualModelID string
	SortOrder     int
}

// ResolveWarmupCandidates uses the last successful catalogue. A non-empty
// catalogue with no available rows intentionally returns no bootstrap model:
// it means the catalogue needs a sync and must not turn a healthy credential
// into a false authentication failure.
func ResolveWarmupCandidates(models []CachedModel, fallbackIDs []string, mappings map[string]string) []Candidate {
	ocModels := make([]CachedModel, 0, len(models))
	for _, model := range models {
		if !model.Deleted {
			ocModels = append(ocModels, model)
		}
	}
	candidates := make([]Candidate, 0, len(ocModels))
	for _, model := range ocModels {
		if !strings.EqualFold(strings.TrimSpace(model.Status), "AVAILABLE") || strings.TrimSpace(model.ActualModelID) == "" {
			continue
		}
		sortOrder := int(^uint(0) >> 1)
		if model.SortOrder != nil {
			sortOrder = *model.SortOrder
		}
		candidates = append(candidates, Candidate{ModelID: model.ModelID, ActualModelID: strings.TrimSpace(model.ActualModelID), SortOrder: sortOrder})
	}
	if len(candidates) > 0 || len(ocModels) > 0 {
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].SortOrder != candidates[j].SortOrder {
				return candidates[i].SortOrder < candidates[j].SortOrder
			}
			return candidates[i].ModelID < candidates[j].ModelID
		})
		return candidates
	}
	for index, modelID := range fallbackIDs {
		actual := mappings[modelID]
		if actual == "" {
			actual = strings.TrimPrefix(modelID, "oc/")
		}
		candidates = append(candidates, Candidate{ModelID: modelID, ActualModelID: actual, SortOrder: index})
	}
	return candidates
}

// IsModelUnavailableError is deliberately narrow. Only an upstream response
// that identifies the requested model as unsupported may try another model.
func IsModelUnavailableError(status int, message string) bool {
	if status != 400 && status != 404 {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(message))
	if text == "" || strings.Contains(text, "route not found") || strings.Contains(text, "endpoint not found") {
		return false
	}
	for _, marker := range []string{"model not found", "model_not_found", "unsupported model", "model unavailable", "unknown model"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func DescribeStaleCatalogue(_ string) error {
	return errors.New("OpenCode Zen free model catalogue is stale; sync models and retry warmup")
}

// ParseSortOrder is a boundary helper for JSON-backed catalogue metadata.
func ParseSortOrder(value any) *int {
	var order int
	switch raw := value.(type) {
	case int:
		order = raw
	case int64:
		order = int(raw)
	case float64:
		if math.IsNaN(raw) || math.IsInf(raw, 0) {
			return nil
		}
		order = int(raw)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil
		}
		order = parsed
	default:
		return nil
	}
	return &order
}
