package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/handler"
	servermiddleware "github.com/luminovaa/neonix-gateway-go/internal/server/middleware"
	"github.com/stretchr/testify/require"
)

func TestProviderSummaryIsAdminOnlyAndUsesCanonicalCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		switch c.GetHeader("Authorization") {
		case "":
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
		case "Bearer admin":
			c.Next()
		default:
			servermiddleware.AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Admin access required")
		}
	})
	passthroughAudit := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	passthroughStepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{}}, adminAuth, passthroughAudit, passthroughStepUp, nil, nil)

	for _, tc := range []struct {
		name       string
		auth       string
		wantStatus int
	}{
		{name: "anonymous", wantStatus: http.StatusUnauthorized},
		{name: "member", auth: "Bearer member", wantStatus: http.StatusForbidden},
		{name: "admin", auth: "Bearer admin", wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/providers/summary", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			require.Equal(t, tc.wantStatus, resp.Code)
			if tc.wantStatus != http.StatusOK {
				return
			}
			var payload struct {
				Data struct {
					Providers []struct {
						ID       string `json:"id"`
						Category string `json:"category"`
					} `json:"providers"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &payload))
			require.NotEmpty(t, payload.Data.Providers)
			foundAntigravity := false
			for _, item := range payload.Data.Providers {
				if item.ID == "antigravity" && item.Category == "provider" {
					foundAntigravity = true
				}
			}
			require.True(t, foundAntigravity)
		})
	}
}
