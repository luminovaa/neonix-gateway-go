package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddy"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/workbuddy"
	"golang.org/x/sync/singleflight"
)

const (
	codeBuddyRefreshSkew     = 5 * time.Minute
	codeBuddyResponseBodyMax = 1 << 20
)

type CodeBuddyTokenError struct {
	Status    int
	Transient bool
	Revoked   bool
	Cause     error
}

func (e *CodeBuddyTokenError) Error() string {
	if e == nil {
		return "CodeBuddy token refresh failed"
	}
	if e.Revoked {
		return "CodeBuddy session expired; sign in again"
	}
	if e.Transient {
		return "CodeBuddy token service is temporarily unavailable"
	}
	return "CodeBuddy token refresh was rejected"
}

func (e *CodeBuddyTokenError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type codeBuddyTokenCacheEntry struct {
	accessToken         string
	expiresAt           time.Time
	credentialSignature [32]byte
}

type CodeBuddyTokenProvider struct {
	accountRepo  AccountRepository
	httpUpstream HTTPUpstream
	now          func() time.Time
	group        singleflight.Group
	mu           sync.Mutex
	cache        map[int64]codeBuddyTokenCacheEntry
}

func NewCodeBuddyTokenProvider(repo AccountRepository, upstream HTTPUpstream) *CodeBuddyTokenProvider {
	return &CodeBuddyTokenProvider{accountRepo: repo, httpUpstream: upstream, now: time.Now, cache: make(map[int64]codeBuddyTokenCacheEntry)}
}

func (p *CodeBuddyTokenProvider) AccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil || !isCodeBuddyOAuthPlatform(account.Platform) {
		return "", errors.New("CodeBuddy or WorkBuddy account is required")
	}
	identity := codeBuddyIdentity(account)
	if account.Platform == PlatformWorkBuddy && !workbuddy.IsGlobalDomain(identity.Domain) {
		return "", errors.New("WorkBuddy credentials must use the workbuddy.ai realm")
	}
	if codebuddy.IsChinaRealm(identity.Domain) {
		return "", errors.New("CodeBuddy China credentials require the codebuddy-china provider")
	}
	if !identity.IsCLI() {
		return codeBuddyLegacyToken(account), nil
	}
	credentialSignature := codeBuddyCredentialSignature(account)
	p.mu.Lock()
	cached, cachedOK := p.cache[account.ID]
	p.mu.Unlock()
	if cachedOK && cached.credentialSignature == credentialSignature && cached.accessToken != "" && p.now().Add(codeBuddyRefreshSkew).Before(cached.expiresAt) {
		return cached.accessToken, nil
	}
	expiresAt := codeBuddyCredentialExpiry(account)
	if identity.AccessToken != "" && ((expiresAt != nil && p.now().Add(codeBuddyRefreshSkew).Before(*expiresAt)) || (expiresAt == nil && identity.RefreshToken == "")) {
		return identity.AccessToken, nil
	}
	if identity.RefreshToken == "" {
		return "", &CodeBuddyTokenError{Status: http.StatusUnauthorized, Revoked: true, Cause: errors.New("refresh token is missing")}
	}
	value, err, _ := p.group.Do(codeBuddyRefreshFlightKey(account), func() (any, error) {
		return p.refresh(ctx, account)
	})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}

func (p *CodeBuddyTokenProvider) RefreshCredentials(ctx context.Context, account *Account) (map[string]any, error) {
	if account == nil || !isCodeBuddyOAuthPlatform(account.Platform) || !codeBuddyIdentity(account).IsCLI() {
		return nil, errors.New("refreshable CodeBuddy or WorkBuddy CLI account is required")
	}
	if account.Platform == PlatformWorkBuddy && !workbuddy.IsGlobalDomain(codeBuddyIdentity(account).Domain) {
		return nil, errors.New("WorkBuddy credentials must use the workbuddy.ai realm")
	}
	if codebuddy.IsChinaRealm(codeBuddyIdentity(account).Domain) {
		return nil, errors.New("CodeBuddy China credentials require the codebuddy-china provider")
	}
	_, err, _ := p.group.Do(codeBuddyRefreshFlightKey(account), func() (any, error) { return p.refresh(ctx, account) })
	if err != nil {
		return nil, err
	}
	return shallowCopyMap(account.Credentials), nil
}

func isCodeBuddyOAuthPlatform(platform string) bool {
	return platform == PlatformCodeBuddy || platform == PlatformWorkBuddy
}

func (p *CodeBuddyTokenProvider) refresh(ctx context.Context, account *Account) (string, error) {
	identity := codeBuddyIdentity(account)
	if identity.RefreshToken == "" {
		return "", &CodeBuddyTokenError{Status: http.StatusUnauthorized, Revoked: true}
	}
	hosts := codebuddy.ResolveHosts(identity.Domain)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codebuddy.RefreshURL(hosts), bytes.NewReader(nil))
	if err != nil {
		return "", err
	}
	req.Header = codebuddy.CommonHeaders(hosts)
	req.Header.Set("X-Refresh-Token", identity.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
	if identity.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", identity.EnterpriseID)
	}
	resp, err := p.do(account, req)
	if err != nil {
		return "", &CodeBuddyTokenError{Status: http.StatusBadGateway, Transient: true, Cause: err}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, codeBuddyResponseBodyMax+1))
	if readErr != nil {
		return "", &CodeBuddyTokenError{Status: http.StatusBadGateway, Transient: true, Cause: readErr}
	}
	if len(body) > codeBuddyResponseBodyMax {
		return "", &CodeBuddyTokenError{Status: http.StatusBadGateway, Transient: true, Cause: errors.New("CodeBuddy token response exceeded the allowed size")}
	}
	envelope, envelopeErr := codebuddy.ParseEnvelope(body)
	if envelopeErr != nil {
		revoked := envelope.Code == 12153 || strings.Contains(strings.ToLower(envelope.Msg), "offline user session not found")
		transient := !revoked && (resp.StatusCode == 0 || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500)
		return "", &CodeBuddyTokenError{Status: resp.StatusCode, Transient: transient, Revoked: revoked, Cause: envelopeErr}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &CodeBuddyTokenError{Status: resp.StatusCode, Transient: resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500}
	}
	tokens, err := codebuddy.ParseTokens(envelope.Data)
	if err != nil {
		return "", &CodeBuddyTokenError{Status: http.StatusBadGateway, Transient: true, Cause: err}
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = identity.RefreshToken
	}
	if tokens.Domain == "" {
		tokens.Domain = identity.Domain
	}
	expiresAt := codebuddy.Expiry(p.now(), tokens.ExpiresIn)
	credentials := shallowCopyMap(account.Credentials)
	setCodeBuddyCredential(credentials, account.Credentials, "accessToken", "access_token", tokens.AccessToken)
	setCodeBuddyCredential(credentials, account.Credentials, "refreshToken", "refresh_token", tokens.RefreshToken)
	setCodeBuddyCredential(credentials, account.Credentials, "expiresAt", "expires_at", expiresAt.UnixMilli())
	if tokens.Domain != "" {
		credentials["domain"] = tokens.Domain
	}
	credentials["codebuddyAuth"] = codebuddy.CLIAuthMark
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := persistAccountCredentials(persistCtx, p.accountRepo, account, credentials); err != nil {
		return "", fmt.Errorf("persist CodeBuddy token: %w", err)
	}
	if p.accountRepo == nil {
		account.Credentials = credentials
	}
	p.mu.Lock()
	account.Credentials = credentials
	p.cache[account.ID] = codeBuddyTokenCacheEntry{accessToken: tokens.AccessToken, expiresAt: expiresAt, credentialSignature: codeBuddyCredentialSignature(account)}
	p.mu.Unlock()
	return tokens.AccessToken, nil
}

func (p *CodeBuddyTokenProvider) do(account *Account, req *http.Request) (*http.Response, error) {
	if p == nil || p.httpUpstream == nil {
		return nil, errors.New("CodeBuddy HTTP upstream is not configured")
	}
	proxyURL := ""
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	accountID, concurrency := int64(0), 1
	if account != nil {
		accountID, concurrency = account.ID, account.Concurrency
	}
	return p.httpUpstream.Do(req, proxyURL, accountID, concurrency)
}

func codeBuddyIdentity(account *Account) codebuddy.Identity {
	return codebuddy.Identity{
		AccessToken: codeBuddyCredential(account, "accessToken", "access_token"), RefreshToken: codeBuddyCredential(account, "refreshToken", "refresh_token"),
		UserID: codeBuddyCredential(account, "userId", "user_id"), EnterpriseID: codeBuddyCredential(account, "enterpriseId", "enterprise_id"),
		Domain: codeBuddyCredential(account, "domain"), AuthMark: codeBuddyCredential(account, "codebuddyAuth", "codebuddy_auth"),
	}
}

func codeBuddyLegacyToken(account *Account) string {
	return codeBuddyCredential(account, "token", "apiKey", "api_key", "authToken", "auth_token", "accessToken", "access_token", "sessionToken", "session_token")
}

func codeBuddyCredential(account *Account, keys ...string) string {
	if account == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(account.GetCredential(key)); value != "" {
			return value
		}
	}
	return ""
}

func setCodeBuddyCredential(dst, original map[string]any, camel, snake string, value any) {
	key := camel
	if _, exists := original[snake]; exists {
		key = snake
	}
	dst[key] = value
}

func codeBuddyCredentialExpiry(account *Account) *time.Time {
	for _, key := range []string{"expiresAt", "expires_at"} {
		raw := strings.TrimSpace(account.GetCredential(key))
		if raw == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			return &parsed
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			continue
		}
		var parsed time.Time
		if value >= 1_000_000_000_000 {
			parsed = time.UnixMilli(value)
		} else {
			parsed = time.Unix(value, 0)
		}
		return &parsed
	}
	return nil
}

func codeBuddyCredentialSignature(account *Account) [32]byte {
	identity := codeBuddyIdentity(account)
	return sha256.Sum256([]byte(strings.Join([]string{identity.AccessToken, identity.RefreshToken, identity.UserID, identity.EnterpriseID, identity.Domain, identity.AuthMark}, "\x00")))
}

func codeBuddyRefreshFlightKey(account *Account) string {
	if account == nil {
		return "0"
	}
	signature := codeBuddyCredentialSignature(account)
	return fmt.Sprintf("%d:%x", account.ID, signature[:8])
}
