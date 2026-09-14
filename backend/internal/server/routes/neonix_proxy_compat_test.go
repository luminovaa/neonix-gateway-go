package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/config"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type neonixProxySettingRepoStub struct {
	service.SettingRepository
	values map[string]string
}

func (r *neonixProxySettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}

func (r *neonixProxySettingRepoStub) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

func (r *neonixProxySettingRepoStub) SetMultiple(_ context.Context, values map[string]string) error {
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func (r *neonixProxySettingRepoStub) GetAll(context.Context) (map[string]string, error) {
	copyValues := make(map[string]string, len(r.values))
	for key, value := range r.values {
		copyValues[key] = value
	}
	return copyValues, nil
}

func TestNormalizeNeonixProxyConfigDropsSecretsAndUnknownFields(t *testing.T) {
	config := normalizeNeonixProxyConfig(map[string]any{
		"port":       float64(8080),
		"host":       " 127.0.0.1 ",
		"apiKey":     "do-not-store",
		"apiKeys":    []any{"secret"},
		"password":   "do-not-store",
		"unknown":    true,
		"maxRetries": float64(3),
		"tls":        map[string]any{"enabled": true, "certPath": "/safe/cert", "cert": "PRIVATE CERT", "key": "PRIVATE KEY"},
	})
	require.Equal(t, 8080, config["port"])
	require.Equal(t, "127.0.0.1", config["host"])
	require.Equal(t, 3, config["maxRetries"])
	_, hasAPIKey := config["apiKey"]
	require.False(t, hasAPIKey)
	_, hasUnknown := config["unknown"]
	require.False(t, hasUnknown)
	tls, ok := config["tls"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "/safe/cert", tls["certPath"])
	_, hasCert := tls["cert"]
	require.False(t, hasCert)
	_, hasKey := tls["key"]
	require.False(t, hasKey)
}

func TestNeonixProxyConfigRuntimeUsesDefaultsAndPersistsSafePatch(t *testing.T) {
	repo := &neonixProxySettingRepoStub{values: map[string]string{}}
	settings := service.NewSettingService(repo, nil)
	runtime := newNeonixProxyConfigRuntime(settings, &config.Config{Server: config.ServerConfig{Host: "127.0.0.1", Port: 8080}})
	router := gin.New()
	router.GET("/api/proxy/config", runtime.get)
	router.PUT("/api/proxy/config", runtime.update)

	getReq := httptest.NewRequest(http.MethodGet, "/api/proxy/config", nil)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code)
	var getEnvelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getEnvelope))
	require.Equal(t, float64(8080), getEnvelope.Data["port"])

	updateReq := httptest.NewRequest(http.MethodPut, "/api/proxy/config", strings.NewReader(`{"port":9090,"apiKey":"secret","host":" localhost "}`))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	router.ServeHTTP(updateRec, updateReq)
	require.Equal(t, http.StatusOK, updateRec.Code)
	require.NotContains(t, repo.values[neonixProxyConfigSettingKey], "secret")
	var updateEnvelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(updateRec.Body.Bytes(), &updateEnvelope))
	require.Equal(t, float64(9090), updateEnvelope.Data["port"])
	require.Equal(t, "localhost", updateEnvelope.Data["host"])
}
