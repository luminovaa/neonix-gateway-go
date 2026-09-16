package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	middleware2 "github.com/luminovaa/neonix-gateway-go/internal/server/middleware"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

type neonixAuthUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
	LastLoginAt *int64 `json:"lastLoginAt,omitempty"`
}

func neonixAuthUserFromService(user *service.User) *neonixAuthUser {
	if user == nil {
		return nil
	}
	result := &neonixAuthUser{
		ID:          strconv.FormatInt(user.ID, 10),
		Username:    user.Username,
		Email:       user.Email,
		DisplayName: user.Username,
		Role:        service.RoleAdmin,
		Status:      user.Status,
		CreatedAt:   user.CreatedAt.UnixMilli(),
		UpdatedAt:   user.UpdatedAt.UnixMilli(),
	}
	if user.LastLoginAt != nil {
		value := user.LastLoginAt.UnixMilli()
		result.LastLoginAt = &value
	}
	return result
}

func (h *AuthHandler) compatServices(c *gin.Context) bool {
	if h == nil || h.authService == nil || h.userService == nil {
		response.ErrorWithDetails(c, http.StatusInternalServerError, "Authentication service is not configured", "AUTH_SERVICE_UNAVAILABLE", nil)
		return false
	}
	return true
}

// LoginCompat implements POST /api/auth/login using the Neonix username
// payload while reusing the Go password verification and JWT issuer.
func (h *AuthHandler) LoginCompat(c *gin.Context) {
	if !h.compatServices(c) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Username) == "" || req.Password == "" {
		response.ErrorWithDetails(c, http.StatusBadRequest, "Username and password are required", "INVALID_REQUEST", nil)
		return
	}
	identifier := strings.TrimSpace(req.Username)
	email := identifier
	if !strings.Contains(identifier, "@") {
		user, err := h.userService.GetByUsername(c.Request.Context(), identifier)
		if err != nil || user == nil {
			response.ErrorWithDetails(c, http.StatusUnauthorized, "Invalid username or password", "INVALID_CREDENTIALS", nil)
			return
		}
		email = user.Email
	}
	token, user, err := h.authService.Login(c.Request.Context(), email, req.Password)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.authService.RecordSuccessfulLogin(c.Request.Context(), user.ID)
	response.Success(c, gin.H{"token": token, "user": neonixAuthUserFromService(user)})
}

// MeCompat implements GET /api/auth/me. The route is admin guarded because
// Neonix is deployed as a single-operator control plane.
func (h *AuthHandler) MeCompat(c *gin.Context) {
	if !h.compatServices(c) {
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "Authentication required")
		return
	}
	user, err := h.userService.GetByID(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"user": neonixAuthUserFromService(user)})
}

func (h *AuthHandler) LogoutCompat(c *gin.Context) {
	if !h.compatServices(c) {
		return
	}
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	_ = c.ShouldBindJSON(&req)
	if strings.TrimSpace(req.RefreshToken) != "" {
		_ = h.authService.RevokeRefreshToken(c.Request.Context(), req.RefreshToken)
	}
	response.Success(c, gin.H{"ok": true})
}

// RefreshCompat accepts the legacy Authorization bearer access token and
// allows the same bounded 24-hour grace window as the Node compatibility
// backend. The new token is still issued by the Go JWT service.
func (h *AuthHandler) RefreshCompat(c *gin.Context) {
	if !h.compatServices(c) {
		return
	}
	header := strings.TrimSpace(c.GetHeader("Authorization"))
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		response.ErrorWithDetails(c, http.StatusUnauthorized, "Token required", "TOKEN_REQUIRED", nil)
		return
	}
	claims, err := h.authService.ValidateToken(strings.TrimSpace(parts[1]))
	if err != nil && !errors.Is(err, service.ErrTokenExpired) {
		response.ErrorWithDetails(c, http.StatusUnauthorized, "Token is invalid", "TOKEN_INVALID", nil)
		return
	}
	if claims == nil || claims.IssuedAt == nil || time.Since(claims.IssuedAt.Time) > 24*time.Hour {
		response.ErrorWithDetails(c, http.StatusUnauthorized, "Token is too old or invalid", "TOKEN_INVALID", nil)
		return
	}
	user, err := h.userService.GetByID(c.Request.Context(), claims.UserID)
	if err != nil || user == nil || !user.IsActive() {
		response.ErrorWithDetails(c, http.StatusUnauthorized, "User not found or inactive", "INVALID_USER", nil)
		return
	}
	if claims.TokenVersion != user.TokenVersion {
		response.ErrorWithDetails(c, http.StatusUnauthorized, "Token has been revoked", "TOKEN_REVOKED", nil)
		return
	}
	token, err := h.authService.GenerateToken(c.Request.Context(), user)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"token": token, "user": neonixAuthUserFromService(user)})
}
