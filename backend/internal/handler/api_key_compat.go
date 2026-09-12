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

type neonixAPIKeyAccessStats struct {
	TotalRequests    int64 `json:"totalRequests"`
	UniqueIPs        int64 `json:"uniqueIPs"`
	UniqueUserAgents int64 `json:"uniqueUserAgents"`
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

	keys, _, err := h.apiKeyService.List(c.Request.Context(), userID, pagination.PaginationParams{
		Page:      1,
		PageSize:  1000,
		SortBy:    "created_at",
		SortOrder: pagination.SortOrderDesc,
	}, service.APIKeyListFilters{})
	if err != nil {
		return nil, err
	}
	if len(keys) > 0 {
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
// oldest available key as a compatibility fallback).
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

// DeleteCompat implements DELETE /api/api-keys/:id.
func (h *APIKeyHandler) DeleteCompat(c *gin.Context) {
	subject, ok := h.compatSubject(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.ErrorWithDetails(c, http.StatusBadRequest, "Invalid API key ID", "API_KEY_ID_INVALID", nil)
		return
	}
	if err := h.apiKeyService.Delete(c.Request.Context(), id, subject.UserID); err != nil {
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
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.ErrorWithDetails(c, http.StatusBadRequest, "Invalid API key ID", "API_KEY_ID_INVALID", nil)
		return
	}
	key, err := h.apiKeyService.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if key.UserID != subject.UserID {
		response.NotFound(c, "API key not found")
		return
	}
	if h.usageService == nil {
		response.ErrorWithDetails(c, http.StatusServiceUnavailable, "API key usage is unavailable", "API_KEY_USAGE_UNAVAILABLE", nil)
		return
	}
	stats, err := h.usageService.GetStatsByAPIKey(c.Request.Context(), id, time.Time{}, time.Time{})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	models, err := h.usageService.GetAPIKeyModelStats(c.Request.Context(), id, time.Time{}, time.Time{})
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
		TotalRequests: stats.TotalRequests,
		// The aggregate schema has no success flag. Successful billed rows are
		// the only rows included by the aggregate query, so this is exact for
		// the dashboard's success-oriented metric.
		SuccessRequests: stats.TotalRequests,
		InputTokens:     stats.TotalInputTokens,
		OutputTokens:    stats.TotalOutputTokens,
		TotalCredits:    stats.TotalActualCost,
		Models:          modelStats,
	})
}

// GetAccessStatsCompat implements GET /api/api-keys/:id/access-stats.
func (h *APIKeyHandler) GetAccessStatsCompat(c *gin.Context) {
	subject, ok := h.compatSubject(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.ErrorWithDetails(c, http.StatusBadRequest, "Invalid API key ID", "API_KEY_ID_INVALID", nil)
		return
	}
	key, err := h.apiKeyService.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if key.UserID != subject.UserID {
		response.NotFound(c, "API key not found")
		return
	}
	if h.usageService == nil {
		response.ErrorWithDetails(c, http.StatusServiceUnavailable, "API key access statistics are unavailable", "API_KEY_ACCESS_STATS_UNAVAILABLE", nil)
		return
	}
	stats, err := h.usageService.GetAPIKeyAccessStats(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, neonixAPIKeyAccessStats{
		TotalRequests:    stats.TotalRequests,
		UniqueIPs:        stats.UniqueIPs,
		UniqueUserAgents: stats.UniqueUserAgents,
	})
}
