package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCustomPoolStatusCompatUsesCanonicalProxiesAndRedactsCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &stubAdminService{
		proxies: []service.Proxy{
			{ID: 1, Protocol: "http", Host: "proxy.example", Port: 8080, Username: "alice", Password: "secret", Status: service.StatusActive},
			{ID: 2, Protocol: "socks5", Host: "saved.example", Port: 1080, Username: "bob", Password: "hidden", Status: service.StatusDisabled},
		},
		proxyCounts: []service.ProxyWithAccountCount{{
			Proxy:        service.Proxy{ID: 1, Protocol: "http", Host: "proxy.example", Port: 8080, Username: "alice", Password: "secret", Status: service.StatusActive},
			AccountCount: 3, LatencyStatus: "success", IPAddress: "203.0.113.9",
		}},
	}
	router := gin.New()
	router.GET("/status", NewProxyHandler(svc).CustomPoolStatusCompat)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "secret")
	require.NotContains(t, recorder.Body.String(), "hidden")
	require.NotContains(t, recorder.Body.String(), "alice")

	var envelope struct {
		Data neonixProxyPoolStatus `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.True(t, envelope.Data.Enabled)
	require.Equal(t, int64(3), envelope.Data.UsedCount)
	require.Len(t, envelope.Data.ActiveProxies, 2)
	require.Equal(t, 2, envelope.Data.CustomProxiesCount)
}

func TestUpdateCustomPoolCompatImportsAndDisablesThroughCanonicalService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &stubAdminService{proxies: []service.Proxy{
		{ID: 1, Protocol: "http", Host: "old.example", Port: 8080, Status: service.StatusActive},
	}}
	router := gin.New()
	router.PATCH("/config", NewProxyHandler(svc).UpdateCustomPoolCompat)
	body := `{"customProtocol":"socks5","customProxies":"new.example:1080:user:pass"}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, svc.createdProxies, 1)
	require.Equal(t, "socks5", svc.createdProxies[0].Protocol)
	require.Equal(t, "new.example", svc.createdProxies[0].Host)
	require.Equal(t, "user", svc.createdProxies[0].Username)
	require.Equal(t, "pass", svc.createdProxies[0].Password)
	require.Contains(t, svc.updatedProxyIDs, int64(1))
	require.Equal(t, service.StatusDisabled, svc.updatedProxies[0].Status)
}

func TestParseNeonixProxyRejectsUnsupportedScheme(t *testing.T) {
	_, _, _, _, _, err := parseNeonixProxy("file://localhost:10", "http")
	require.Error(t, err)
}

func TestUpdateCustomPoolEntryCompatChangesOnlyStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &stubAdminService{proxies: []service.Proxy{{
		ID: 7, Protocol: "http", Host: "proxy.example", Port: 8080,
		Username: "alice", Password: "secret", Status: service.StatusActive,
	}}}
	router := gin.New()
	router.PATCH("/pool/:id", NewProxyHandler(svc).UpdateCustomPoolEntryCompat)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/pool/7", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, svc.updatedProxies, 1)
	require.Equal(t, service.StatusDisabled, svc.updatedProxies[0].Status)
	require.Empty(t, svc.updatedProxies[0].Username)
	require.Empty(t, svc.updatedProxies[0].Password)
	require.NotContains(t, recorder.Body.String(), "alice")
	require.NotContains(t, recorder.Body.String(), "secret")
}

func TestUpdateCustomPoolEntryCompatRequiresBoolean(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.PATCH("/pool/:id", NewProxyHandler(&stubAdminService{}).UpdateCustomPoolEntryCompat)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/pool/1", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}
