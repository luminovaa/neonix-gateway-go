package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/handler"
	servermiddleware "github.com/luminovaa/neonix-gateway-go/internal/server/middleware"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newAuthRoutesTestRouter(redisClient *redis.Client) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	v1 := router.Group("/api/v1")

	RegisterAuthRoutes(
		v1,
		&handler.Handlers{
			Auth:    &handler.AuthHandler{},
			Setting: &handler.SettingHandler{},
		},
		servermiddleware.JWTAuthMiddleware(func(c *gin.Context) {
			c.Next()
		}),
		servermiddleware.AuditLogMiddleware(func(c *gin.Context) {
			c.Next()
		}),
		redisClient,
		nil,
		nil,
	)

	return router
}

func TestAuthRoutesRateLimitFailCloseWhenRedisUnavailable(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() {
		_ = rdb.Close()
	})

	router := newAuthRoutesTestRouter(rdb)
	paths := []string{
		"/api/v1/auth/login",
		"/api/v1/auth/login/2fa",
		"/api/v1/auth/send-verify-code",
		"/api/v1/auth/oauth/pending/send-verify-code",
	}

	for _, path := range paths {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.10:12345"

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusTooManyRequests, w.Code, "path=%s", path)
		require.Contains(t, w.Body.String(), "rate limit exceeded", "path=%s", path)
	}
}

func TestAuthRoutesDoNotExposePublicRegistration(t *testing.T) {
	router := newAuthRoutesTestRouter(nil)
	for _, path := range []string{
		"/api/v1/auth/register",
		"/api/v1/auth/validate-promo-code",
		"/api/v1/auth/validate-invitation-code",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code, "path=%s", path)
	}
}
