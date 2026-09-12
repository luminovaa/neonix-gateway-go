package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/xai"
)

const (
	grokDeviceSessionTTL  = 15 * time.Minute
	grokDeviceMinInterval = 1
	grokDeviceMaxInterval = 30
	grokDeviceBodyLimit   = 1 << 20
)

type grokDeviceSession struct {
	mu sync.Mutex

	loginID    string
	deviceCode string
	userCode   string
	verifyURL  string
	interval   int
	expiresAt  time.Time
	polling    bool
	tokenInfo  *GrokTokenInfo
}

// GrokDeviceStartResult is the browser-safe result of the RFC 8628 flow.
type GrokDeviceStartResult struct {
	LoginID         string `json:"loginId"`
	VerificationURL string `json:"verificationUrl"`
	UserCode        string `json:"userCode"`
	Interval        int    `json:"interval"`
	ExpiresAt       int64  `json:"expiresAt"`
}

// GrokDevicePollResult keeps token information in process memory only. The
// account handler consumes it after persistence and returns a sanitized DTO.
type GrokDevicePollResult struct {
	Pending    bool
	RetryAfter int
	ExpiresAt  int64
	TokenInfo  *GrokTokenInfo
}

type grokDeviceJSON map[string]any

// SetGrokDeviceHTTPClient is used by tests and controlled deployments. The
// production default is a bounded client and uses fixed xAI endpoints.
func (s *GrokOAuthService) SetGrokDeviceHTTPClient(client *http.Client) {
	if s == nil || client == nil {
		return
	}
	s.deviceMu.Lock()
	s.deviceClient = client
	s.deviceMu.Unlock()
}

// StartGrokDevice requests an xAI device code without automating a browser or
// accepting a password.
func (s *GrokOAuthService) StartGrokDevice(ctx context.Context) (*GrokDeviceStartResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "GROK_OAUTH_UNAVAILABLE", "Grok OAuth is not configured")
	}
	s.cleanupGrokDeviceSessions(time.Now())
	form := url.Values{
		"client_id": {xai.EffectiveClientID()},
		"scope":     {xai.EffectiveScope()},
		"referrer":  {"grok-build"},
	}
	payload, status, err := s.grokDeviceRequest(ctx, http.MethodPost, xai.SSODeviceURL, form)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "GROK_DEVICE_START_FAILED", "Grok device authorization was rejected (status %d)", status)
	}
	deviceCode := jsonStringValue(payload, "device_code")
	userCode := jsonStringValue(payload, "user_code")
	verifyURL := jsonStringValue(payload, "verification_uri_complete")
	if verifyURL == "" {
		verifyURL = jsonStringValue(payload, "verification_uri")
	}
	if deviceCode == "" || userCode == "" || !validGrokVerificationURL(verifyURL) {
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_DEVICE_RESPONSE_INCOMPLETE", "Grok device login returned an incomplete response")
	}
	interval := jsonIntValue(payload, "interval")
	if interval < grokDeviceMinInterval {
		interval = 5
	}
	if interval > grokDeviceMaxInterval {
		interval = grokDeviceMaxInterval
	}
	expiresIn := jsonIntValue(payload, "expires_in")
	maxExpiresIn := int(grokDeviceSessionTTL / time.Second)
	if expiresIn <= 0 || expiresIn > maxExpiresIn {
		expiresIn = maxExpiresIn
	}
	loginID, err := xai.GenerateSessionID()
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "GROK_DEVICE_SESSION_FAILED", "failed to create Grok OAuth session")
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
	s.deviceMu.Lock()
	if s.deviceSessions == nil {
		s.deviceSessions = make(map[string]*grokDeviceSession)
	}
	s.deviceSessions[loginID] = &grokDeviceSession{
		loginID: loginID, deviceCode: deviceCode, userCode: userCode,
		verifyURL: verifyURL, interval: interval, expiresAt: expiresAt,
	}
	s.deviceMu.Unlock()
	return &GrokDeviceStartResult{LoginID: loginID, VerificationURL: verifyURL, UserCode: userCode, Interval: interval, ExpiresAt: expiresAt.UnixMilli()}, nil
}

// PollGrokDevice performs one RFC 8628 token poll. authorization_pending and
// slow_down remain pending; access_denied and expired_token are actionable
// errors and do not trigger a blind retry loop in the UI.
func (s *GrokOAuthService) PollGrokDevice(ctx context.Context, loginID string) (*GrokDevicePollResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "GROK_OAUTH_UNAVAILABLE", "Grok OAuth is not configured")
	}
	session, err := s.grokDeviceSession(loginID)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	now := time.Now()
	if !now.Before(session.expiresAt) {
		session.mu.Unlock()
		s.deleteGrokDeviceSession(loginID)
		return nil, infraerrors.New(http.StatusGone, "GROK_DEVICE_SESSION_EXPIRED", "Grok device code expired; restart login")
	}
	if session.tokenInfo != nil {
		tokenInfo := session.tokenInfo
		session.mu.Unlock()
		return &GrokDevicePollResult{ExpiresAt: session.expiresAt.UnixMilli(), TokenInfo: tokenInfo}, nil
	}
	if session.polling {
		retry := session.interval
		session.mu.Unlock()
		return &GrokDevicePollResult{Pending: true, RetryAfter: retry, ExpiresAt: session.expiresAt.UnixMilli()}, nil
	}
	session.polling = true
	deviceCode := session.deviceCode
	interval := session.interval
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.polling = false
		session.mu.Unlock()
	}()

	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {xai.EffectiveClientID()},
	}
	payload, status, err := s.grokDeviceRequest(ctx, http.MethodPost, xai.SSOTokenURL, form)
	if err != nil {
		return nil, err
	}
	if status == http.StatusBadRequest {
		switch jsonStringValue(payload, "error") {
		case "authorization_pending":
			return &GrokDevicePollResult{Pending: true, RetryAfter: interval, ExpiresAt: session.expiresAt.UnixMilli()}, nil
		case "slow_down":
			session.mu.Lock()
			session.interval = minInt(grokDeviceMaxInterval, session.interval+5)
			retry := session.interval
			session.mu.Unlock()
			return &GrokDevicePollResult{Pending: true, RetryAfter: retry, ExpiresAt: session.expiresAt.UnixMilli()}, nil
		case "access_denied":
			return nil, infraerrors.New(http.StatusBadRequest, "GROK_DEVICE_ACCESS_DENIED", "Grok login was denied")
		case "expired_token":
			return nil, infraerrors.New(http.StatusGone, "GROK_DEVICE_SESSION_EXPIRED", "Grok device code expired; restart login")
		}
	}
	if status < 200 || status >= 300 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "GROK_DEVICE_POLL_FAILED", "Grok device authorization failed (status %d)", status)
	}
	accessToken := jsonStringValue(payload, "access_token")
	if accessToken == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_DEVICE_TOKEN_INCOMPLETE", "Grok token exchange returned no access token")
	}
	tokenResp := &xai.TokenResponse{
		AccessToken:  accessToken,
		RefreshToken: jsonStringValue(payload, "refresh_token"),
		IDToken:      jsonStringValue(payload, "id_token"),
		TokenType:    jsonStringValue(payload, "token_type"),
		ExpiresIn:    int64(jsonIntValue(payload, "expires_in")),
		Scope:        jsonStringValue(payload, "scope"),
	}
	tokenInfo := s.tokenInfoFromResponse(tokenResp, xai.EffectiveClientID(), nil)
	if tokenInfo.RefreshToken == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_DEVICE_REFRESH_TOKEN_MISSING", "Grok did not return a refresh token; restart login and approve offline access")
	}
	if tokenInfo.Subject == "" && tokenInfo.Email == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "GROK_DEVICE_IDENTITY_MISSING", "Grok token did not include an account identity")
	}
	session.mu.Lock()
	session.tokenInfo = tokenInfo
	session.mu.Unlock()
	return &GrokDevicePollResult{ExpiresAt: session.expiresAt.UnixMilli(), TokenInfo: tokenInfo}, nil
}

func (s *GrokOAuthService) ConsumeGrokDevice(loginID string) {
	s.deleteGrokDeviceSession(loginID)
}

func (s *GrokOAuthService) CancelGrokDevice(loginID string) {
	s.deleteGrokDeviceSession(loginID)
}

func (s *GrokOAuthService) grokDeviceSession(loginID string) (*grokDeviceSession, error) {
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "GROK_DEVICE_SESSION_REQUIRED", "loginId is required")
	}
	s.deviceMu.Lock()
	if s.deviceSessions == nil {
		s.deviceSessions = make(map[string]*grokDeviceSession)
	}
	s.cleanupGrokDeviceSessionsLocked(time.Now())
	session := s.deviceSessions[loginID]
	s.deviceMu.Unlock()
	if session == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "GROK_DEVICE_SESSION_NOT_FOUND", "Grok OAuth session not found or expired")
	}
	return session, nil
}

func (s *GrokOAuthService) deleteGrokDeviceSession(loginID string) {
	if s == nil {
		return
	}
	s.deviceMu.Lock()
	delete(s.deviceSessions, strings.TrimSpace(loginID))
	s.deviceMu.Unlock()
}

func (s *GrokOAuthService) cleanupGrokDeviceSessions(now time.Time) {
	s.deviceMu.Lock()
	s.cleanupGrokDeviceSessionsLocked(now)
	s.deviceMu.Unlock()
}

func (s *GrokOAuthService) cleanupGrokDeviceSessionsLocked(now time.Time) {
	for loginID, session := range s.deviceSessions {
		if !now.Before(session.expiresAt) {
			delete(s.deviceSessions, loginID)
		}
	}
}

func (s *GrokOAuthService) grokDeviceRequest(ctx context.Context, method, endpoint string, form url.Values) (grokDeviceJSON, int, error) {
	s.deviceMu.Lock()
	client := s.deviceClient
	s.deviceMu.Unlock()
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusInternalServerError, "GROK_DEVICE_REQUEST_FAILED", "failed to create Grok OAuth request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-grok-client-surface", "ui")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusBadGateway, "GROK_DEVICE_UPSTREAM_UNAVAILABLE", "Grok OAuth upstream is unavailable").WithCause(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, grokDeviceBodyLimit))
	if err != nil {
		return nil, response.StatusCode, infraerrors.New(http.StatusBadGateway, "GROK_DEVICE_RESPONSE_FAILED", "failed to read Grok OAuth response")
	}
	if len(data) == 0 {
		return grokDeviceJSON{}, response.StatusCode, nil
	}
	var payload grokDeviceJSON
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, response.StatusCode, infraerrors.New(http.StatusBadGateway, "GROK_DEVICE_RESPONSE_INVALID", "Grok OAuth upstream returned invalid JSON")
	}
	return payload, response.StatusCode, nil
}

func jsonStringValue(payload grokDeviceJSON, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func validGrokVerificationURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "auth.x.ai" {
		return false
	}
	return parsed.Path != "" && parsed.Fragment == ""
}

func jsonIntValue(payload grokDeviceJSON, key string) int {
	switch value := payload[key].(type) {
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := strconv.Atoi(string(value))
		return parsed
	case int:
		return value
	default:
		return 0
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
