package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// GetNeonixSettings reads the JSON-backed key/value settings used by the
// existing Neonix UI. The canonical Sub2API settings API exposes typed system
// settings; this helper keeps the compatibility store isolated at the edge.
func (s *SettingService) GetNeonixSettings(ctx context.Context, keys []string) (map[string]any, error) {
	if s == nil || s.settingRepo == nil {
		return nil, fmt.Errorf("setting repository is not configured")
	}
	raw, err := s.settingRepo.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("get Neonix settings: %w", err)
	}
	if len(keys) == 0 {
		keys = make([]string, 0, len(raw))
		for key := range raw {
			keys = append(keys, key)
		}
	}
	result := make(map[string]any, len(keys))
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			result[key] = decodeNeonixSetting(value)
		}
	}
	return result, nil
}

// GetNeonixSetting returns one JSON-backed setting. Missing keys are reported
// as a nil value so the direct endpoint matches the Node compatibility route.
func (s *SettingService) GetNeonixSetting(ctx context.Context, key string) (any, error) {
	if s == nil || s.settingRepo == nil {
		return nil, fmt.Errorf("setting repository is not configured")
	}
	raw, err := s.settingRepo.GetValue(ctx, key)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get Neonix setting: %w", err)
	}
	return decodeNeonixSetting(raw), nil
}

// SetNeonixSetting persists a value in the same JSON representation used by
// the Node implementation. This preserves booleans/numbers/objects across a
// rolling cutover instead of flattening everything into display strings.
func (s *SettingService) SetNeonixSetting(ctx context.Context, key string, value any) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting repository is not configured")
	}
	if strings.TrimSpace(key) == "" || len(key) > 128 {
		return fmt.Errorf("invalid setting key")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode Neonix setting: %w", err)
	}
	if err := s.settingRepo.Set(ctx, key, string(encoded)); err != nil {
		return fmt.Errorf("set Neonix setting: %w", err)
	}
	if s.onUpdate != nil {
		s.onUpdate()
	}
	return nil
}

// SetNeonixSettings persists a batch atomically through the repository's
// upsert operation and notifies the normal settings cache invalidation hook
// once after the batch completes.
func (s *SettingService) SetNeonixSettings(ctx context.Context, values map[string]any) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting repository is not configured")
	}
	encoded := make(map[string]string, len(values))
	for key, value := range values {
		if strings.TrimSpace(key) == "" || len(key) > 128 {
			return fmt.Errorf("invalid setting key")
		}
		payload, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode Neonix setting %q: %w", key, err)
		}
		encoded[key] = string(payload)
	}
	if err := s.settingRepo.SetMultiple(ctx, encoded); err != nil {
		return fmt.Errorf("set Neonix settings: %w", err)
	}
	if len(encoded) > 0 && s.onUpdate != nil {
		s.onUpdate()
	}
	return nil
}

func decodeNeonixSetting(raw string) any {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err == nil {
		return value
	}
	// Older rows written before JSON serialization remain readable.
	return raw
}
