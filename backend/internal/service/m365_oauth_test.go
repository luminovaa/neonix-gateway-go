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

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/m365"
	"github.com/stretchr/testify/require"
)

type m365OAuthRoundTripper struct {
	status int
	body   string
}

func (r m365OAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func m365TestJWT() string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"oid": "oid-1", "tid": "tenant-1", "preferred_username": "owner@example.com", "name": "Owner",
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestM365OAuthStartAndCompletePKCE(t *testing.T) {
	service := NewM365OAuthService()
	service.SetHTTPClient(&http.Client{Transport: m365OAuthRoundTripper{
		status: http.StatusOK,
		body:   `{"access_token":"` + m365TestJWT() + `","refresh_token":"refresh-secret","id_token":"` + m365TestJWT() + `","expires_in":3600,"scope":"openid"}`,
	}})
	started, err := service.Start(context.Background())
	require.NoError(t, err)
	require.Equal(t, m365.RedirectURI, started.RedirectURI)
	parsed, err := url.Parse(started.AuthorizationURL)
	require.NoError(t, err)
	require.Equal(t, m365.ClientID, parsed.Query().Get("client_id"))
	require.NotEmpty(t, parsed.Query().Get("state"))
	require.Equal(t, "S256", parsed.Query().Get("code_challenge_method"))

	_, err = service.Complete(context.Background(), started.LoginID, m365.RedirectURI+"?code=wrong&state=wrong")
	require.Error(t, err)
	state := parsed.Query().Get("state")
	tokenInfo, err := service.Complete(context.Background(), started.LoginID, m365.RedirectURI+"?code=auth-code&state="+url.QueryEscape(state))
	require.NoError(t, err)
	require.Equal(t, "oid-1", tokenInfo.OID)
	require.Equal(t, "tenant-1", tokenInfo.TID)
	require.Equal(t, "owner@example.com", tokenInfo.Email)
	require.Equal(t, "refresh-secret", tokenInfo.RefreshToken)

	// Persistence can retry completion without another upstream token request.
	replay, err := service.Complete(context.Background(), started.LoginID, m365.RedirectURI+"?code=auth-code&state="+url.QueryEscape(state))
	require.NoError(t, err)
	require.Equal(t, tokenInfo, replay)
	service.Consume(started.LoginID)
	_, err = service.Complete(context.Background(), started.LoginID, "garbage")
	require.Error(t, err)
}

func TestM365OAuthCompleteRejectsMissingIdentity(t *testing.T) {
	service := NewM365OAuthService()
	service.SetHTTPClient(&http.Client{Transport: m365OAuthRoundTripper{
		status: http.StatusOK,
		body:   `{"access_token":"opaque","refresh_token":"refresh"}`,
	}})
	started, err := service.Start(context.Background())
	require.NoError(t, err)
	parsed, err := url.Parse(started.AuthorizationURL)
	require.NoError(t, err)
	_, err = service.Complete(context.Background(), started.LoginID, m365.RedirectURI+"?code=auth&state="+url.QueryEscape(parsed.Query().Get("state")))
	require.Error(t, err)
	require.Contains(t, err.Error(), "M365_OAUTH_IDENTITY_MISSING")
}
