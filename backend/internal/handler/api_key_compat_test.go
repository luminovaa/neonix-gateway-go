package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/pagination"
	middleware2 "github.com/luminovaa/neonix-gateway-go/internal/server/middleware"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type compatAPIKeyRepoStub struct {
	service.APIKeyRepository
	keys []*service.APIKey
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

type compatUserRepoStub struct {
	service.UserRepository
	user *service.User
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
