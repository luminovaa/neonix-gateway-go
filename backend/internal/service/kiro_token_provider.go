package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	kiroSocialRefreshURL = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	kiroRefreshSkew      = 2 * time.Minute
)

var kiroRegionPattern = regexp.MustCompile(`^[a-z]{2}(?:-gov)?-[a-z]+-\d$`)

type KiroTokenProvider struct {
	accountRepo  AccountRepository
	httpUpstream HTTPUpstream
	refreshGroup singleflight.Group
	now          func() time.Time
}

func NewKiroTokenProvider(accountRepo AccountRepository, httpUpstream HTTPUpstream) *KiroTokenProvider {
	return &KiroTokenProvider{accountRepo: accountRepo, httpUpstream: httpUpstream, now: time.Now}
}

func (p *KiroTokenProvider) Token(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("kiro account is required")
	}
	token := kiroCredential(account, "accessToken", "access_token", "token")
	if token == "" {
		return p.Refresh(ctx, account)
	}
	expiresAt := kiroCredentialExpiry(account)
	if expiresAt.IsZero() || p.now().Add(kiroRefreshSkew).Before(expiresAt) {
		return token, nil
	}
	if kiroCredential(account, "refreshToken", "refresh_token") == "" {
		return "", errors.New("kiro access token expired and no refresh token is available")
	}
	return p.Refresh(ctx, account)
}

func (p *KiroTokenProvider) Refresh(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("kiro account is required")
	}
	result, err, _ := p.refreshGroup.Do(strconv.FormatInt(account.ID, 10), func() (any, error) {
		return p.refresh(ctx, account)
	})
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

func (p *KiroTokenProvider) refresh(ctx context.Context, account *Account) (string, error) {
	refreshToken := kiroCredential(account, "refreshToken", "refresh_token")
	if refreshToken == "" {
		return "", errors.New("kiro refresh token is missing")
	}
	authMethod := strings.ToLower(kiroCredential(account, "authMethod", "auth_method"))
	var spec kiroRefreshSpec
	if authMethod == "social" {
		spec = kiroRefreshSpec{url: kiroSocialRefreshURL, payload: map[string]any{"refreshToken": refreshToken}}
	} else {
		clientID := kiroCredential(account, "clientId", "client_id")
		clientSecret := kiroCredential(account, "clientSecret", "client_secret")
		if clientID == "" || clientSecret == "" {
			return "", errors.New("kiro OIDC client credentials are missing")
		}
		region := strings.ToLower(kiroCredential(account, "region"))
		if region == "" {
			region = "us-east-1"
		}
		if !kiroRegionPattern.MatchString(region) {
			return "", errors.New("kiro region is invalid")
		}
		spec = kiroRefreshSpec{url: "https://oidc." + region + ".amazonaws.com/token", payload: map[string]any{"clientId": clientID, "clientSecret": clientSecret, "refreshToken": refreshToken, "grantType": "refresh_token"}}
	}
	tokens, err := p.exchange(ctx, account, spec)
	if err != nil && authMethod == "social" {
		clientID := kiroCredential(account, "clientId", "client_id")
		clientSecret := kiroCredential(account, "clientSecret", "client_secret")
		region := strings.ToLower(kiroCredential(account, "region"))
		if region == "" {
			region = "us-east-1"
		}
		if clientID != "" && clientSecret != "" && kiroRegionPattern.MatchString(region) {
			tokens, err = p.exchange(ctx, account, kiroRefreshSpec{url: "https://oidc." + region + ".amazonaws.com/token", payload: map[string]any{"clientId": clientID, "clientSecret": clientSecret, "refreshToken": refreshToken, "grantType": "refresh_token"}})
		}
	}
	if err != nil {
		return "", err
	}
	if tokens.AccessToken == "" {
		return "", errors.New("kiro refresh response did not contain an access token")
	}
	originalCredentials := account.Credentials
	credentials := shallowCopyMap(originalCredentials)
	setKiroCredential(credentials, account.Credentials, "accessToken", "access_token", tokens.AccessToken)
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	setKiroCredential(credentials, account.Credentials, "refreshToken", "refresh_token", tokens.RefreshToken)
	if tokens.ExpiresIn <= 0 {
		tokens.ExpiresIn = 3600
	}
	expiresAt := p.now().Add(time.Duration(tokens.ExpiresIn) * time.Second).UnixMilli()
	setKiroCredential(credentials, account.Credentials, "expiresAt", "expires_at", expiresAt)
	if err := persistAccountCredentials(ctx, p.accountRepo, account, credentials); err != nil {
		return "", fmt.Errorf("persist refreshed Kiro token: %w", err)
	}
	if p.accountRepo == nil {
		account.Credentials = credentials
	}
	return tokens.AccessToken, nil
}

type kiroRefreshSpec struct {
	url     string
	payload map[string]any
}
type kiroRefreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int    `json:"expiresIn"`
}

func (p *KiroTokenProvider) exchange(ctx context.Context, account *Account, spec kiroRefreshSpec) (kiroRefreshResponse, error) {
	if p.httpUpstream == nil {
		return kiroRefreshResponse{}, errors.New("kiro HTTP upstream is not configured")
	}
	body, err := json.Marshal(spec.payload)
	if err != nil {
		return kiroRefreshResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.url, bytes.NewReader(body))
	if err != nil {
		return kiroRefreshResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", kiroUserAgent(stableKiroMachineID(account)))
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := p.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return kiroRefreshResponse{}, fmt.Errorf("kiro token refresh transport failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return kiroRefreshResponse{}, errors.New("kiro token refresh response could not be read")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return kiroRefreshResponse{}, fmt.Errorf("kiro token refresh rejected with HTTP %d", resp.StatusCode)
	}
	var out kiroRefreshResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return kiroRefreshResponse{}, errors.New("kiro token refresh response was invalid")
	}
	return out, nil
}

func kiroCredential(account *Account, keys ...string) string {
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

func kiroCredentialExpiry(account *Account) time.Time {
	value := kiroCredential(account, "expiresAt", "expires_at", "expiry")
	if value == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}
	}
	if n > 10_000_000_000 {
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
}

func setKiroCredential(dst, original map[string]any, camel, snake string, value any) {
	key := camel
	if _, ok := original[snake]; ok {
		key = snake
	} else if _, ok := original[camel]; !ok && strings.Contains(snake, "_") {
		key = camel
	}
	dst[key] = value
}
