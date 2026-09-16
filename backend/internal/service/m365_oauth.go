package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/m365"
)

const (
	m365OAuthSessionTTL = 15 * time.Minute
	m365OAuthBodyLimit  = 1 << 20
	m365DefaultTokenTTL = time.Hour
)

type m365OAuthSession struct {
	mu        sync.Mutex
	state     string
	verifier  string
	expiresAt time.Time
	polling   bool
	tokenInfo *M365OAuthTokenInfo
}

// M365OAuthStartResult contains only values safe for the operator browser.
type M365OAuthStartResult struct {
	LoginID          string `json:"loginId"`
	AuthorizationURL string `json:"authorizationUrl"`
	RedirectURI      string `json:"redirectUri"`
	ExpiresAt        int64  `json:"expiresAt"`
}

type M365OAuthTokenInfo struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    int64
	OID          string
	TID          string
	Email        string
	Name         string
}

type M365OAuthService struct {
	mu       sync.Mutex
	sessions map[string]*m365OAuthSession
	client   *http.Client
}

func NewM365OAuthService() *M365OAuthService {
	return &M365OAuthService{
		sessions: make(map[string]*m365OAuthSession),
		client:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (s *M365OAuthService) SetHTTPClient(client *http.Client) {
	if s == nil || client == nil {
		return
	}
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
}

func (s *M365OAuthService) Start(ctx context.Context) (*M365OAuthStartResult, error) {
	_ = ctx
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "M365_OAUTH_UNAVAILABLE", "M365 OAuth is not configured")
	}
	state, err := m365.GenerateState()
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "M365_OAUTH_STATE_FAILED", "failed to create M365 OAuth state")
	}
	verifier, err := m365.GenerateCodeVerifier()
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "M365_OAUTH_VERIFIER_FAILED", "failed to create M365 OAuth verifier")
	}
	loginID, err := m365.GenerateSessionID()
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "M365_OAUTH_SESSION_FAILED", "failed to create M365 OAuth session")
	}
	expiresAt := time.Now().Add(m365OAuthSessionTTL)
	s.mu.Lock()
	s.cleanupLocked(time.Now())
	if s.sessions == nil {
		s.sessions = make(map[string]*m365OAuthSession)
	}
	s.sessions[loginID] = &m365OAuthSession{state: state, verifier: verifier, expiresAt: expiresAt}
	s.mu.Unlock()
	return &M365OAuthStartResult{
		LoginID: loginID, AuthorizationURL: m365.BuildAuthorizationURL(state, m365.CodeChallenge(verifier)),
		RedirectURI: m365.RedirectURI, ExpiresAt: expiresAt.UnixMilli(),
	}, nil
}

func (s *M365OAuthService) Complete(ctx context.Context, loginID, callbackURL string) (*M365OAuthTokenInfo, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "M365_OAUTH_UNAVAILABLE", "M365 OAuth is not configured")
	}
	session, err := s.session(loginID)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	now := time.Now()
	if !now.Before(session.expiresAt) {
		session.mu.Unlock()
		s.deleteSession(loginID)
		return nil, infraerrors.New(http.StatusGone, "M365_OAUTH_SESSION_EXPIRED", "M365 OAuth session expired; restart login")
	}
	if session.tokenInfo != nil {
		result := session.tokenInfo
		state := session.state
		session.mu.Unlock()
		if _, err := m365.ParseCallback(callbackURL, state); err != nil {
			return nil, infraerrors.New(http.StatusBadRequest, "M365_OAUTH_CALLBACK_INVALID", "invalid Microsoft OAuth callback")
		}
		return result, nil
	}
	if session.polling {
		session.mu.Unlock()
		return nil, infraerrors.New(http.StatusConflict, "M365_OAUTH_SESSION_BUSY", "M365 OAuth login is already being processed")
	}
	session.polling = true
	state, verifier := session.state, session.verifier
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.polling = false
		session.mu.Unlock()
	}()

	code, err := m365.ParseCallback(callbackURL, state)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "M365_OAUTH_CALLBACK_INVALID", "invalid Microsoft OAuth callback")
	}
	form := url.Values{
		"client_id": {m365.ClientID}, "grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {m365.RedirectURI}, "code_verifier": {verifier}, "scope": {m365.Scope},
	}
	payload, status, err := s.request(ctx, http.MethodPost, m365.TokenURL, form)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "M365_OAUTH_UPSTREAM_FAILED", "Microsoft token exchange failed (status %d)", status)
	}
	accessToken := m365JSONString(payload, "access_token")
	if accessToken == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "M365_OAUTH_TOKEN_INCOMPLETE", "Microsoft token exchange returned no access token")
	}
	identity, identityErr := m365.DecodeIdentity(accessToken)
	if identityErr != nil {
		if idToken := m365JSONString(payload, "id_token"); idToken != "" {
			identity, identityErr = m365.DecodeIdentity(idToken)
		}
	}
	if identityErr != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "M365_OAUTH_IDENTITY_MISSING", "Microsoft token did not include oid and tid")
	}
	expiresIn := m365JSONInt64(payload, "expires_in")
	if expiresIn <= 0 || expiresIn > int64((24*time.Hour)/time.Second) {
		expiresIn = int64(m365DefaultTokenTTL / time.Second)
	}
	result := &M365OAuthTokenInfo{
		AccessToken: accessToken, RefreshToken: m365JSONString(payload, "refresh_token"),
		IDToken: m365JSONString(payload, "id_token"), ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second).Unix(),
		OID: identity.OID, TID: identity.TID, Email: identity.Email, Name: identity.Name,
	}
	session.mu.Lock()
	session.tokenInfo = result
	session.mu.Unlock()
	return result, nil
}

func (s *M365OAuthService) Consume(loginID string) { s.deleteSession(loginID) }

func (s *M365OAuthService) Cancel(loginID string) { s.deleteSession(loginID) }

func (s *M365OAuthService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.sessions = make(map[string]*m365OAuthSession)
	s.mu.Unlock()
}

func (s *M365OAuthService) session(loginID string) (*m365OAuthSession, error) {
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "M365_OAUTH_SESSION_REQUIRED", "loginId is required")
	}
	s.mu.Lock()
	s.cleanupLocked(time.Now())
	item := s.sessions[loginID]
	s.mu.Unlock()
	if item == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "M365_OAUTH_SESSION_NOT_FOUND", "M365 OAuth session not found or expired")
	}
	return item, nil
}

func (s *M365OAuthService) deleteSession(loginID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.sessions, strings.TrimSpace(loginID))
	s.mu.Unlock()
}

func (s *M365OAuthService) cleanupLocked(now time.Time) {
	for loginID, session := range s.sessions {
		if !now.Before(session.expiresAt) {
			delete(s.sessions, loginID)
		}
	}
}

func (s *M365OAuthService) request(ctx context.Context, method, endpoint string, form url.Values) (map[string]any, int, error) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusInternalServerError, "M365_OAUTH_REQUEST_FAILED", "failed to create Microsoft OAuth request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusBadGateway, "M365_OAUTH_UPSTREAM_UNAVAILABLE", "Microsoft OAuth upstream is unavailable").WithCause(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, m365OAuthBodyLimit))
	if err != nil {
		return nil, response.StatusCode, infraerrors.New(http.StatusBadGateway, "M365_OAUTH_RESPONSE_FAILED", "failed to read Microsoft OAuth response")
	}
	if len(body) == 0 {
		return map[string]any{}, response.StatusCode, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, response.StatusCode, infraerrors.New(http.StatusBadGateway, "M365_OAUTH_RESPONSE_INVALID", "Microsoft OAuth upstream returned invalid JSON")
	}
	return payload, response.StatusCode, nil
}

func m365JSONString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func m365JSONInt64(payload map[string]any, key string) int64 {
	value, ok := payload[key].(float64)
	if !ok {
		return 0
	}
	return int64(value)
}
