package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountCompatCheckAndWarmupReturnSanitizedState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAccountHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, action := range []string{"check", "warmup"} {
		t.Run(action, func(t *testing.T) {
			router := gin.New()
			if action == "check" {
				router.POST("/api/accounts/:id/check", h.CheckCompat)
			} else {
				router.POST("/api/accounts/:id/warmup", h.WarmupCompat)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/accounts/41/"+action, nil)
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			require.Equal(t, http.StatusOK, resp.Code)
			require.NotContains(t, resp.Body.String(), "access_token")
		})
	}
}
