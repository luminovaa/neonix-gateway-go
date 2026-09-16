package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/openai"
)

const (
	codexDeviceSessionTTL  = 15 * time.Minute
	codexDeviceMinInterval = 1
	codexDeviceMaxInterval = 30
	codexDeviceBodyLimit   = 1 << 20
)

type codexDeviceSession struct {
	mu sync.Mutex

	loginID      string
	deviceAuthID string
	userCode     string
	interval     int
	expiresAt    time.Time

	polling           bool
	authorizationCode string
	codeVerifier      string
	codeChallenge     string
	tokenInfo         *OpenAITokenInfo
}

// CodexDeviceStartResult is the browser-safe part of the official Codex
// device authorization flow. Device IDs and verifiers never leave the server.
type CodexDeviceStartResult struct {
	LoginID         string `json:"loginId"`
	VerificationURL string `json:"verificationUrl"`
	UserCode        string `json:"userCode"`
	Interval        int    `json:"interval"`
	ExpiresAt       int64  `json:"expiresAt"`
}

// CodexDevicePollResult is deliberately not JSON-shaped: TokenInfo is
// consumed by the admin handler and is never serialized into an HTTP response.
type CodexDevicePollResult struct {
	Pending    bool
	RetryAfter int
	ExpiresAt  int64
	TokenInfo  *OpenAITokenInfo
}

type codexDeviceJSON map[string]any

// SetCodexDeviceHTTPClient injects an HTTP client for the device endpoints.
// Production uses the bounded default client; tests use a transport that does
// not contact OpenAI. The client is not copied so its transport remains under
// application control.
func (s *OpenAIOAuthService) SetCodexDeviceHTTPClient(client *http.Client) {
	if s == nil || client == nil {
		return
	}
	s.codexDeviceMu.Lock()
	s.codexDeviceClient = client
	s.codexDeviceMu.Unlock()
}

// StartCodexDevice starts the official Codex device authorization flow.
func (s *OpenAIOAuthService) StartCodexDevice(ctx context.Context) (*CodexDeviceStartResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_CODEX_OAUTH_UNAVAILABLE", "Codex OAuth is not configured")
	}
	s.cleanupCodexDeviceSessions(time.Now())
	data, status, err := s.codexDeviceRequest(ctx, http.MethodPost, openai.DeviceUserCodeURL, mustJSON(map[string]string{"client_id": openai.ClientID}))
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, codexDeviceUpstreamError("OPENAI_CODEX_DEVICE_START_FAILED", status)
	}
	deviceAuthID := jsonString(data, "device_auth_id")
	userCode := jsonString(data, "user_code")
	if userCode == "" {
		userCode = jsonString(data, "usercode")
	}
	if deviceAuthID == "" || userCode == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_DEVICE_RESPONSE_INCOMPLETE", "Codex device login returned an incomplete response")
	}
	interval := jsonInt(data, "interval")
	if interval < codexDeviceMinInterval {
		interval = 5
	}
	if interval > codexDeviceMaxInterval {
		interval = codexDeviceMaxInterval
	}
	loginID, err := openai.GenerateSessionID()
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_CODEX_DEVICE_SESSION_FAILED", "failed to create Codex OAuth session")
	}
	expiresAt := time.Now().Add(codexDeviceSessionTTL)
	s.codexDeviceMu.Lock()
	if s.codexDeviceSessions == nil {
		s.codexDeviceSessions = make(map[string]*codexDeviceSession)
	}
	s.codexDeviceSessions[loginID] = &codexDeviceSession{
		loginID: loginID, deviceAuthID: deviceAuthID, userCode: userCode,
		interval: interval, expiresAt: expiresAt,
	}
	s.codexDeviceMu.Unlock()
	return &CodexDeviceStartResult{
		LoginID: loginID, VerificationURL: openai.DeviceVerifyURL, UserCode: userCode,
		Interval: interval, ExpiresAt: expiresAt.UnixMilli(),
	}, nil
}

// PollCodexDevice performs one bounded poll. Concurrent polls for one login
// are single-flight: the loser receives pending and the UI schedules the next
// attempt using the server-provided interval.
func (s *OpenAIOAuthService) PollCodexDevice(ctx context.Context, loginID string) (*CodexDevicePollResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_CODEX_OAUTH_UNAVAILABLE", "Codex OAuth is not configured")
	}
	session, err := s.codexDeviceSession(loginID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	session.mu.Lock()
	if !now.Before(session.expiresAt) {
		session.mu.Unlock()
		s.deleteCodexDeviceSession(loginID)
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_CODEX_DEVICE_SESSION_EXPIRED", "Codex OAuth session expired; restart login")
	}
	if session.tokenInfo != nil {
		tokenInfo := session.tokenInfo
		session.mu.Unlock()
		return &CodexDevicePollResult{ExpiresAt: session.expiresAt.UnixMilli(), TokenInfo: tokenInfo}, nil
	}
	if session.polling {
		retry := session.interval
		session.mu.Unlock()
		return &CodexDevicePollResult{Pending: true, RetryAfter: retry, ExpiresAt: session.expiresAt.UnixMilli()}, nil
	}
	session.polling = true
	interval := session.interval
	deviceAuthID := session.deviceAuthID
	userCode := session.userCode
	authorizationCode := session.authorizationCode
	codeVerifier := session.codeVerifier
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.polling = false
		session.mu.Unlock()
	}()

	if authorizationCode == "" {
		payload, status, err := s.codexDeviceRequest(ctx, http.MethodPost, openai.DeviceTokenURL, mustJSON(map[string]string{
			"device_auth_id": deviceAuthID,
			"user_code":      userCode,
		}))
		if err != nil {
			return nil, err
		}
		if status == http.StatusForbidden || status == http.StatusNotFound {
			return &CodexDevicePollResult{Pending: true, RetryAfter: interval, ExpiresAt: session.expiresAt.UnixMilli()}, nil
		}
		if status < 200 || status >= 300 {
			return nil, codexDeviceUpstreamError("OPENAI_CODEX_DEVICE_POLL_FAILED", status)
		}
		authorizationCode = jsonString(payload, "authorization_code")
		codeVerifier = jsonString(payload, "code_verifier")
		codeChallenge := jsonString(payload, "code_challenge")
		if authorizationCode == "" || codeVerifier == "" || codeChallenge == "" {
			return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_DEVICE_AUTHORIZATION_INCOMPLETE", "Codex authorization returned an incomplete response")
		}
		session.mu.Lock()
		session.authorizationCode = authorizationCode
		session.codeVerifier = codeVerifier
		session.codeChallenge = codeChallenge
		session.mu.Unlock()
	}

	if s.oauthClient == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_CODEX_OAUTH_UNAVAILABLE", "Codex token exchange is not configured")
	}
	tokenResp, err := s.oauthClient.ExchangeCode(ctx, authorizationCode, codeVerifier, openai.DeviceRedirectURI, "", openai.ClientID)
	if err != nil {
		return nil, err
	}
	if tokenResp == nil || strings.TrimSpace(tokenResp.AccessToken) == "" ||
		strings.TrimSpace(tokenResp.RefreshToken) == "" || strings.TrimSpace(tokenResp.IDToken) == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_TOKEN_RESPONSE_INCOMPLETE", "Codex token exchange returned an incomplete token bundle")
	}
	tokenInfo := &OpenAITokenInfo{
		AccessToken: tokenResp.AccessToken, RefreshToken: tokenResp.RefreshToken, IDToken: tokenResp.IDToken,
		ExpiresIn: int64(tokenResp.ExpiresIn), ExpiresAt: time.Now().Unix() + int64(tokenResp.ExpiresIn), ClientID: openai.ClientID,
	}
	if claims, parseErr := openai.DecodeIDToken(tokenResp.IDToken); parseErr == nil {
		userInfo := claims.GetUserInfo()
		tokenInfo.Email = userInfo.Email
		tokenInfo.ChatGPTAccountID = userInfo.ChatGPTAccountID
		tokenInfo.ChatGPTUserID = userInfo.ChatGPTUserID
		tokenInfo.OrganizationID = userInfo.OrganizationID
		tokenInfo.PlanType = userInfo.PlanType
	}
	if tokenInfo.ChatGPTAccountID == "" && tokenInfo.Email == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_IDENTITY_MISSING", "Codex token exchange did not include an account identity")
	}
	s.enrichTokenInfo(ctx, tokenInfo, "")
	session.mu.Lock()
	session.tokenInfo = tokenInfo
	session.mu.Unlock()
	return &CodexDevicePollResult{ExpiresAt: session.expiresAt.UnixMilli(), TokenInfo: tokenInfo}, nil
}

// ConsumeCodexDevice removes a successfully persisted session. Keeping the
// token in memory until persistence succeeds lets the UI retry a transient DB
// failure without asking the operator to log in again.
func (s *OpenAIOAuthService) ConsumeCodexDevice(loginID string) {
	s.deleteCodexDeviceSession(loginID)
}

// CancelCodexDevice is idempotent and safe to call when a dialog closes.
func (s *OpenAIOAuthService) CancelCodexDevice(loginID string) {
	s.deleteCodexDeviceSession(loginID)
}

func (s *OpenAIOAuthService) codexDeviceSession(loginID string) (*codexDeviceSession, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_CODEX_OAUTH_UNAVAILABLE", "Codex OAuth is not configured")
	}
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_CODEX_DEVICE_SESSION_REQUIRED", "loginId is required")
	}
	s.codexDeviceMu.Lock()
	if s.codexDeviceSessions == nil {
		s.codexDeviceSessions = make(map[string]*codexDeviceSession)
	}
	s.cleanupCodexDeviceSessionsLocked(time.Now())
	session := s.codexDeviceSessions[loginID]
	s.codexDeviceMu.Unlock()
	if session == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_CODEX_DEVICE_SESSION_NOT_FOUND", "Codex OAuth session not found or expired")
	}
	return session, nil
}

func (s *OpenAIOAuthService) deleteCodexDeviceSession(loginID string) {
	if s == nil {
		return
	}
	s.codexDeviceMu.Lock()
	delete(s.codexDeviceSessions, strings.TrimSpace(loginID))
	s.codexDeviceMu.Unlock()
}

func (s *OpenAIOAuthService) cleanupCodexDeviceSessions(now time.Time) {
	s.codexDeviceMu.Lock()
	s.cleanupCodexDeviceSessionsLocked(now)
	s.codexDeviceMu.Unlock()
}

func (s *OpenAIOAuthService) cleanupCodexDeviceSessionsLocked(now time.Time) {
	for loginID, session := range s.codexDeviceSessions {
		if !now.Before(session.expiresAt) {
			delete(s.codexDeviceSessions, loginID)
		}
	}
}

func (s *OpenAIOAuthService) codexDeviceRequest(ctx context.Context, method, endpoint string, body []byte) (codexDeviceJSON, int, error) {
	s.codexDeviceMu.Lock()
	client := s.codexDeviceClient
	s.codexDeviceMu.Unlock()
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusInternalServerError, "OPENAI_CODEX_DEVICE_REQUEST_FAILED", "failed to create Codex OAuth request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Codex/1.0")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_DEVICE_UPSTREAM_UNAVAILABLE", "Codex OAuth upstream is unavailable").WithCause(err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, codexDeviceBodyLimit)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, response.StatusCode, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_DEVICE_RESPONSE_FAILED", "failed to read Codex OAuth response")
	}
	if len(data) == 0 {
		return codexDeviceJSON{}, response.StatusCode, nil
	}
	var payload codexDeviceJSON
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, response.StatusCode, infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_DEVICE_RESPONSE_INVALID", "Codex OAuth upstream returned invalid JSON")
	}
	return payload, response.StatusCode, nil
}

func codexDeviceUpstreamError(code string, status int) error {
	return infraerrors.Newf(http.StatusBadGateway, code, "Codex OAuth upstream rejected the device request (status %d)", status)
}

func jsonString(payload codexDeviceJSON, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func jsonInt(payload codexDeviceJSON, key string) int {
	switch value := payload[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte(`{}`)
	}
	return encoded
}
