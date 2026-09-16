package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type codexDeviceOAuthClientStub struct {
	mu       sync.Mutex
	code     string
	verifier string
	calls    int
}

func (s *codexDeviceOAuthClientStub) ExchangeCode(_ context.Context, code, verifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	s.mu.Lock()
	s.calls++
	s.code, s.verifier = code, verifier
	s.mu.Unlock()
	if redirectURI != openai.DeviceRedirectURI || proxyURL != "" || clientID != openai.ClientID {
		return nil, context.Canceled
	}
	return &openai.TokenResponse{
		AccessToken: "access-secret", RefreshToken: "refresh-secret", IDToken: codexTestIDToken(), ExpiresIn: 3600,
	}, nil
}

func (s *codexDeviceOAuthClientStub) RefreshToken(context.Context, string, string) (*openai.TokenResponse, error) {
	return nil, context.Canceled
}

func (s *codexDeviceOAuthClientStub) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	return nil, context.Canceled
}

type codexDeviceHTTPResponse struct {
	status int
	body   string
}

type codexDeviceRoundTripper struct {
	mu         sync.Mutex
	responses  []codexDeviceHTTPResponse
	requests   []*http.Request
	started    chan struct{}
	release    chan struct{}
	blockAfter int
}

func (r *codexDeviceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	index := len(r.requests)
	r.requests = append(r.requests, req.Clone(req.Context()))
	response := r.responses[index]
	started, release := r.started, r.release
	block := r.blockAfter > 0 && index == r.blockAfter
	r.mu.Unlock()
	if block {
		close(started)
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return &http.Response{
		StatusCode: response.status,
		Status:     http.StatusText(response.status),
		Body:       io.NopCloser(strings.NewReader(response.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func codexTestIDToken() string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"email": "owner@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "chatgpt-account-1",
			"chatgpt_user_id":    "chatgpt-user-1",
		},
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestOpenAIOAuthServiceCodexDeviceFlowPendingExchangeAndConsume(t *testing.T) {
	clientStub := &codexDeviceOAuthClientStub{}
	transport := &codexDeviceRoundTripper{responses: []codexDeviceHTTPResponse{
		{status: http.StatusOK, body: `{"device_auth_id":"device-1","user_code":"ABCD-EFGH","interval":2}`},
		{status: http.StatusForbidden, body: `{}`},
		{status: http.StatusOK, body: `{"authorization_code":"auth-code","code_verifier":"verifier","code_challenge":"challenge"}`},
	}}
	service := NewOpenAIOAuthService(nil, clientStub)
	service.SetCodexDeviceHTTPClient(&http.Client{Transport: transport})

	started, err := service.StartCodexDevice(context.Background())
	require.NoError(t, err)
	require.Equal(t, openai.DeviceVerifyURL, started.VerificationURL)
	require.Equal(t, "ABCD-EFGH", started.UserCode)
	require.Equal(t, 2, started.Interval)
	require.NotEmpty(t, started.LoginID)
	require.Greater(t, started.ExpiresAt, time.Now().UnixMilli())

	pending, err := service.PollCodexDevice(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.True(t, pending.Pending)
	require.Equal(t, 2, pending.RetryAfter)

	complete, err := service.PollCodexDevice(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.False(t, complete.Pending)
	require.NotNil(t, complete.TokenInfo)
	require.Equal(t, "owner@example.com", complete.TokenInfo.Email)
	require.Equal(t, "chatgpt-account-1", complete.TokenInfo.ChatGPTAccountID)
	clientStub.mu.Lock()
	require.Equal(t, 1, clientStub.calls)
	require.Equal(t, "auth-code", clientStub.code)
	require.Equal(t, "verifier", clientStub.verifier)
	clientStub.mu.Unlock()

	// A second poll after exchange returns the in-memory result and does not
	// contact OpenAI again; the handler can persist it before consuming the session.
	replay, err := service.PollCodexDevice(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.Equal(t, complete.TokenInfo, replay.TokenInfo)
	transport.mu.Lock()
	require.Len(t, transport.requests, 3)
	transport.mu.Unlock()

	service.ConsumeCodexDevice(started.LoginID)
	_, err = service.PollCodexDevice(context.Background(), started.LoginID)
	require.Error(t, err)
}

func TestOpenAIOAuthServiceCodexDevicePollIsSingleFlight(t *testing.T) {
	clientStub := &codexDeviceOAuthClientStub{}
	startedSignal := make(chan struct{})
	release := make(chan struct{})
	transport := &codexDeviceRoundTripper{
		responses: []codexDeviceHTTPResponse{
			{status: http.StatusOK, body: `{"device_auth_id":"device-1","user_code":"ABCD","interval":1}`},
			{status: http.StatusForbidden, body: `{}`},
		},
		started: startedSignal, release: release, blockAfter: 1,
	}
	service := NewOpenAIOAuthService(nil, clientStub)
	service.SetCodexDeviceHTTPClient(&http.Client{Transport: transport})
	started, err := service.StartCodexDevice(context.Background())
	require.NoError(t, err)

	firstDone := make(chan error, 1)
	go func() {
		_, pollErr := service.PollCodexDevice(context.Background(), started.LoginID)
		firstDone <- pollErr
	}()
	<-startedSignal
	second, err := service.PollCodexDevice(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.True(t, second.Pending)
	close(release)
	require.NoError(t, <-firstDone)
	transport.mu.Lock()
	require.Len(t, transport.requests, 2)
	transport.mu.Unlock()
}

func TestOpenAIOAuthServiceCodexDeviceStartRejectsIncompleteResponse(t *testing.T) {
	transport := &codexDeviceRoundTripper{responses: []codexDeviceHTTPResponse{{status: http.StatusOK, body: `{"user_code":"only-code"}`}}}
	service := NewOpenAIOAuthService(nil, &codexDeviceOAuthClientStub{})
	service.SetCodexDeviceHTTPClient(&http.Client{Transport: transport})
	_, err := service.StartCodexDevice(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "OPENAI_CODEX_DEVICE_RESPONSE_INCOMPLETE")
}
