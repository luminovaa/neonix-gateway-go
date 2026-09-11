package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRegisterCommonRoutesExposesLivenessAndReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterCommonRoutes(router)

	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/health", body: `{"status":"ok"}`},
		{path: "/ready", body: `{"status":"ready"}`},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
		require.Equal(t, http.StatusOK, recorder.Code, test.path)
		require.JSONEq(t, test.body, recorder.Body.String(), test.path)
	}
}
