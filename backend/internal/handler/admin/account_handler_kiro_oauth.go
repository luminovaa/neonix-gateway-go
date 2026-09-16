package admin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/kiro"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

type kiroOAuthCompatRequest struct {
	LoginID     string `json:"loginId" binding:"required"`
	CallbackURL string `json:"callbackUrl" binding:"required"`
}

// StartKiroOAuthCompat creates the Kiro PKCE session and dynamic public OIDC
// client used by the official Kiro browser sign-in page. The operator chooses
// Google in that page; Neonix never receives the Google password.
func (h *AccountHandler) StartKiroOAuthCompat(c *gin.Context) {
	if h == nil || h.kiroOAuthService == nil {
		response.InternalError(c, "Kiro OAuth is not configured")
		return
	}
	result, err := h.kiroOAuthService.Start(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// CompleteKiroOAuthCompat consumes either stage of Kiro's two-redirect flow.
// The first callback follows identity-provider sign-in and returns the AWS OIDC
// authorization URL. The second exchanges the authorization code and upserts
// the canonical Kiro account.
func (h *AccountHandler) CompleteKiroOAuthCompat(c *gin.Context) {
	if h == nil || h.kiroOAuthService == nil || h.adminService == nil {
		response.InternalError(c, "Kiro OAuth is not configured")
		return
	}
	var req kiroOAuthCompatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid Kiro OAuth request")
		return
	}
	result, err := h.kiroOAuthService.Complete(c.Request.Context(), strings.TrimSpace(req.LoginID), strings.TrimSpace(req.CallbackURL))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if result.Status == "authorize" {
		response.Success(c, gin.H{"status": "authorize", "authorizationUrl": result.AuthorizationURL})
		return
	}
	tokenInfo := result.TokenInfo
	if tokenInfo == nil {
		response.InternalError(c, "Kiro OAuth returned no credentials")
		return
	}
	credentials := map[string]any{
		"access_token":  tokenInfo.AccessToken,
		"refresh_token": tokenInfo.RefreshToken,
		"client_id":     tokenInfo.ClientID,
		"client_secret": tokenInfo.ClientSecret,
		"expires_at":    tokenInfo.ExpiresAt.Unix(),
		"auth_method":   "IdC",
		"provider":      "Google",
		"region":        "us-east-1",
		"profile_arn":   kiro.SocialProfileARN,
	}
	if tokenInfo.IDToken != "" {
		credentials["id_token"] = tokenInfo.IDToken
	}
	if tokenInfo.Subject != "" {
		credentials["subject"] = tokenInfo.Subject
	}
	if tokenInfo.Email != "" {
		credentials["email"] = tokenInfo.Email
	}

	accounts, err := h.adminService.ListAccountsForSchedulerScoreFilter(c.Request.Context(), service.PlatformKiro, "", "", "", 0, "")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	var existing *service.Account
	for index := range accounts {
		candidate := &accounts[index]
		if tokenInfo.Subject != "" && strings.EqualFold(strings.TrimSpace(candidate.GetCredential("subject")), tokenInfo.Subject) {
			existing = candidate
			break
		}
		if existing == nil && tokenInfo.Email != "" && strings.EqualFold(strings.TrimSpace(candidate.GetCredential("email")), tokenInfo.Email) {
			existing = candidate
		}
	}
	var account *service.Account
	created := false
	if existing != nil {
		account, err = h.adminService.UpdateAccount(c.Request.Context(), existing.ID, &service.UpdateAccountInput{Type: service.AccountTypeOAuth, Credentials: credentials})
	} else {
		name := neonixFirstNonEmpty(tokenInfo.Email, tokenInfo.Name, tokenInfo.Subject, "Kiro Google account")
		account, err = h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
			Name: name, Platform: service.PlatformKiro, Type: service.AccountTypeOAuth,
			Credentials: credentials, Extra: map[string]any{"source_provider": "kiro"},
		})
		created = true
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.kiroOAuthService.Consume(req.LoginID)
	response.Success(c, gin.H{"status": "complete", "created": created, "account": neonixAccountView(account)})
}

func (h *AccountHandler) CancelKiroOAuthCompat(c *gin.Context) {
	if h != nil && h.kiroOAuthService != nil {
		var req struct {
			LoginID string `json:"loginId"`
		}
		if c.ShouldBindJSON(&req) == nil {
			h.kiroOAuthService.Cancel(req.LoginID)
		}
	}
	response.Success(c, gin.H{"ok": true, "cancelled": true})
}
