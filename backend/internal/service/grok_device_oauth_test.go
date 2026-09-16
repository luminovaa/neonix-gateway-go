package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type grokDeviceHTTPResponse struct {
	status int
	body   string
}

type grokDeviceRoundTripper struct {
	mu         sync.Mutex
	responses  []grokDeviceHTTPResponse
	requests   []*http.Request
	bodies     []string
	started    chan struct{}
	release    chan struct{}
	blockAfter int
}

func (r *grokDeviceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	index := len(r.requests)
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	r.requests = append(r.requests, req.Clone(req.Context()))
	r.bodies = append(r.bodies, string(body))
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

func grokTestJWT() string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"sub":   "grok-sub-1",
		"email": "grok@example.com",
		"name":  "Grok Owner",
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestGrokDeviceFlowPendingSlowDownApprovalAndReplay(t *testing.T) {
	transport := &grokDeviceRoundTripper{responses: []grokDeviceHTTPResponse{
		{status: http.StatusOK, body: `{"device_code":"device-1","user_code":"GROK-CODE","verification_uri_complete":"https://auth.x.ai/oauth2/device/complete","interval":2,"expires_in":120}`},
		{status: http.StatusBadRequest, body: `{"error":"authorization_pending"}`},
		{status: http.StatusBadRequest, body: `{"error":"slow_down"}`},
		{status: http.StatusOK, body: `{"access_token":"` + grokTestJWT() + `","refresh_token":"refresh-secret","id_token":"` + grokTestJWT() + `","expires_in":3600,"token_type":"Bearer","scope":"openid"}`},
	}}
	service := NewGrokOAuthService(nil, nil)
	service.SetGrokDeviceHTTPClient(&http.Client{Transport: transport})

	started, err := service.StartGrokDevice(context.Background())
	require.NoError(t, err)
	require.Equal(t, "GROK-CODE", started.UserCode)
	require.Equal(t, "https://auth.x.ai/oauth2/device/complete", started.VerificationURL)
	require.Equal(t, 2, started.Interval)
	require.NotEmpty(t, started.LoginID)

	pending, err := service.PollGrokDevice(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.True(t, pending.Pending)
	require.Equal(t, 2, pending.RetryAfter)

	slow, err := service.PollGrokDevice(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.True(t, slow.Pending)
	require.Equal(t, 7, slow.RetryAfter)

	complete, err := service.PollGrokDevice(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.NotNil(t, complete.TokenInfo)
	require.Equal(t, "grok-sub-1", complete.TokenInfo.Subject)
	require.Equal(t, "grok@example.com", complete.TokenInfo.Email)
	require.NotEmpty(t, complete.TokenInfo.RefreshToken)

	replay, err := service.PollGrokDevice(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.Equal(t, complete.TokenInfo, replay.TokenInfo)
	transport.mu.Lock()
	require.Len(t, transport.requests, 4)
	for _, body := range transport.bodies[1:] {
		form, parseErr := url.ParseQuery(body)
		require.NoError(t, parseErr)
		require.NotEmpty(t, form.Get("device_code"))
	}
	transport.mu.Unlock()

	service.ConsumeGrokDevice(started.LoginID)
	_, err = service.PollGrokDevice(context.Background(), started.LoginID)
	require.Error(t, err)
}

func TestGrokDeviceFlowSingleFlightAndDenied(t *testing.T) {
	startedSignal := make(chan struct{})
	release := make(chan struct{})
	transport := &grokDeviceRoundTripper{
		responses: []grokDeviceHTTPResponse{
			{status: http.StatusOK, body: `{"device_code":"device-1","user_code":"CODE","verification_uri":"https://auth.x.ai/oauth2/device","interval":1}`},
			{status: http.StatusBadRequest, body: `{"error":"access_denied"}`},
		},
		started: startedSignal, release: release, blockAfter: 1,
	}
	service := NewGrokOAuthService(nil, nil)
	service.SetGrokDeviceHTTPClient(&http.Client{Transport: transport})
	started, err := service.StartGrokDevice(context.Background())
	require.NoError(t, err)

	firstDone := make(chan error, 1)
	go func() {
		_, pollErr := service.PollGrokDevice(context.Background(), started.LoginID)
		firstDone <- pollErr
	}()
	<-startedSignal
	second, err := service.PollGrokDevice(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.True(t, second.Pending)
	close(release)
	require.Error(t, <-firstDone)
	transport.mu.Lock()
	require.Len(t, transport.requests, 2)
	transport.mu.Unlock()
}

func TestGrokDeviceFlowRejectsIncompleteStart(t *testing.T) {
	transport := &grokDeviceRoundTripper{responses: []grokDeviceHTTPResponse{{status: http.StatusOK, body: `{"device_code":"only"}`}}}
	service := NewGrokOAuthService(nil, nil)
	service.SetGrokDeviceHTTPClient(&http.Client{Transport: transport})
	_, err := service.StartGrokDevice(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "GROK_DEVICE_RESPONSE_INCOMPLETE")
}
