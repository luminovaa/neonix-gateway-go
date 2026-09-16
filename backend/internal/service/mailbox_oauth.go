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
	mailboxOAuthSessionTTL = 15 * time.Minute
	mailboxOAuthBodyLimit  = 1 << 20
)

type mailboxOAuthSession struct {
	mu        sync.Mutex
	state     string
	verifier  string
	expiresAt time.Time
	working   bool
	tokenInfo *MailboxOAuthTokenInfo
}

type MailboxOAuthStartResult struct {
	LoginID          string `json:"loginId"`
	AuthorizationURL string `json:"authorizationUrl"`
	RedirectURI      string `json:"redirectUri"`
	ExpiresAt        int64  `json:"expiresAt"`
}

type MailboxOAuthTokenInfo struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    int64
	OID          string
	TID          string
	Email        string
	Name         string
}

type MailboxOAuthService struct {
	mu       sync.Mutex
	sessions map[string]*mailboxOAuthSession
	client   *http.Client
}

func NewMailboxOAuthService() *MailboxOAuthService {
	return &MailboxOAuthService{sessions: make(map[string]*mailboxOAuthSession), client: &http.Client{Timeout: 20 * time.Second}}
}

func (s *MailboxOAuthService) SetHTTPClient(client *http.Client) {
	if s == nil || client == nil {
		return
	}
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
}

func (s *MailboxOAuthService) Start(ctx context.Context) (*MailboxOAuthStartResult, error) {
	_ = ctx
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "MAILBOX_OAUTH_UNAVAILABLE", "mailbox OAuth is not configured")
	}
	state, err := m365.GenerateState()
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "MAILBOX_OAUTH_STATE_FAILED", "failed to create mailbox OAuth state")
	}
	verifier, err := m365.GenerateCodeVerifier()
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "MAILBOX_OAUTH_VERIFIER_FAILED", "failed to create mailbox OAuth verifier")
	}
	loginID, err := m365.GenerateSessionID()
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "MAILBOX_OAUTH_SESSION_FAILED", "failed to create mailbox OAuth session")
	}
	expiresAt := time.Now().Add(mailboxOAuthSessionTTL)
	s.mu.Lock()
	s.cleanupLocked(time.Now())
	if s.sessions == nil {
		s.sessions = make(map[string]*mailboxOAuthSession)
	}
	s.sessions[loginID] = &mailboxOAuthSession{state: state, verifier: verifier, expiresAt: expiresAt}
	s.mu.Unlock()
	return &MailboxOAuthStartResult{LoginID: loginID, AuthorizationURL: m365.BuildMailboxAuthorizationURL(state, m365.CodeChallenge(verifier)), RedirectURI: m365.RedirectURI, ExpiresAt: expiresAt.UnixMilli()}, nil
}

func (s *MailboxOAuthService) Complete(ctx context.Context, loginID, callbackURL string) (*MailboxOAuthTokenInfo, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "MAILBOX_OAUTH_UNAVAILABLE", "mailbox OAuth is not configured")
	}
	session, err := s.session(loginID)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	if !time.Now().Before(session.expiresAt) {
		session.mu.Unlock()
		s.deleteSession(loginID)
		return nil, infraerrors.New(http.StatusGone, "MAILBOX_OAUTH_SESSION_EXPIRED", "mailbox OAuth session expired; restart login")
	}
	state := session.state
	if session.tokenInfo != nil {
		result := session.tokenInfo
		session.mu.Unlock()
		if _, err := m365.ParseCallback(callbackURL, state); err != nil {
			return nil, infraerrors.New(http.StatusBadRequest, "MAILBOX_OAUTH_CALLBACK_INVALID", "invalid Microsoft mailbox callback")
		}
		return result, nil
	}
	if session.working {
		session.mu.Unlock()
		return nil, infraerrors.New(http.StatusConflict, "MAILBOX_OAUTH_SESSION_BUSY", "mailbox OAuth login is already being processed")
	}
	session.working = true
	verifier := session.verifier
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.working = false
		session.mu.Unlock()
	}()

	code, err := m365.ParseCallback(callbackURL, state)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "MAILBOX_OAUTH_CALLBACK_INVALID", "invalid Microsoft mailbox callback")
	}
	form := url.Values{
		"client_id": {m365.MailboxClientID}, "grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {m365.RedirectURI}, "code_verifier": {verifier}, "scope": {m365.MailboxScope},
	}
	payload, status, err := s.request(ctx, form)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, infraerrors.Newf(http.StatusBadGateway, "MAILBOX_OAUTH_UPSTREAM_FAILED", "Microsoft mailbox token exchange failed (status %d)", status)
	}
	accessToken := mailboxOAuthJSONString(payload, "access_token")
	if accessToken == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "MAILBOX_OAUTH_TOKEN_INCOMPLETE", "Microsoft mailbox token exchange returned no access token")
	}
	identity, identityErr := m365.DecodeIdentity(accessToken)
	if identityErr != nil {
		if idToken := mailboxOAuthJSONString(payload, "id_token"); idToken != "" {
			identity, identityErr = m365.DecodeIdentity(idToken)
		}
	}
	if identityErr != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "MAILBOX_OAUTH_IDENTITY_MISSING", "Microsoft mailbox token did not include oid and tid")
	}
	expiresIn := mailboxOAuthJSONInt64(payload, "expires_in")
	if expiresIn <= 0 || expiresIn > int64((24*time.Hour)/time.Second) {
		expiresIn = int64(time.Hour / time.Second)
	}
	result := &MailboxOAuthTokenInfo{
		AccessToken: accessToken, RefreshToken: mailboxOAuthJSONString(payload, "refresh_token"),
		IDToken: mailboxOAuthJSONString(payload, "id_token"), ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second).Unix(),
		OID: identity.OID, TID: identity.TID, Email: identity.Email, Name: identity.Name,
	}
	session.mu.Lock()
	session.tokenInfo = result
	session.mu.Unlock()
	return result, nil
}

func (s *MailboxOAuthService) Consume(loginID string) { s.deleteSession(loginID) }

func (s *MailboxOAuthService) Cancel(loginID string) { s.deleteSession(loginID) }

func (s *MailboxOAuthService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.sessions = make(map[string]*mailboxOAuthSession)
	s.mu.Unlock()
}

func (s *MailboxOAuthService) session(loginID string) (*mailboxOAuthSession, error) {
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "MAILBOX_OAUTH_SESSION_REQUIRED", "loginId is required")
	}
	s.mu.Lock()
	s.cleanupLocked(time.Now())
	item := s.sessions[loginID]
	s.mu.Unlock()
	if item == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "MAILBOX_OAUTH_SESSION_NOT_FOUND", "mailbox OAuth session not found or expired")
	}
	return item, nil
}

func (s *MailboxOAuthService) deleteSession(loginID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.sessions, strings.TrimSpace(loginID))
	s.mu.Unlock()
}

func (s *MailboxOAuthService) cleanupLocked(now time.Time) {
	for loginID, session := range s.sessions {
		if !now.Before(session.expiresAt) {
			delete(s.sessions, loginID)
		}
	}
}

func (s *MailboxOAuthService) request(ctx context.Context, form url.Values) (map[string]any, int, error) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m365.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusInternalServerError, "MAILBOX_OAUTH_REQUEST_FAILED", "failed to create Microsoft mailbox request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, infraerrors.New(http.StatusBadGateway, "MAILBOX_OAUTH_UPSTREAM_UNAVAILABLE", "Microsoft OAuth upstream is unavailable").WithCause(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, mailboxOAuthBodyLimit))
	if err != nil {
		return nil, response.StatusCode, infraerrors.New(http.StatusBadGateway, "MAILBOX_OAUTH_RESPONSE_FAILED", "failed to read Microsoft mailbox response")
	}
	if len(body) == 0 {
		return map[string]any{}, response.StatusCode, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, response.StatusCode, infraerrors.New(http.StatusBadGateway, "MAILBOX_OAUTH_RESPONSE_INVALID", "Microsoft OAuth upstream returned invalid JSON")
	}
	return payload, response.StatusCode, nil
}

func mailboxOAuthJSONString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func mailboxOAuthJSONInt64(payload map[string]any, key string) int64 {
	value, _ := payload[key].(float64)
	return int64(value)
}
