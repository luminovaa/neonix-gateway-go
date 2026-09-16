package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/pagination"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	middleware2 "github.com/luminovaa/neonix-gateway-go/internal/server/middleware"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

// neonixAPIKey is the intentionally small direct-JSON shape consumed by the
// existing Neonix control plane. The canonical Sub2API API-key handlers keep
// their envelope and snake_case DTOs under /api/v1; this mapper belongs only
// to the compatibility boundary.
type neonixAPIKey struct {
	ID            string   `json:"id"`
	UserID        string   `json:"userId"`
	Name          string   `json:"name"`
	KeyPrefix     string   `json:"keyPrefix"`
	Key           string   `json:"key,omitempty"`
	CreatedAt     int64    `json:"createdAt"`
	LastUsedAt    *int64   `json:"lastUsedAt,omitempty"`
	IsActive      bool     `json:"isActive"`
	KeyKind       string   `json:"keyKind,omitempty"`
	TokenLimit    float64  `json:"tokenLimit,omitempty"`
	TokensUsed    float64  `json:"tokensUsed,omitempty"`
	AllowedModels []string `json:"allowedModels,omitempty"`
	RateLimitRPM  *float64 `json:"rateLimitRpm,omitempty"`
	Notes         *string  `json:"notes,omitempty"`
}

type neonixAPIKeyUsage struct {
	TotalRequests   int64                    `json:"totalRequests"`
	SuccessRequests int64                    `json:"successRequests"`
	InputTokens     int64                    `json:"inputTokens"`
	OutputTokens    int64                    `json:"outputTokens"`
	TotalCredits    float64                  `json:"totalCredits"`
	Models          []neonixAPIKeyModelUsage `json:"models"`
}

type neonixAPIKeyModelUsage struct {
	Model        string  `json:"model"`
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	Credits      float64 `json:"credits"`
}

func apiKeyCompatPrefix(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12]
}

func neonixAPIKeyFromService(key *service.APIKey) *neonixAPIKey {
	if key == nil {
		return nil
	}
	result := &neonixAPIKey{
		ID:            strconv.FormatInt(key.ID, 10),
		UserID:        strconv.FormatInt(key.UserID, 10),
		Name:          key.Name,
		KeyPrefix:     apiKeyCompatPrefix(key.Key),
		Key:           key.Key,
		CreatedAt:     key.CreatedAt.UnixMilli(),
		IsActive:      key.Status == service.StatusAPIKeyActive,
		AllowedModels: nil,
	}
	if key.LastUsedAt != nil {
		value := key.LastUsedAt.UnixMilli()
		result.LastUsedAt = &value
	}
	return result
}

func (h *APIKeyHandler) compatSubject(c *gin.Context) (middleware2.AuthSubject, bool) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return middleware2.AuthSubject{}, false
	}
	if h == nil || h.apiKeyService == nil {
		response.ErrorWithDetails(c, http.StatusInternalServerError, "API key service is not configured", "API_KEY_SERVICE_UNAVAILABLE", nil)
		return middleware2.AuthSubject{}, false
	}
	return subject, true
}

// ensureDefaultCompatKey preserves Neonix's bootstrap contract: the first
// read creates one operator key named "default". The mutex prevents two
// simultaneous login/bootstrap requests from creating duplicate keys.
func (h *APIKeyHandler) ensureDefaultCompatKey(c *gin.Context, userID int64) ([]service.APIKey, error) {
	h.compatMu.Lock()
	defer h.compatMu.Unlock()

	return h.ensureDefaultCompatKeyLocked(c, userID)
}

func (h *APIKeyHandler) listCompatKeys(c *gin.Context, userID int64) ([]service.APIKey, error) {
	keys, _, err := h.apiKeyService.List(c.Request.Context(), userID, pagination.PaginationParams{
		Page:      1,
		PageSize:  1000,
		SortBy:    "created_at",
		SortOrder: pagination.SortOrderDesc,
	}, service.APIKeyListFilters{})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func (h *APIKeyHandler) ensureDefaultCompatKeyLocked(c *gin.Context, userID int64) ([]service.APIKey, error) {
	keys, err := h.listCompatKeys(c, userID)
	if err != nil {
		return nil, err
	}
	if len(keys) > 0 {
		for _, key := range keys {
			if strings.EqualFold(strings.TrimSpace(key.Name), "default") {
				return keys, nil
			}
		}
		// Match the Node bootstrap behavior for custom-only installations: add
		// the default key while the per-user key limit still permits it.
		if len(keys) < 3 {
			created, err := h.apiKeyService.Create(c.Request.Context(), userID, service.CreateAPIKeyRequest{Name: "default"})
			if err != nil {
				return nil, err
			}
			keys = append(keys, *created)
		}
		return keys, nil
	}
	created, err := h.apiKeyService.Create(c.Request.Context(), userID, service.CreateAPIKeyRequest{Name: "default"})
	if err != nil {
		return nil, err
	}
	return []service.APIKey{*created}, nil
}

// ListCompat implements GET /api/api-keys with the legacy direct array shape.
func (h *APIKeyHandler) ListCompat(c *gin.Context) {
	subject, ok := h.compatSubject(c)
	if !ok {
		return
	}
	keys, err := h.ensureDefaultCompatKey(c, subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	result := make([]*neonixAPIKey, 0, len(keys))
	for i := range keys {
		result = append(result, neonixAPIKeyFromService(&keys[i]))
	}
	response.Success(c, result)
}

// MeCompat implements GET /api/api-keys/me and returns the default key (or the
// newest available key as a compatibility fallback).
func (h *APIKeyHandler) MeCompat(c *gin.Context) {
	subject, ok := h.compatSubject(c)
	if !ok {
		return
	}
	keys, err := h.ensureDefaultCompatKey(c, subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	selected := &keys[0]
	for i := range keys {
		if strings.EqualFold(strings.TrimSpace(keys[i].Name), "default") {
			selected = &keys[i]
			break
		}
	}
	response.Success(c, neonixAPIKeyFromService(selected))
}

// CreateCompat implements POST /api/api-keys. The name-only payload mirrors
// Neonix while delegating key generation and validation to the canonical Go
// service.
func (h *APIKeyHandler) CreateCompat(c *gin.Context) {
	subject, ok := h.compatSubject(c)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		response.ErrorWithDetails(c, http.StatusBadRequest, "API key name is required", "API_KEY_NAME_REQUIRED", nil)
		return
	}
	created, err := h.apiKeyService.Create(c.Request.Context(), subject.UserID, service.CreateAPIKeyRequest{Name: strings.TrimSpace(req.Name)})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, neonixAPIKeyFromService(created))
}

// RegenerateCompat implements POST /api/api-keys/regenerate. A fresh key is
// created before the previous default is removed so a failed entropy/DB write
// never strands the operator without a working client credential.
func (h *APIKeyHandler) RegenerateCompat(c *gin.Context) {
	subject, ok := h.compatSubject(c)
	if !ok {
		return
	}
	h.compatMu.Lock()
	defer h.compatMu.Unlock()

	keys, err := h.listCompatKeys(c, subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if len(keys) == 0 {
		created, err := h.apiKeyService.Create(c.Request.Context(), subject.UserID, service.CreateAPIKeyRequest{Name: "default"})
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, neonixAPIKeyFromService(created))
		return
	}
	var oldDefault *service.APIKey
	for i := range keys {
		if strings.EqualFold(strings.TrimSpace(keys[i].Name), "default") {
			copy := keys[i]
			oldDefault = &copy
			break
		}
	}
	victim := oldDefault
	if victim == nil && len(keys) >= 3 {
		// The legacy route evicted the last key when the per-user limit was
		// reached. Keep the same bounded cardinality for custom-only installs.
		copy := keys[len(keys)-1]
		victim = &copy
	}
	created, err := h.apiKeyService.Create(c.Request.Context(), subject.UserID, service.CreateAPIKeyRequest{Name: "default"})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if victim != nil {
		if err := h.apiKeyService.Delete(c.Request.Context(), victim.ID, subject.UserID); err != nil {
			// Roll back the replacement where possible. The old key remains
			// usable if the cleanup operation itself failed.
			_ = h.apiKeyService.Delete(c.Request.Context(), created.ID, subject.UserID)
			response.ErrorFrom(c, err)
			return
		}
	}
	response.Success(c, neonixAPIKeyFromService(created))
}

// DeleteCompat implements DELETE /api/api-keys/:id.
func (h *APIKeyHandler) DeleteCompat(c *gin.Context) {
	subject, ok := h.compatSubject(c)
	if !ok {
		return
	}
	rawID := strings.TrimSpace(c.Param("id"))
	if rawID == "" {
		response.ErrorWithDetails(c, http.StatusBadRequest, "Invalid API key ID", "API_KEY_ID_INVALID", nil)
		return
	}
	key, err := h.apiKeyService.GetByCompatID(c.Request.Context(), subject.UserID, rawID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if err := h.apiKeyService.Delete(c.Request.Context(), key.ID, subject.UserID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true})
}

// GetUsageCompat implements GET /api/api-keys/:id/usage. Usage is read from
// the same aggregate store used by the Go dashboard, keeping this route useful
// during the Node → Go UI cutover.
func (h *APIKeyHandler) GetUsageCompat(c *gin.Context) {
	subject, ok := h.compatSubject(c)
	if !ok {
		return
	}
	rawID := strings.TrimSpace(c.Param("id"))
	if rawID == "" {
		response.ErrorWithDetails(c, http.StatusBadRequest, "Invalid API key ID", "API_KEY_ID_INVALID", nil)
		return
	}
	key, err := h.apiKeyService.GetByCompatID(c.Request.Context(), subject.UserID, rawID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if h.usageService == nil {
		response.ErrorWithDetails(c, http.StatusServiceUnavailable, "API key usage is unavailable", "API_KEY_USAGE_UNAVAILABLE", nil)
		return
	}
	startTime, endTime := neonixAllTimeUsageRange()
	stats, err := h.usageService.GetStatsByAPIKey(c.Request.Context(), key.ID, startTime, endTime)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	requestStats, err := h.usageService.GetAPIKeyRequestStats(c.Request.Context(), key.ID, startTime, endTime)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	models, err := h.usageService.GetAPIKeyModelStats(c.Request.Context(), key.ID, startTime, endTime)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	modelStats := make([]neonixAPIKeyModelUsage, 0, len(models))
	for _, model := range models {
		modelStats = append(modelStats, neonixAPIKeyModelUsage{
			Model:        model.Model,
			Requests:     model.Requests,
			InputTokens:  model.InputTokens,
			OutputTokens: model.OutputTokens,
			Credits:      model.ActualCost,
		})
	}
	response.Success(c, neonixAPIKeyUsage{
		TotalRequests:   requestStats.TotalRequests,
		SuccessRequests: requestStats.SuccessRequests,
		InputTokens:     stats.TotalInputTokens,
		OutputTokens:    stats.TotalOutputTokens,
		TotalCredits:    stats.TotalActualCost,
		Models:          modelStats,
	})
}

func neonixAllTimeUsageRange() (time.Time, time.Time) {
	return time.Unix(0, 0).UTC(), time.Now().UTC().Add(time.Nanosecond)
}
