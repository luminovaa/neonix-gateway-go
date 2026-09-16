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
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/kiro"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type kiroHandlerRoundTripper struct{}

func (kiroHandlerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	body := "{\"clientId\":\"client-id\",\"clientSecret\":\"client-secret\"}"
	if request.URL.String() == kiro.TokenURL {
		header := base64.RawURLEncoding.EncodeToString([]byte("{\"alg\":\"none\"}"))
		payload := base64.RawURLEncoding.EncodeToString([]byte("{\"sub\":\"google-subject\",\"email\":\"owner@example.com\"}"))
		body = "{\"accessToken\":\"" + header + "." + payload + ".signature\",\"refreshToken\":\"refresh-secret\",\"expiresIn\":3600}"
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
}

func TestCompleteKiroOAuthPersistsGoogleCredentialAndSanitizes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminService := newStubAdminService()
	h := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h.kiroOAuthService.SetHTTPClient(&http.Client{Transport: kiroHandlerRoundTripper{}})
	started, err := h.kiroOAuthService.Start(context.Background())
	require.NoError(t, err)
	parsed, _ := url.Parse(started.AuthorizationURL)
	state := parsed.Query().Get("state")

	first := callKiroComplete(t, h, started.LoginID, kiro.RedirectURI+"/signin/callback?state="+state+"&login_option=Google")
	require.Equal(t, http.StatusOK, first.Code)
	second := callKiroComplete(t, h, started.LoginID, kiro.RedirectURI+"?state="+state+"&code=auth-code")
	require.Equal(t, http.StatusOK, second.Code)
	require.Len(t, adminService.createdAccounts, 1)
	created := adminService.createdAccounts[0]
	require.Equal(t, service.PlatformKiro, created.Platform)
	require.Equal(t, service.AccountTypeOAuth, created.Type)
	require.Equal(t, "Google", created.Credentials["provider"])
	require.Equal(t, kiro.SocialProfileARN, created.Credentials["profile_arn"])
	require.NotContains(t, second.Body.String(), "refresh-secret")
	require.NotContains(t, second.Body.String(), "client-secret")
}

func callKiroComplete(t *testing.T, h *AccountHandler, loginID, callback string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"loginId": loginID, "callbackUrl": callback})
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/kiro/oauth/complete", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = request
	h.CompleteKiroOAuthCompat(ctx)
	return writer
}
