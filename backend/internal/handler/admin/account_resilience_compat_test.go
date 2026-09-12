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

func TestResilienceCompatReturnsProviderCoverageAndSafeAccountReasons(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminService := newStubAdminService()
	adminService.accounts = []service.Account{
		{ID: 1, Name: "ready@example.com", Platform: service.PlatformAntigravity, Status: service.StatusActive, Schedulable: true, Concurrency: 2},
		{ID: 2, Name: "held@example.com", Platform: service.PlatformOpenAI, Status: service.StatusError, Schedulable: false, Concurrency: 1},
		{ID: 3, Name: "legacy@example.com", Platform: "historic", Status: service.StatusDisabled, Schedulable: false},
	}
	h := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/api/proxy/resilience", h.ResilienceCompat)
	req := httptest.NewRequest(http.MethodGet, "/api/proxy/resilience?page=1&pageSize=2&provider=", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	var envelope struct {
		Data struct {
			Coverage []struct {
				ID             string `json:"id"`
				TotalAccounts  int    `json:"totalAccounts"`
				UsableAccounts int    `json:"usableAccounts"`
				Status         string `json:"status"`
			} `json:"coverage"`
			Accounts []struct {
				Provider string   `json:"provider"`
				Eligible bool     `json:"eligible"`
				Reasons  []string `json:"reasons"`
			} `json:"accounts"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &envelope))
	var foundReady, foundHeld bool
	for _, item := range envelope.Data.Coverage {
		if item.ID == service.PlatformAntigravity {
			foundReady = item.TotalAccounts == 1 && item.UsableAccounts == 1 && item.Status == "ready"
		}
		if item.ID == service.PlatformOpenAI {
			foundHeld = item.TotalAccounts == 1 && item.UsableAccounts == 0 && item.Status == "unavailable"
		}
	}
	require.True(t, foundReady)
	require.True(t, foundHeld)
	require.NotEmpty(t, envelope.Data.Accounts)
	for _, account := range envelope.Data.Accounts {
		require.NotContains(t, strings.Join(account.Reasons, ","), "access_token")
	}
}
