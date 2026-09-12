package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type compatSettingRepoStub struct {
	service.SettingRepository
	values map[string]string
}

func (r *compatSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}

func (r *compatSettingRepoStub) GetAll(context.Context) (map[string]string, error) {
	copyValues := make(map[string]string, len(r.values))
	for key, value := range r.values {
		copyValues[key] = value
	}
	return copyValues, nil
}

func (r *compatSettingRepoStub) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

func (r *compatSettingRepoStub) SetMultiple(_ context.Context, values map[string]string) error {
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func newCompatSettingHandler(repo *compatSettingRepoStub) *SettingHandler {
	return &SettingHandler{settingService: service.NewSettingService(repo, nil)}
}

func TestGetNeonixSettingsCompatDecodesJSONAndInitializesFooterDefaults(t *testing.T) {
	repo := &compatSettingRepoStub{values: map[string]string{
		"theme":             `"dark"`,
		"autoWarmupEnabled": "true",
	}}
	h := newCompatSettingHandler(repo)
	router := gin.New()
	router.GET("/api/settings", h.GetNeonixSettingsCompat)

	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, "dark", envelope.Data["theme"])
	require.Equal(t, true, envelope.Data["autoWarmupEnabled"])
	require.Equal(t, true, envelope.Data["response_footer_enabled"])
	require.Equal(t, "Powered by Neonix", envelope.Data["response_footer_text"])
	require.Equal(t, `true`, repo.values["response_footer_enabled"])
	require.Equal(t, `"Powered by Neonix"`, repo.values["response_footer_text"])
}

func TestNeonixSettingCompatRoundTripsTypedValue(t *testing.T) {
	repo := &compatSettingRepoStub{values: map[string]string{}}
	h := newCompatSettingHandler(repo)
	router := gin.New()
	router.PUT("/api/settings/:key", h.SetNeonixSettingCompat)
	router.GET("/api/settings/:key", h.GetNeonixSettingCompat)

	putReq := httptest.NewRequest(http.MethodPut, "/api/settings/autoRefresh", strings.NewReader(`{"value":false}`))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	router.ServeHTTP(putRec, putReq)
	require.Equal(t, http.StatusOK, putRec.Code)
	require.Equal(t, "false", repo.values["autoRefresh"])

	getReq := httptest.NewRequest(http.MethodGet, "/api/settings/autoRefresh", nil)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code)
	var envelope struct {
		Data struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &envelope))
	require.Equal(t, "autoRefresh", envelope.Data.Key)
	require.Equal(t, false, envelope.Data.Value)
}
