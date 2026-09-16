package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/m365"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type m365HandlerRoundTripper struct{}

func (m365HandlerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"access_token":"` + m365HandlerJWT() + `","refresh_token":"refresh-secret","expires_in":3600}`)),
		Header:     make(http.Header), Request: req,
	}, nil
}

type m365FallbackRoundTripper struct{}

func (m365FallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"access_token":"` + m365HandlerJWT() + `","expires_in":3600}`)),
		Header:     make(http.Header), Request: req,
	}, nil
}

func m365HandlerJWT() string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{"oid": "oid-handler", "tid": "tenant-handler", "preferred_username": "m365@example.com", "name": "M365 Owner"})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestCompleteM365OAuthCompatPersistsAndSanitizes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminService := newStubAdminService()
	h := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h.m365OAuthService.SetHTTPClient(&http.Client{Transport: m365HandlerRoundTripper{}})
	started, err := h.m365OAuthService.Start(context.Background())
	require.NoError(t, err)
	parsed, err := url.Parse(started.AuthorizationURL)
	require.NoError(t, err)
	callback := m365.RedirectURI + "?code=auth-code&state=" + url.QueryEscape(parsed.Query().Get("state"))

	request := httptest.NewRequest(http.MethodPost, "/api/accounts/m365/oauth/complete", bytes.NewBufferString(`{"loginId":"`+started.LoginID+`","callbackUrl":"`+callback+`"}`))
	request.Header.Set("Content-Type", "application/json")
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = request
	h.CompleteM365OAuthCompat(ctx)

	require.Equal(t, http.StatusOK, writer.Code)
	require.Len(t, adminService.createdAccounts, 1)
	created := adminService.createdAccounts[0]
	require.Equal(t, "m365", created.Platform)
	require.Equal(t, "oid-handler", created.Credentials["oid"])
	require.Equal(t, "tenant-handler", created.Credentials["tid"])
	require.Equal(t, "refresh-secret", created.Credentials["refresh_token"])
	require.NotContains(t, writer.Body.String(), "refresh-secret")
}

func TestCompleteM365OAuthCompatKeepsExistingRefreshToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminService := newStubAdminService()
	adminService.accounts = []service.Account{{
		ID: 77, Platform: "m365", Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"oid": "oid-handler", "tid": "tenant-handler", "refresh_token": "old-refresh", "email": "m365@example.com"},
	}}
	h := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h.m365OAuthService.SetHTTPClient(&http.Client{Transport: m365FallbackRoundTripper{}})
	started, err := h.m365OAuthService.Start(context.Background())
	require.NoError(t, err)
	parsed, err := url.Parse(started.AuthorizationURL)
	require.NoError(t, err)
	callback := m365.RedirectURI + "?code=auth-code&state=" + url.QueryEscape(parsed.Query().Get("state"))
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/m365/oauth/complete", bytes.NewBufferString(`{"loginId":"`+started.LoginID+`","callbackUrl":"`+callback+`"}`))
	request.Header.Set("Content-Type", "application/json")
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = request
	h.CompleteM365OAuthCompat(ctx)
	require.Equal(t, http.StatusOK, writer.Code)
	require.Equal(t, 1, adminService.updateAccountCalls)
	require.Equal(t, "old-refresh", adminService.lastUpdateAccountInput.Credentials["refresh_token"])
}
