package routes

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/handler"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	"github.com/luminovaa/neonix-gateway-go/internal/provider"
	"github.com/luminovaa/neonix-gateway-go/internal/server/middleware"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

// compatibilityResponseWriter buffers the standard Go response envelope so
// the legacy Neonix UI can keep its direct JSON contract during cutover. The
// upstream Sub2API admin routes continue to use the envelope on /api/v1.
type compatibilityResponseWriter struct {
	gin.ResponseWriter
	body   bytes.Buffer
	status int
	wrote  bool
}

func (w *compatibilityResponseWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.status = code
	w.wrote = true
}

func (w *compatibilityResponseWriter) WriteHeaderNow() {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
}

func (w *compatibilityResponseWriter) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	return w.body.Write(data)
}

func (w *compatibilityResponseWriter) WriteString(value string) (int, error) {
	w.WriteHeaderNow()
	return w.body.WriteString(value)
}

func (w *compatibilityResponseWriter) Status() int {
	if !w.wrote {
		return http.StatusOK
	}
	return w.status
}

func (w *compatibilityResponseWriter) Size() int { return w.body.Len() }

func (w *compatibilityResponseWriter) Written() bool { return w.wrote }

func flattenCompatibilityResponse() gin.HandlerFunc {
	return func(c *gin.Context) {
		original := c.Writer
		buffer := &compatibilityResponseWriter{ResponseWriter: original}
		c.Writer = buffer
		c.Next()
		c.Writer = original

		status := buffer.Status()
		if buffer.body.Len() == 0 {
			original.WriteHeader(status)
			return
		}

		var envelope struct {
			Code     int               `json:"code"`
			Message  string            `json:"message"`
			Reason   string            `json:"reason"`
			Metadata map[string]string `json:"metadata"`
			Data     json.RawMessage   `json:"data"`
		}
		if err := json.Unmarshal(buffer.body.Bytes(), &envelope); err != nil || envelope.Code == 0 && envelope.Data == nil {
			original.WriteHeader(status)
			_, _ = original.Write(buffer.body.Bytes())
			return
		}

		original.Header().Set("Content-Type", "application/json; charset=utf-8")
		original.WriteHeader(status)
		if envelope.Code >= http.StatusBadRequest {
			payload := gin.H{"error": envelope.Message}
			if envelope.Reason != "" {
				payload["errorCode"] = envelope.Reason
			}
			if len(envelope.Metadata) > 0 {
				payload["errorParams"] = envelope.Metadata
			}
			encoded, _ := json.Marshal(payload)
			_, _ = original.Write(encoded)
			return
		}
		_, _ = original.Write(envelope.Data)
	}
}

// RegisterNeonixCompatibilityRoutes exposes the small operator-facing surface
// consumed by the Neonix Next.js UI while the remaining /api/v1 routes are
// migrated. It intentionally reuses the admin guard and never exposes tokens.
func RegisterNeonixCompatibilityRoutes(
	r *gin.Engine,
	h *handler.Handlers,
	adminAuth middleware.AdminAuthMiddleware,
	auditLog middleware.AuditLogMiddleware,
	settingService *service.SettingService,
	panelRateLimiter *middleware.PanelRateLimiter,
) {
	api := r.Group("/api")
	// Install first so auth/compliance failures are flattened too; otherwise
	// an early middleware abort would still leak the Go envelope to the
	// direct-JSON Neonix transport.
	api.Use(flattenCompatibilityResponse())
	api.Use(gin.HandlerFunc(adminAuth))
	api.Use(panelRateLimiter.Global())
	api.Use(gin.HandlerFunc(auditLog))
	api.Use(middleware.AdminComplianceGuard(settingService))

	accounts := api.Group("/accounts")
	accounts.GET("", h.Admin.Account.List)
	accounts.POST("/antigravity/oauth/start", h.Admin.Account.StartAntigravityOAuthCompat)
	accounts.POST("/antigravity/oauth/complete", h.Admin.Account.CompleteAntigravityOAuthCompat)
	accounts.POST("/antigravity/oauth/cancel", h.Admin.Account.CancelAntigravityOAuthCompat)
	accounts.GET("/:id", h.Admin.Account.GetByID)
	accounts.POST("", h.Admin.Account.Create)
	accounts.POST("/batch", h.Admin.Account.BatchCreate)
	accounts.PUT("/:id", h.Admin.Account.Update)
	accounts.PATCH("/:id/enabled", h.Admin.Account.UpdateEnabled)
	accounts.DELETE("/:id", h.Admin.Account.Delete)
	accounts.POST("/:id/refresh", h.Admin.Account.Refresh)

	api.GET("/providers", func(c *gin.Context) {
		response.Success(c, gin.H{"providers": provider.All()})
	})
	api.GET("/providers/summary", func(c *gin.Context) {
		if h != nil && h.Admin != nil && h.Admin.Account != nil {
			h.Admin.Account.ProviderSummary(c)
			return
		}
		response.Success(c, gin.H{"providers": provider.BuildSummary(nil)})
	})
}
