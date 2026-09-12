package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/handler"
	adminhandler "github.com/luminovaa/neonix-gateway-go/internal/handler/admin"
	servermiddleware "github.com/luminovaa/neonix-gateway-go/internal/server/middleware"
	"github.com/stretchr/testify/require"
)

func TestNeonixCompatibilityRoutesAreAdminOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer admin" {
			servermiddleware.AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Admin access required")
			return
		}
		c.Next()
	})
	passthroughAudit := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	RegisterNeonixCompatibilityRoutes(
		router,
		&handler.Handlers{Admin: &handler.AdminHandlers{Account: &adminhandler.AccountHandler{}, Group: adminhandler.NewGroupHandler(nil, nil, nil)}},
		adminAuth,
		passthroughAudit,
		nil,
		nil,
		nil,
	)

	for _, path := range []string{"/api/accounts", "/api/providers/summary", "/api/api-keys", "/api/api-keys/me", "/api/settings", "/api/proxy/models"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		require.Equal(t, http.StatusForbidden, resp.Code, path)
		require.Contains(t, resp.Body.String(), `"error"`, path)
	}
	for _, path := range []string{
		"/api/accounts/groups",
		"/api/accounts/codex/oauth/start",
		"/api/accounts/codex/oauth/poll",
		"/api/accounts/codex/oauth/cancel",
		"/api/accounts/grok/oauth/start",
		"/api/accounts/grok/oauth/poll",
		"/api/accounts/grok/oauth/cancel",
		"/api/accounts/m365/oauth/start",
		"/api/accounts/m365/oauth/complete",
		"/api/accounts/m365/oauth/cancel",
		"/api/mailbox/accounts",
		"/api/mailbox/poll",
		"/api/mailbox/oauth/start",
		"/api/mailbox/oauth/complete",
		"/api/mailbox/oauth/cancel",
		"/api/accounts/1/check",
		"/api/accounts/1/warmup",
		"/api/accounts/1/clear-error",
		"/api/accounts/1/recover-state",
		"/api/accounts/batch-refresh",
		"/api/accounts/batch-clear-error",
		"/api/accounts/batch-delete",
		"/api/accounts/bulk-update",
		"/api/proxy/resilience",
		"/api/proxy/resilience/model-locks/clear",
		"/api/proxy/status",
		"/api/proxy/stats",
		"/api/proxy/stats/reset",
		"/api/proxy/request-logs",
		"/api/proxy/logs",
		"/api/proxy/config",
		"/api/proxy/start",
		"/api/proxy/stop",
		"/api/proxy/models/sync/providers/oc",
		"/api/api-keys/1/usage",
		"/api/api-keys/1/access-stats",
		"/api/api-keys/regenerate",
		"/api/settings/theme",
	} {
		method := http.MethodPost
		if path == "/api/mailbox/accounts" || path == "/api/accounts/groups" || path == "/api/proxy/resilience" || path == "/api/proxy/status" || path == "/api/proxy/stats" || path == "/api/proxy/request-logs" || path == "/api/proxy/logs" || path == "/api/proxy/config" || path == "/api/api-keys/1/usage" || path == "/api/api-keys/1/access-stats" {
			method = http.MethodGet
		} else if path == "/api/settings/theme" {
			method = http.MethodPut
		}
		req := httptest.NewRequest(method, path, nil)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		require.Equal(t, http.StatusForbidden, resp.Code, path)
		require.Contains(t, resp.Body.String(), `"error"`, path)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/providers/summary", nil)
	req.Header.Set("Authorization", "Bearer admin")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	var payload struct {
		Providers []struct {
			ID string `json:"id"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &payload))
	found := false
	for _, item := range payload.Providers {
		if item.ID == "antigravity" {
			found = true
			break
		}
	}
	require.True(t, found)

	req = httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer admin")
	resp = httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	var accountsPayload struct {
		Accounts []any `json:"accounts"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &accountsPayload))
	require.Empty(t, accountsPayload.Accounts)
}

func TestNeonixProviderDetailUsesCanonicalRegistry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	passthroughAudit := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	RegisterNeonixCompatibilityRoutes(
		router,
		&handler.Handlers{Admin: &handler.AdminHandlers{Account: &adminhandler.AccountHandler{}}},
		adminAuth, passthroughAudit, nil, nil, nil,
	)
	req := httptest.NewRequest(http.MethodGet, "/api/providers/antigravity", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"id":"antigravity"`)
	require.NotContains(t, resp.Body.String(), "access_token")

	req = httptest.NewRequest(http.MethodGet, "/api/providers/does-not-exist", nil)
	resp = httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	require.Equal(t, http.StatusNotFound, resp.Code)
}

func TestNeonixAuthCompatibilitySeparatesPublicLoginFromOperatorSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
	})
	passthroughAudit := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	RegisterNeonixCompatibilityRoutes(
		router,
		&handler.Handlers{Admin: &handler.AdminHandlers{Account: &adminhandler.AccountHandler{}}},
		adminAuth, passthroughAudit, nil, nil, nil,
	)

	login := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"operator","password":"password"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(login, loginReq)
	// Login reaches the public compatibility handler (which has no services in
	// this wiring-only test) rather than being rejected by adminAuth.
	require.Equal(t, http.StatusInternalServerError, login.Code)

	me := httptest.NewRecorder()
	router.ServeHTTP(me, httptest.NewRequest(http.MethodGet, "/api/auth/me", nil))
	require.Equal(t, http.StatusUnauthorized, me.Code)
}
