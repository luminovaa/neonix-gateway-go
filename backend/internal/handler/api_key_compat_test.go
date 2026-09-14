package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/pagination"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/usagestats"
	middleware2 "github.com/luminovaa/neonix-gateway-go/internal/server/middleware"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type compatAPIKeyRepoStub struct {
	service.APIKeyRepository
	keys   []*service.APIKey
	legacy map[string]int64
}

func (r *compatAPIKeyRepoStub) GetByLegacyID(_ context.Context, legacyID string) (*service.APIKey, error) {
	id, ok := r.legacy[legacyID]
	if !ok {
		return nil, service.ErrAPIKeyNotFound
	}
	return r.GetByID(context.Background(), id)
}

func (r *compatAPIKeyRepoStub) ListByUserID(_ context.Context, userID int64, _ pagination.PaginationParams, _ service.APIKeyListFilters) ([]service.APIKey, *pagination.PaginationResult, error) {
	result := make([]service.APIKey, 0, len(r.keys))
	for _, key := range r.keys {
		if key != nil && key.UserID == userID {
			result = append(result, *key)
		}
	}
	return result, &pagination.PaginationResult{Total: int64(len(result)), Page: 1, PageSize: 1000, Pages: 1}, nil
}

func (r *compatAPIKeyRepoStub) Create(_ context.Context, key *service.APIKey) error {
	key.ID = int64(len(r.keys) + 1)
	key.CreatedAt = time.Unix(1700000000+int64(key.ID), 0).UTC()
	key.UpdatedAt = key.CreatedAt
	r.keys = append(r.keys, key)
	return nil
}

func (r *compatAPIKeyRepoStub) GetByID(_ context.Context, id int64) (*service.APIKey, error) {
	for _, key := range r.keys {
		if key != nil && key.ID == id {
			clone := *key
			return &clone, nil
		}
	}
	return nil, service.ErrAPIKeyNotFound
}

func (r *compatAPIKeyRepoStub) GetKeyAndOwnerID(_ context.Context, id int64) (string, int64, error) {
	for _, key := range r.keys {
		if key != nil && key.ID == id {
			return key.Key, key.UserID, nil
		}
	}
	return "", 0, service.ErrAPIKeyNotFound
}

func (r *compatAPIKeyRepoStub) DeleteWithAudit(_ context.Context, id int64) error {
	for i, key := range r.keys {
		if key != nil && key.ID == id {
			r.keys = append(r.keys[:i], r.keys[i+1:]...)
			return nil
		}
	}
	return service.ErrAPIKeyNotFound
}

type compatUserRepoStub struct {
	service.UserRepository
	user *service.User
}

type compatUsageRepoStub struct {
	service.UsageLogRepository
}

func (r *compatUsageRepoStub) GetAPIKeyStatsAggregated(context.Context, int64, time.Time, time.Time) (*usagestats.UsageStats, error) {
	return &usagestats.UsageStats{TotalRequests: 4, TotalInputTokens: 10, TotalOutputTokens: 20, TotalActualCost: 1.25}, nil
}

func (r *compatUsageRepoStub) GetModelStatsWithFilters(context.Context, time.Time, time.Time, int64, int64, int64, int64, *int16, *bool, *int8) ([]usagestats.ModelStat, error) {
	return []usagestats.ModelStat{{Model: "gpt-test", Requests: 4, InputTokens: 10, OutputTokens: 20, ActualCost: 1.25}}, nil
}

func (r *compatUsageRepoStub) GetAPIKeyRequestStats(context.Context, int64, time.Time, time.Time) (*service.APIKeyRequestStats, error) {
	return &service.APIKeyRequestStats{TotalRequests: 5, SuccessRequests: 4}, nil
}

func (r *compatUsageRepoStub) GetAPIKeyAccessStatsSince(context.Context, int64, time.Time) (*service.APIKeyAccessStats, error) {
	last := time.UnixMilli(1710000000123).UTC()
	return &service.APIKeyAccessStats{TotalRequests: 5, UniqueIPs: 2, UniqueUserAgents: 3, LastAccessedAt: &last}, nil
}

func (r *compatUserRepoStub) GetByID(context.Context, int64) (*service.User, error) {
	return r.user, nil
}

func newCompatAPIKeyHandler(repo *compatAPIKeyRepoStub) *APIKeyHandler {
	userRepo := &compatUserRepoStub{user: &service.User{ID: 7, Role: service.RoleAdmin, Status: service.StatusActive}}
	return NewAPIKeyHandler(service.NewAPIKeyService(repo, userRepo, nil, nil, nil, nil, nil))
}

func withCompatSubject(handler gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7})
		handler(c)
	}
}

func TestNeonixAPIKeyCompatMapperKeepsDirectCamelCaseShape(t *testing.T) {
	created := time.UnixMilli(1710000000123).UTC()
	key := neonixAPIKeyFromService(&service.APIKey{
		ID:        42,
		UserID:    7,
		Name:      "default",
		Key:       "neon-1234567890123456",
		Status:    service.StatusAPIKeyActive,
		CreatedAt: created,
	})

	encoded, err := json.Marshal(key)
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"42","userId":"7","name":"default","keyPrefix":"neon-1234567","key":"neon-1234567890123456","createdAt":1710000000123,"isActive":true}`, string(encoded))
}

func TestListCompatReturnsArrayAndBootstrapsDefaultKey(t *testing.T) {
	repo := &compatAPIKeyRepoStub{}
	h := newCompatAPIKeyHandler(repo)
	router := gin.New()
	router.GET("/api/api-keys", withCompatSubject(h.ListCompat))

	req := httptest.NewRequest(http.MethodGet, "/api/api-keys", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope struct {
		Data []*neonixAPIKey `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	body := envelope.Data
	require.Len(t, body, 1)
	require.Equal(t, "default", body[0].Name)
	require.NotEmpty(t, body[0].Key)
	require.Len(t, repo.keys, 1)
}

func TestMeCompatPrefersDefaultKey(t *testing.T) {
	repo := &compatAPIKeyRepoStub{keys: []*service.APIKey{
		{ID: 1, UserID: 7, Name: "client", Key: "neon-client-key", Status: service.StatusAPIKeyActive, CreatedAt: time.Now()},
		{ID: 2, UserID: 7, Name: "default", Key: "neon-default-key", Status: service.StatusAPIKeyActive, CreatedAt: time.Now()},
	}}
	h := newCompatAPIKeyHandler(repo)
	router := gin.New()
	router.GET("/api/api-keys/me", withCompatSubject(h.MeCompat))

	req := httptest.NewRequest(http.MethodGet, "/api/api-keys/me", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope struct {
		Data neonixAPIKey `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	body := envelope.Data
	require.Equal(t, "2", body.ID)
	require.Equal(t, "neon-default-key", body.Key)
}

func TestEnsureDefaultCompatKeyAddsDefaultForCustomOnlyInstall(t *testing.T) {
	repo := &compatAPIKeyRepoStub{keys: []*service.APIKey{
		{ID: 1, UserID: 7, Name: "client", Key: "neon-client-key", Status: service.StatusAPIKeyActive, CreatedAt: time.Now()},
	}}
	h := newCompatAPIKeyHandler(repo)
	router := gin.New()
	router.GET("/api/api-keys/me", withCompatSubject(h.MeCompat))

	req := httptest.NewRequest(http.MethodGet, "/api/api-keys/me", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope struct {
		Data neonixAPIKey `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, "default", envelope.Data.Name)
	require.Len(t, repo.keys, 2)
}

func TestAPIKeyCompatUsageAndAccessStatsUseGoAggregates(t *testing.T) {
	repo := &compatAPIKeyRepoStub{keys: []*service.APIKey{
		{ID: 4, UserID: 7, Name: "default", Key: "neon-default-key", Status: service.StatusAPIKeyActive, CreatedAt: time.Now()},
	}}
	h := newCompatAPIKeyHandler(repo)
	h.SetUsageService(service.NewUsageService(&compatUsageRepoStub{}, nil, nil, nil))
	router := gin.New()
	router.GET("/api/api-keys/:id/usage", withCompatSubject(h.GetUsageCompat))
	router.GET("/api/api-keys/:id/access-stats", withCompatSubject(h.GetAccessStatsCompat))

	usageReq := httptest.NewRequest(http.MethodGet, "/api/api-keys/4/usage", nil)
	usageRec := httptest.NewRecorder()
	router.ServeHTTP(usageRec, usageReq)
	require.Equal(t, http.StatusOK, usageRec.Code)
	var usageEnvelope struct {
		Data neonixAPIKeyUsage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(usageRec.Body.Bytes(), &usageEnvelope))
	require.Equal(t, int64(5), usageEnvelope.Data.TotalRequests)
	require.Equal(t, int64(4), usageEnvelope.Data.SuccessRequests)
	require.Len(t, usageEnvelope.Data.Models, 1)

	accessReq := httptest.NewRequest(http.MethodGet, "/api/api-keys/4/access-stats", nil)
	accessRec := httptest.NewRecorder()
	router.ServeHTTP(accessRec, accessReq)
	require.Equal(t, http.StatusOK, accessRec.Code)
	var accessEnvelope struct {
		Data neonixAPIKeyAccessStats `json:"data"`
	}
	require.NoError(t, json.Unmarshal(accessRec.Body.Bytes(), &accessEnvelope))
	require.Equal(t, int64(2), accessEnvelope.Data.UniqueIPs)
	require.NotNil(t, accessEnvelope.Data.LastAccessed)
}

func TestMeCompatCanResolveLegacyTextIDForUsage(t *testing.T) {
	repo := &compatAPIKeyRepoStub{
		keys:   []*service.APIKey{{ID: 4, UserID: 7, Name: "default", Key: "neon-default-key", Status: service.StatusAPIKeyActive, CreatedAt: time.Now()}},
		legacy: map[string]int64{"legacy-key-uuid": 4},
	}
	h := newCompatAPIKeyHandler(repo)
	key, err := h.apiKeyService.GetByCompatID(context.Background(), 7, "legacy-key-uuid")
	require.NoError(t, err)
	require.Equal(t, int64(4), key.ID)
}

func TestRegenerateCompatCreatesReplacementBeforeRemovingDefault(t *testing.T) {
	repo := &compatAPIKeyRepoStub{keys: []*service.APIKey{
		{ID: 1, UserID: 7, Name: "default", Key: "neon-old-key", Status: service.StatusAPIKeyActive, CreatedAt: time.Now()},
	}}
	h := newCompatAPIKeyHandler(repo)
	router := gin.New()
	router.POST("/api/api-keys/regenerate", withCompatSubject(h.RegenerateCompat))

	req := httptest.NewRequest(http.MethodPost, "/api/api-keys/regenerate", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope struct {
		Data neonixAPIKey `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, "default", envelope.Data.Name)
	require.NotEqual(t, "neon-old-key", envelope.Data.Key)
	require.Len(t, repo.keys, 1)
	require.Equal(t, envelope.Data.Key, repo.keys[0].Key)
}
