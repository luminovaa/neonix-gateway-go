package routes

import (
	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/handler"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	"github.com/luminovaa/neonix-gateway-go/internal/provider"
	"github.com/luminovaa/neonix-gateway-go/internal/server/middleware"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

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
	api.Use(gin.HandlerFunc(adminAuth))
	api.Use(panelRateLimiter.Global())
	api.Use(gin.HandlerFunc(auditLog))
	api.Use(middleware.AdminComplianceGuard(settingService))

	api.GET("/accounts", h.Admin.Account.List)
	api.GET("/providers/summary", func(c *gin.Context) {
		response.Success(c, gin.H{"providers": provider.All()})
	})
}
