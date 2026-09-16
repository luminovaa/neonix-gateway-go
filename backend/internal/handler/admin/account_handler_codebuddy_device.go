package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddy"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/workbuddy"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

type codeBuddyDeviceStartRequest struct {
	Domain string `json:"domain"`
}

type codeBuddyDevicePollRequest struct {
	LoginID string `json:"loginId"`
	State   string `json:"state"`
}

func (r codeBuddyDevicePollRequest) sessionID() string {
	if value := strings.TrimSpace(r.LoginID); value != "" {
		return value
	}
	return strings.TrimSpace(r.State)
}

func (h *AccountHandler) StartCodeBuddyDeviceCompat(c *gin.Context) {
	h.startCodeBuddyDeviceCompat(c, "")
}

// StartWorkBuddyDeviceCompat fixes the realm at the server boundary so a UI
// request cannot turn a WorkBuddy login into a CodeBuddy login by supplying a
// different domain.
func (h *AccountHandler) StartWorkBuddyDeviceCompat(c *gin.Context) {
	h.startCodeBuddyDeviceCompat(c, "www.workbuddy.ai")
}

func (h *AccountHandler) startCodeBuddyDeviceCompat(c *gin.Context, forcedDomain string) {
	if h == nil || h.codeBuddyDeviceLogin == nil {
		response.InternalError(c, "CodeBuddy device login is not configured")
		return
	}
	var req codeBuddyDeviceStartRequest
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			response.BadRequest(c, "invalid CodeBuddy device login request")
			return
		}
	}
	domain := strings.TrimSpace(req.Domain)
	if forcedDomain != "" {
		domain = forcedDomain
	}
	result, err := h.codeBuddyDeviceLogin.Start(c.Request.Context(), domain)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *AccountHandler) PollCodeBuddyDeviceCompat(c *gin.Context) {
	if h == nil || h.codeBuddyDeviceLogin == nil || h.adminService == nil {
		response.InternalError(c, "CodeBuddy device login is not configured")
		return
	}
	var req codeBuddyDevicePollRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.sessionID() == "" {
		response.BadRequest(c, "invalid CodeBuddy device login request")
		return
	}
	loginID := req.sessionID()
	poll, err := h.codeBuddyDeviceLogin.Poll(c.Request.Context(), loginID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if poll.Pending {
		response.Success(c, gin.H{"status": "pending", "retryAfter": poll.RetryAfter, "expiresAt": poll.ExpiresAt})
		return
	}
	if poll.Result == nil || strings.TrimSpace(poll.Result.Tokens.AccessToken) == "" {
		response.Error(c, http.StatusBadGateway, "CodeBuddy login did not return token information")
		return
	}
	consumed := false
	defer func() {
		if !consumed {
			h.codeBuddyDeviceLogin.Release(loginID)
		}
	}()
	credentials := codeBuddyDeviceCredentials(poll.Result)
	platform := service.PlatformCodeBuddy
	if workbuddy.IsGlobalDomain(poll.Result.Tokens.Domain) {
		platform = service.PlatformWorkBuddy
	}
	accounts, err := h.adminService.ListAccountsForSchedulerScoreFilter(c.Request.Context(), platform, "", "", "", 0, "")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	existing := findCodeBuddyDeviceAccount(accounts, poll.Result)
	if existing != nil {
		merged := make(map[string]any, len(existing.Credentials)+len(credentials))
		for key, value := range existing.Credentials {
			merged[key] = value
		}
		for key, value := range credentials {
			merged[key] = value
		}
		if credentialMapString(merged, "refreshToken") == "" {
			for _, key := range []string{"refreshToken", "refresh_token"} {
				if oldRefresh := strings.TrimSpace(existing.GetCredential(key)); oldRefresh != "" {
					merged["refreshToken"] = oldRefresh
					break
				}
			}
		}
		credentials = merged
	}
	if credentialMapString(credentials, "refreshToken") == "" && credentialMapString(credentials, "refresh_token") == "" {
		response.Error(c, http.StatusBadRequest, "CodeBuddy did not return a refresh token; restart device login")
		return
	}
	var account *service.Account
	created := false
	if existing != nil {
		account, err = h.adminService.UpdateAccount(c.Request.Context(), existing.ID, &service.UpdateAccountInput{Type: service.AccountTypeOAuth, Credentials: credentials, Status: service.StatusActive})
	} else {
		name := strings.TrimSpace(poll.Result.AccountInfo.Nickname)
		if name == "" && strings.TrimSpace(poll.Result.AccountInfo.UserID) != "" {
			name = platform + ":" + strings.TrimSpace(poll.Result.AccountInfo.UserID)
		}
		if name == "" {
			name = "CodeBuddy account"
			if platform == service.PlatformWorkBuddy {
				name = "WorkBuddy account"
			}
		}
		account, err = h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
			Name: name, Platform: platform, Type: service.AccountTypeOAuth, Credentials: credentials,
			Extra: map[string]any{"source_provider": platform},
		})
		created = true
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.codeBuddyDeviceLogin.Consume(loginID)
	consumed = true
	response.Success(c, gin.H{"status": "complete", "created": created, "account": h.buildAccountResponseWithRuntime(c.Request.Context(), account)})
}

func (h *AccountHandler) CancelCodeBuddyDeviceCompat(c *gin.Context) {
	if h != nil && h.codeBuddyDeviceLogin != nil {
		var req codeBuddyDevicePollRequest
		if c.ShouldBindJSON(&req) == nil {
			h.codeBuddyDeviceLogin.Cancel(req.sessionID())
		}
	}
	response.Success(c, gin.H{"ok": true, "cancelled": true})
}

func codeBuddyDeviceCredentials(result *service.CodeBuddyDeviceTokenResult) map[string]any {
	tokens := result.Tokens
	credentials := map[string]any{
		"accessToken":   tokens.AccessToken,
		"expiresAt":     codebuddy.Expiry(time.Now(), tokens.ExpiresIn).UnixMilli(),
		"codebuddyAuth": codebuddy.CLIAuthMark,
	}
	if tokens.RefreshToken != "" {
		credentials["refreshToken"] = tokens.RefreshToken
	}
	if tokens.Domain != "" {
		credentials["domain"] = tokens.Domain
	}
	if result.AccountInfo.UserID != "" {
		credentials["userId"] = result.AccountInfo.UserID
	}
	if result.AccountInfo.EnterpriseID != "" {
		credentials["enterpriseId"] = result.AccountInfo.EnterpriseID
	}
	if result.AccountInfo.Nickname != "" {
		credentials["accountLabel"] = result.AccountInfo.Nickname
	}
	return credentials
}

func findCodeBuddyDeviceAccount(accounts []service.Account, result *service.CodeBuddyDeviceTokenResult) *service.Account {
	uid := strings.TrimSpace(result.AccountInfo.UserID)
	for index := range accounts {
		candidate := &accounts[index]
		if uid != "" && (strings.EqualFold(uid, strings.TrimSpace(candidate.GetCredential("userId"))) || strings.EqualFold(uid, strings.TrimSpace(candidate.GetCredential("user_id")))) {
			return candidate
		}
	}
	enterpriseID := strings.TrimSpace(result.AccountInfo.EnterpriseID)
	label := strings.TrimSpace(result.AccountInfo.Nickname)
	domain := strings.TrimSpace(result.Tokens.Domain)
	if enterpriseID == "" || label == "" {
		return nil
	}
	for index := range accounts {
		candidate := &accounts[index]
		if strings.EqualFold(enterpriseID, strings.TrimSpace(candidate.GetCredential("enterpriseId"))) &&
			strings.EqualFold(label, strings.TrimSpace(candidate.GetCredential("accountLabel"))) &&
			strings.EqualFold(domain, strings.TrimSpace(candidate.GetCredential("domain"))) {
			return candidate
		}
	}
	return nil
}
