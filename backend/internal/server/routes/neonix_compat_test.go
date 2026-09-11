package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		&handler.Handlers{Admin: &handler.AdminHandlers{Account: &adminhandler.AccountHandler{}}},
		adminAuth,
		passthroughAudit,
		nil,
		nil,
	)

	for _, path := range []string{"/api/accounts", "/api/providers/summary"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
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
