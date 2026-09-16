package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

type kiroOAuthRoundTripper struct{ requests []*http.Request }

func (r *kiroOAuthRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	r.requests = append(r.requests, request)
	body := "{\"clientId\":\"client-id\",\"clientSecret\":\"client-secret\"}"
	if request.URL.String() == kiro.TokenURL {
		body = "{\"accessToken\":\"" + kiroOAuthJWT() + "\",\"refreshToken\":\"refresh-secret\",\"expiresIn\":3600}"
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
}

func kiroOAuthJWT() string {
	header, _ := json.Marshal(map[string]string{"alg": "none"})
	payload, _ := json.Marshal(map[string]string{"sub": "google-subject", "email": "owner@example.com", "name": "Owner"})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestKiroOAuthGoogleTwoStagePKCE(t *testing.T) {
	transport := &kiroOAuthRoundTripper{}
	service := NewKiroOAuthService()
	service.SetHTTPClient(&http.Client{Transport: transport})
	started, err := service.Start(context.Background())
	require.NoError(t, err)
	require.Equal(t, kiro.RedirectURI, started.RedirectURI)
	signIn, err := url.Parse(started.AuthorizationURL)
	require.NoError(t, err)
	require.Equal(t, "S256", signIn.Query().Get("code_challenge_method"))
	state := signIn.Query().Get("state")

	identityCallback := kiro.RedirectURI + "/signin/callback?state=" + url.QueryEscape(state) + "&login_option=Google"
	identity, err := service.Complete(context.Background(), started.LoginID, identityCallback)
	require.NoError(t, err)
	require.Equal(t, "authorize", identity.Status)
	authorize, err := url.Parse(identity.AuthorizationURL)
	require.NoError(t, err)
	require.Equal(t, "client-id", authorize.Query().Get("client_id"))
	require.Equal(t, state, authorize.Query().Get("state"))
	require.Equal(t, signIn.Query().Get("code_challenge"), authorize.Query().Get("code_challenge"))

	completed, err := service.Complete(context.Background(), started.LoginID, kiro.RedirectURI+"?state="+url.QueryEscape(state)+"&code=authorization-code")
	require.NoError(t, err)
	require.Equal(t, "complete", completed.Status)
	require.Equal(t, "refresh-secret", completed.TokenInfo.RefreshToken)
	require.Equal(t, "google-subject", completed.TokenInfo.Subject)
	require.Equal(t, "owner@example.com", completed.TokenInfo.Email)
	require.Len(t, transport.requests, 2)
}

func TestKiroOAuthRejectsWrongOriginAndState(t *testing.T) {
	service := NewKiroOAuthService()
	service.SetHTTPClient(&http.Client{Transport: &kiroOAuthRoundTripper{}})
	started, err := service.Start(context.Background())
	require.NoError(t, err)
	parsed, _ := url.Parse(started.AuthorizationURL)
	state := parsed.Query().Get("state")
	_, err = service.Complete(context.Background(), started.LoginID, "http://evil.example:3128/?state="+state+"&login_option=Google")
	require.Error(t, err)
	_, err = service.Complete(context.Background(), started.LoginID, kiro.RedirectURI+"/?state=wrong&login_option=Google")
	require.Error(t, err)
}

func TestKiroOAuthAcceptsDirectAuthorizationCallback(t *testing.T) {
	service := NewKiroOAuthService()
	service.SetHTTPClient(&http.Client{Transport: &kiroOAuthRoundTripper{}})
	started, err := service.Start(context.Background())
	require.NoError(t, err)
	parsed, _ := url.Parse(started.AuthorizationURL)
	completed, err := service.Complete(context.Background(), started.LoginID, kiro.RedirectURI+"/?state="+url.QueryEscape(parsed.Query().Get("state"))+"&code=direct-code")
	require.NoError(t, err)
	require.Equal(t, "complete", completed.Status)
}
