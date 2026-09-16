package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/kiro"
)

const (
	kiroOAuthSessionTTL = 15 * time.Minute
	kiroOAuthBodyLimit  = 1 << 20
)

type kiroOAuthSession struct {
	mu           sync.Mutex
	state        string
	verifier     string
	challenge    string
	clientID     string
	clientSecret string
	expiresAt    time.Time
	identityDone bool
	busy         bool
	tokenInfo    *KiroOAuthTokenInfo
}

type KiroOAuthStartResult struct {
	LoginID          string `json:"loginId"`
	AuthorizationURL string `json:"authorizationUrl"`
	RedirectURI      string `json:"redirectUri"`
	ExpiresAt        int64  `json:"expiresAt"`
}

type KiroOAuthCompleteResult struct {
	Status           string
	AuthorizationURL string
	TokenInfo        *KiroOAuthTokenInfo
}

type KiroOAuthTokenInfo struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ClientID     string
	ClientSecret string
	ExpiresAt    time.Time
	Subject      string
	Email        string
	Name         string
}

type KiroOAuthService struct {
	mu       sync.Mutex
	sessions map[string]*kiroOAuthSession
	client   *http.Client
}

func NewKiroOAuthService() *KiroOAuthService {
	return &KiroOAuthService{sessions: make(map[string]*kiroOAuthSession), client: &http.Client{Timeout: 20 * time.Second}}
}

func (s *KiroOAuthService) SetHTTPClient(client *http.Client) {
	if s == nil || client == nil {
		return
	}
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
}

func (s *KiroOAuthService) Start(ctx context.Context) (*KiroOAuthStartResult, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "KIRO_OAUTH_UNAVAILABLE", "Kiro OAuth is not configured")
	}
	state, err := kiro.GenerateOAuthSecret(32)
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "KIRO_OAUTH_STATE_FAILED", "failed to create Kiro OAuth state")
	}
	verifier, err := kiro.GenerateOAuthSecret(64)
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "KIRO_OAUTH_VERIFIER_FAILED", "failed to create Kiro OAuth verifier")
	}
	loginID, err := kiro.GenerateOAuthSessionID()
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "KIRO_OAUTH_SESSION_FAILED", "failed to create Kiro OAuth session")
	}
	registration := map[string]any{
		"clientName": "Kiro IDE", "clientType": "public",
		"grantTypes": []string{"authorization_code", "refresh_token"},
		"issuerUrl":  kiro.IssuerURL, "redirectUris": []string{kiro.RedirectURI},
		"scopes": kiro.OAuthScopes,
	}
	payload, status, err := s.request(ctx, kiro.RegisterURL, registration)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "KIRO_OAUTH_UPSTREAM_FAILED", "Kiro client registration failed (status %d)", status)
	}
	clientID, clientSecret := kiroString(payload, "clientId"), kiroString(payload, "clientSecret")
	if clientID == "" || clientSecret == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "KIRO_OAUTH_RESPONSE_INCOMPLETE", "Kiro client registration returned an incomplete response")
	}
	challenge := kiro.OAuthCodeChallenge(verifier)
	expiresAt := time.Now().Add(kiroOAuthSessionTTL)
	s.mu.Lock()
	s.cleanupLocked(time.Now())
	s.sessions[loginID] = &kiroOAuthSession{state: state, verifier: verifier, challenge: challenge, clientID: clientID, clientSecret: clientSecret, expiresAt: expiresAt}
	s.mu.Unlock()
	return &KiroOAuthStartResult{LoginID: loginID, AuthorizationURL: kiro.BuildSignInURL(state, challenge), RedirectURI: kiro.RedirectURI, ExpiresAt: expiresAt.UnixMilli()}, nil
}

func (s *KiroOAuthService) Complete(ctx context.Context, loginID, callbackURL string) (*KiroOAuthCompleteResult, error) {
	session, err := s.session(loginID)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	if !time.Now().Before(session.expiresAt) {
		session.mu.Unlock()
		s.delete(loginID)
		return nil, infraerrors.New(http.StatusGone, "KIRO_OAUTH_SESSION_EXPIRED", "Kiro OAuth session expired; restart login")
	}
	if session.busy {
		session.mu.Unlock()
		return nil, infraerrors.New(http.StatusConflict, "KIRO_OAUTH_SESSION_BUSY", "Kiro OAuth login is already being processed")
	}
	state, verifier, challenge := session.state, session.verifier, session.challenge
	clientID, clientSecret := session.clientID, session.clientSecret
	identityDone, tokenInfo := session.identityDone, session.tokenInfo
	session.busy = true
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.busy = false
		session.mu.Unlock()
	}()

	kind, code, parseErr := kiro.ParseOAuthCallback(callbackURL, state)
	if parseErr != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_OAUTH_CALLBACK_INVALID", parseErr.Error())
	}
	if tokenInfo != nil {
		if kind != kiro.CallbackAuthorization {
			return nil, infraerrors.New(http.StatusBadRequest, "KIRO_OAUTH_CALLBACK_INVALID", "Kiro authorization callback is required")
		}
		return &KiroOAuthCompleteResult{Status: "complete", TokenInfo: tokenInfo}, nil
	}
	if !identityDone {
		if kind == kiro.CallbackIdentity {
			session.mu.Lock()
			session.identityDone = true
			session.mu.Unlock()
			return &KiroOAuthCompleteResult{Status: "authorize", AuthorizationURL: kiro.BuildAuthorizeURL(clientID, state, challenge)}, nil
		}
	}
	if kind != kiro.CallbackAuthorization {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_OAUTH_CALLBACK_ORDER", "Kiro authorization callback is required")
	}
	payload, status, err := s.request(ctx, kiro.TokenURL, map[string]any{
		"clientId": clientID, "clientSecret": clientSecret, "grantType": "authorization_code",
		"code": code, "redirectUri": kiro.RedirectURI, "codeVerifier": verifier,
	})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "KIRO_OAUTH_UPSTREAM_FAILED", "Kiro token exchange failed (status %d)", status)
	}
	accessToken, refreshToken := kiroString(payload, "accessToken"), kiroString(payload, "refreshToken")
	if accessToken == "" || refreshToken == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "KIRO_OAUTH_RESPONSE_INCOMPLETE", "Kiro token exchange returned incomplete credentials")
	}
	expiresIn := kiroInt64(payload, "expiresIn")
	if expiresIn <= 0 || expiresIn > int64((24*time.Hour)/time.Second) {
		expiresIn = 8 * 60 * 60
	}
	idToken := kiroString(payload, "idToken")
	identity := decodeKiroIdentity(idToken)
	if identity.Subject == "" && identity.Email == "" {
		identity = decodeKiroIdentity(accessToken)
	}
	if identity.Email == "" {
		if enriched, enrichErr := s.fetchIdentity(ctx, accessToken); enrichErr == nil {
			identity = mergeKiroIdentity(identity, enriched)
		}
	}
	if identity.Subject == "" && identity.Email == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "KIRO_OAUTH_IDENTITY_MISSING", "Kiro login returned no stable account identity")
	}
	result := &KiroOAuthTokenInfo{
		AccessToken: accessToken, RefreshToken: refreshToken, IDToken: idToken,
		ClientID: clientID, ClientSecret: clientSecret, ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second),
		Subject: identity.Subject, Email: identity.Email, Name: identity.Name,
	}
	session.mu.Lock()
	session.tokenInfo = result
	session.mu.Unlock()
	return &KiroOAuthCompleteResult{Status: "complete", TokenInfo: result}, nil
}

func (s *KiroOAuthService) fetchIdentity(ctx context.Context, accessToken string) (kiroIdentity, error) {
	endpoint := "https://q.us-east-1.amazonaws.com/getUsageLimits?isemailRequired=true&profileArn=" + url.QueryEscape(kiro.SocialProfileARN)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return kiroIdentity{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	response, err := client.Do(request)
	if err != nil {
		return kiroIdentity{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return kiroIdentity{}, infraerrors.Newf(http.StatusBadGateway, "KIRO_OAUTH_IDENTITY_UNAVAILABLE", "Kiro identity lookup failed (status %d)", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, kiroOAuthBodyLimit))
	if err != nil {
		return kiroIdentity{}, err
	}
	var payload struct {
		UserInfo struct {
			Email  string `json:"email"`
			UserID string `json:"userId"`
			Name   string `json:"name"`
		} `json:"userInfo"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return kiroIdentity{}, err
	}
	return kiroIdentity{Subject: strings.TrimSpace(payload.UserInfo.UserID), Email: strings.TrimSpace(payload.UserInfo.Email), Name: strings.TrimSpace(payload.UserInfo.Name)}, nil
}

func (s *KiroOAuthService) Consume(loginID string) { s.delete(loginID) }
func (s *KiroOAuthService) Cancel(loginID string)  { s.delete(loginID) }

func (s *KiroOAuthService) session(loginID string) (*kiroOAuthSession, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "KIRO_OAUTH_UNAVAILABLE", "Kiro OAuth is not configured")
	}
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_OAUTH_SESSION_REQUIRED", "loginId is required")
	}
	s.mu.Lock()
	s.cleanupLocked(time.Now())
	item := s.sessions[loginID]
	s.mu.Unlock()
	if item == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "KIRO_OAUTH_SESSION_NOT_FOUND", "Kiro OAuth session not found or expired")
	}
	return item, nil
}

func (s *KiroOAuthService) delete(loginID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.sessions, strings.TrimSpace(loginID))
	s.mu.Unlock()
}

func (s *KiroOAuthService) cleanupLocked(now time.Time) {
	for loginID, session := range s.sessions {
		if !now.Before(session.expiresAt) {
			delete(s.sessions, loginID)
		}
	}
}

func (s *KiroOAuthService) request(ctx context.Context, endpoint string, input map[string]any) (map[string]any, int, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusInternalServerError, "KIRO_OAUTH_REQUEST_FAILED", "failed to create Kiro OAuth request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusInternalServerError, "KIRO_OAUTH_REQUEST_FAILED", "failed to create Kiro OAuth request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusBadGateway, "KIRO_OAUTH_UPSTREAM_UNAVAILABLE", "Kiro OAuth upstream is unavailable").WithCause(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, kiroOAuthBodyLimit))
	if err != nil {
		return nil, response.StatusCode, infraerrors.New(http.StatusBadGateway, "KIRO_OAUTH_RESPONSE_FAILED", "failed to read Kiro OAuth response")
	}
	if len(data) == 0 {
		return map[string]any{}, response.StatusCode, nil
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return nil, response.StatusCode, infraerrors.New(http.StatusBadGateway, "KIRO_OAUTH_RESPONSE_INVALID", "Kiro OAuth upstream returned invalid JSON")
	}
	return payload, response.StatusCode, nil
}

type kiroIdentity struct{ Subject, Email, Name string }

func mergeKiroIdentity(primary, fallback kiroIdentity) kiroIdentity {
	if primary.Subject == "" {
		primary.Subject = fallback.Subject
	}
	if primary.Email == "" {
		primary.Email = fallback.Email
	}
	if primary.Name == "" {
		primary.Name = fallback.Name
	}
	return primary
}

func decodeKiroIdentity(token string) kiroIdentity {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return kiroIdentity{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return kiroIdentity{}
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return kiroIdentity{}
	}
	return kiroIdentity{Subject: kiroString(claims, "sub"), Email: firstKiroString(claims, "email", "preferred_username", "username"), Name: firstKiroString(claims, "name", "display_name")}
}

func firstKiroString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := kiroString(payload, key); value != "" {
			return value
		}
	}
	return ""
}
func kiroString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}
func kiroInt64(payload map[string]any, key string) int64 {
	value, _ := payload[key].(float64)
	return int64(value)
}
