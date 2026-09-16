package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

type CodeBuddyTokenRefresher struct {
	provider *CodeBuddyTokenProvider
}

func NewCodeBuddyTokenRefresher(provider *CodeBuddyTokenProvider) *CodeBuddyTokenRefresher {
	return &CodeBuddyTokenRefresher{provider: provider}
}

func (r *CodeBuddyTokenRefresher) CacheKey(account *Account) string {
	if account == nil {
		return "codebuddy:token:0"
	}
	identity := strings.TrimSpace(account.GetCredential("userId"))
	if identity == "" {
		identity = strings.TrimSpace(account.GetCredential("user_id"))
	}
	if identity == "" {
		identity = strconv.FormatInt(account.ID, 10)
	}
	return "codebuddy:token:" + identity
}

func (r *CodeBuddyTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && isCodeBuddyOAuthPlatform(account.Platform) && codeBuddyIdentity(account).IsCLI() && codeBuddyIdentity(account).RefreshToken != ""
}

func (r *CodeBuddyTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	if refreshWindow < codeBuddyRefreshSkew {
		refreshWindow = codeBuddyRefreshSkew
	}
	expiresAt := codeBuddyCredentialExpiry(account)
	return expiresAt == nil || time.Until(*expiresAt) < refreshWindow
}

func (r *CodeBuddyTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if r == nil || r.provider == nil {
		return nil, errors.New("CodeBuddy token provider is not configured")
	}
	return r.provider.RefreshCredentials(ctx, account)
}
