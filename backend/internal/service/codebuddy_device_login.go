package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddy"
	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
)

const (
	codeBuddyDeviceSessionTTL = 15 * time.Minute
	codeBuddyDeviceInterval   = 3
)

type codeBuddyDeviceSession struct {
	mu sync.Mutex

	loginID   string
	state     string
	domain    string
	authURL   string
	expiresAt time.Time
	polling   bool
	claimed   bool
	cancelled bool
	result    *CodeBuddyDeviceTokenResult
}

type CodeBuddyDeviceLoginService struct {
	httpUpstream HTTPUpstream
	now          func() time.Time
	mu           sync.Mutex
	sessions     map[string]*codeBuddyDeviceSession
}

func NewCodeBuddyDeviceLoginService(upstream HTTPUpstream) *CodeBuddyDeviceLoginService {
	return &CodeBuddyDeviceLoginService{httpUpstream: upstream, now: time.Now, sessions: make(map[string]*codeBuddyDeviceSession)}
}

type CodeBuddyDeviceStartResult struct {
	LoginID          string `json:"loginId"`
	State            string `json:"state"`
	AuthorizationURL string `json:"authorizationUrl"`
	AuthURL          string `json:"authUrl"`
	Interval         int    `json:"interval"`
	ExpiresAt        int64  `json:"expiresAt"`
}

type CodeBuddyDeviceTokenResult struct {
	Tokens      codebuddy.Tokens
	AccountInfo codebuddy.AccountInfo
}

type CodeBuddyDevicePollResult struct {
	Pending    bool
	RetryAfter int
	ExpiresAt  int64
	Result     *CodeBuddyDeviceTokenResult
}

func (s *CodeBuddyDeviceLoginService) Start(ctx context.Context, domain string) (*CodeBuddyDeviceStartResult, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "CODEBUDDY_DEVICE_UNAVAILABLE", "CodeBuddy device login is unavailable")
	}
	s.cleanup(s.now())
	hosts := codebuddy.ResolveHosts(domain)
	if codebuddy.IsChinaRealm(domain) {
		return nil, infraerrors.New(http.StatusBadRequest, "CODEBUDDY_CHINA_DEVICE_UNSUPPORTED", "CodeBuddy China device login is not available in the CodeBuddy .ai flow")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codebuddy.DeviceStartURL(hosts), bytes.NewBufferString("{}"))
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "CODEBUDDY_DEVICE_START_FAILED", "failed to create CodeBuddy device request")
	}
	req.Header = codebuddy.CommonHeaders(hosts)
	resp, err := s.httpUpstream.Do(req, "", 0, 1)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "CODEBUDDY_DEVICE_UPSTREAM_UNAVAILABLE", "CodeBuddy login service is unavailable").WithCause(err)
	}
	body, status, err := readCodeBuddyResponse(resp)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, infraerrors.New(http.StatusBadGateway, "CODEBUDDY_DEVICE_START_FAILED", "CodeBuddy rejected the device login request")
	}
	envelope, err := codebuddy.ParseEnvelope(body)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "CODEBUDDY_DEVICE_START_FAILED", "CodeBuddy rejected the device login request").WithCause(err)
	}
	var data struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if json.Unmarshal(envelope.Data, &data) != nil || strings.TrimSpace(data.State) == "" || !validCodeBuddyAuthURL(data.AuthURL, hosts) {
		return nil, infraerrors.New(http.StatusBadGateway, "CODEBUDDY_DEVICE_RESPONSE_INCOMPLETE", "CodeBuddy returned an incomplete device login response")
	}
	loginID, err := codeBuddyLoginID()
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "CODEBUDDY_DEVICE_SESSION_FAILED", "failed to create CodeBuddy login session")
	}
	expiresAt := s.now().Add(codeBuddyDeviceSessionTTL)
	session := &codeBuddyDeviceSession{loginID: loginID, state: data.State, domain: domain, authURL: data.AuthURL, expiresAt: expiresAt}
	s.mu.Lock()
	s.sessions[loginID] = session
	s.mu.Unlock()
	// state is a compatibility alias for the previous Node endpoint. It is the
	// opaque local login ID, never the upstream state.
	return &CodeBuddyDeviceStartResult{LoginID: loginID, State: loginID, AuthorizationURL: data.AuthURL, AuthURL: data.AuthURL, Interval: codeBuddyDeviceInterval, ExpiresAt: expiresAt.UnixMilli()}, nil
}

func (s *CodeBuddyDeviceLoginService) Poll(ctx context.Context, loginID string) (*CodeBuddyDevicePollResult, error) {
	session, err := s.session(loginID)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	if session.cancelled {
		session.mu.Unlock()
		return nil, infraerrors.New(http.StatusGone, "CODEBUDDY_DEVICE_SESSION_CANCELLED", "CodeBuddy login was cancelled")
	}
	if !s.now().Before(session.expiresAt) {
		session.mu.Unlock()
		s.Cancel(loginID)
		return nil, infraerrors.New(http.StatusGone, "CODEBUDDY_DEVICE_SESSION_EXPIRED", "CodeBuddy login expired; restart login")
	}
	if session.result != nil {
		if session.claimed {
			session.mu.Unlock()
			return nil, infraerrors.New(http.StatusConflict, "CODEBUDDY_DEVICE_SESSION_IN_USE", "CodeBuddy login completion is already being processed")
		}
		session.claimed = true
		result := session.result
		expiresAt := session.expiresAt.UnixMilli()
		session.mu.Unlock()
		return &CodeBuddyDevicePollResult{ExpiresAt: expiresAt, Result: result}, nil
	}
	if session.polling {
		expiresAt := session.expiresAt.UnixMilli()
		session.mu.Unlock()
		return &CodeBuddyDevicePollResult{Pending: true, RetryAfter: codeBuddyDeviceInterval, ExpiresAt: expiresAt}, nil
	}
	session.polling = true
	state, domain, expiresAt := session.state, session.domain, session.expiresAt
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.polling = false
		session.mu.Unlock()
	}()

	hosts := codebuddy.ResolveHosts(domain)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, codebuddy.DevicePollURL(hosts, state), nil)
	req.Header = codebuddy.CommonHeaders(hosts)
	resp, err := s.httpUpstream.Do(req, "", 0, 1)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "CODEBUDDY_DEVICE_POLL_FAILED", "CodeBuddy login service is unavailable").WithCause(err)
	}
	body, status, err := readCodeBuddyResponse(resp)
	if err != nil {
		return nil, err
	}
	if status >= 500 || status == 0 {
		return nil, infraerrors.New(http.StatusBadGateway, "CODEBUDDY_DEVICE_POLL_FAILED", "CodeBuddy login service is unavailable")
	}
	envelope, envelopeErr := codebuddy.ParseEnvelope(body)
	if envelopeErr != nil {
		// CodeBuddy uses non-zero business codes for the ordinary pending state.
		return &CodeBuddyDevicePollResult{Pending: true, RetryAfter: codeBuddyDeviceInterval, ExpiresAt: expiresAt.UnixMilli()}, nil
	}
	tokens, tokenErr := codebuddy.ParseTokens(envelope.Data)
	if tokenErr != nil {
		return &CodeBuddyDevicePollResult{Pending: true, RetryAfter: codeBuddyDeviceInterval, ExpiresAt: expiresAt.UnixMilli()}, nil
	}
	if tokens.Domain == "" {
		tokens.Domain = domain
	}
	info := s.lookupAccount(ctx, hosts, state, tokens.AccessToken)
	result := &CodeBuddyDeviceTokenResult{Tokens: tokens, AccountInfo: info}
	session.mu.Lock()
	if session.cancelled {
		session.mu.Unlock()
		return nil, infraerrors.New(http.StatusGone, "CODEBUDDY_DEVICE_SESSION_CANCELLED", "CodeBuddy login was cancelled")
	}
	session.result = result
	session.claimed = true
	session.mu.Unlock()
	return &CodeBuddyDevicePollResult{ExpiresAt: expiresAt.UnixMilli(), Result: result}, nil
}

func (s *CodeBuddyDeviceLoginService) lookupAccount(ctx context.Context, hosts codebuddy.Hosts, state, accessToken string) codebuddy.AccountInfo {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, codebuddy.DeviceAccountURL(hosts, state), nil)
	req.Header = codebuddy.CommonHeaders(hosts)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := s.httpUpstream.Do(req, "", 0, 1)
	if err != nil {
		return codebuddy.AccountInfo{}
	}
	body, status, err := readCodeBuddyResponse(resp)
	if err != nil || status < 200 || status >= 300 {
		return codebuddy.AccountInfo{}
	}
	envelope, err := codebuddy.ParseEnvelope(body)
	if err != nil {
		return codebuddy.AccountInfo{}
	}
	return codebuddy.ParseAccountInfo(envelope.Data)
}

func (s *CodeBuddyDeviceLoginService) Consume(loginID string) { s.Cancel(loginID) }

// Release makes a completed token bundle claimable again after persistence
// failed. Tokens remain server-side and the session keeps its original expiry.
func (s *CodeBuddyDeviceLoginService) Release(loginID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	session := s.sessions[strings.TrimSpace(loginID)]
	s.mu.Unlock()
	if session == nil {
		return
	}
	session.mu.Lock()
	if session.result != nil {
		session.claimed = false
	}
	session.mu.Unlock()
}

func (s *CodeBuddyDeviceLoginService) Cancel(loginID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	loginID = strings.TrimSpace(loginID)
	session := s.sessions[loginID]
	delete(s.sessions, loginID)
	s.mu.Unlock()
	if session != nil {
		session.mu.Lock()
		session.cancelled = true
		session.result = nil
		session.claimed = false
		session.mu.Unlock()
	}
}

func (s *CodeBuddyDeviceLoginService) session(loginID string) (*codeBuddyDeviceSession, error) {
	if s == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "CODEBUDDY_DEVICE_UNAVAILABLE", "CodeBuddy device login is unavailable")
	}
	loginID = strings.TrimSpace(loginID)
	if loginID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "CODEBUDDY_DEVICE_SESSION_REQUIRED", "loginId is required")
	}
	s.mu.Lock()
	session := s.sessions[loginID]
	s.mu.Unlock()
	if session == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "CODEBUDDY_DEVICE_SESSION_NOT_FOUND", "CodeBuddy login was not found or expired")
	}
	return session, nil
}

func (s *CodeBuddyDeviceLoginService) cleanup(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, session := range s.sessions {
		if !now.Before(session.expiresAt) {
			delete(s.sessions, id)
		}
	}
}

func readCodeBuddyResponse(resp *http.Response) ([]byte, int, error) {
	if resp == nil {
		return nil, 0, infraerrors.New(http.StatusBadGateway, "CODEBUDDY_RESPONSE_INVALID", "CodeBuddy returned no response")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, codeBuddyResponseBodyMax+1))
	if err != nil {
		return nil, resp.StatusCode, infraerrors.New(http.StatusBadGateway, "CODEBUDDY_RESPONSE_INVALID", "CodeBuddy response could not be read")
	}
	if len(body) > codeBuddyResponseBodyMax {
		return nil, resp.StatusCode, infraerrors.New(http.StatusBadGateway, "CODEBUDDY_RESPONSE_TOO_LARGE", "CodeBuddy response exceeded the allowed size")
	}
	return body, resp.StatusCode, nil
}

func codeBuddyLoginID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func validCodeBuddyAuthURL(raw string, hosts codebuddy.Hosts) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	switch hosts {
	case codebuddy.WorkBuddyHosts:
		return host == "workbuddy.ai" || strings.HasSuffix(host, ".workbuddy.ai")
	case codebuddy.AIHosts:
		return host == "codebuddy.ai" || strings.HasSuffix(host, ".codebuddy.ai")
	default:
		return false
	}
}
