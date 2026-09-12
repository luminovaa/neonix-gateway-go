package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type grokDeviceHandlerRoundTripper struct {
	responses []struct {
		status int
		body   string
	}
	index int
}

func (r *grokDeviceHandlerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	_ = req
	response := r.responses[r.index]
	r.index++
	return &http.Response{
		StatusCode: response.status,
		Body:       io.NopCloser(strings.NewReader(response.body)),
		Header:     make(http.Header),
	}, nil
}

func grokHandlerTestIDToken() string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{"sub": "handler-sub", "email": "handler@example.com", "name": "Handler owner"})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestPollGrokOAuthCompatPersistsAndSanitizesDeviceLogin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	transport := &grokDeviceHandlerRoundTripper{responses: []struct {
		status int
		body   string
	}{
		{status: http.StatusOK, body: `{"device_code":"device","user_code":"CODE","verification_uri":"https://auth.x.ai/oauth2/device","interval":1}`},
		{status: http.StatusOK, body: `{"access_token":"access-secret","refresh_token":"refresh-secret","id_token":"` + grokHandlerTestIDToken() + `","expires_in":3600}`},
	}}
	grokOAuth := service.NewGrokOAuthService(nil, nil)
	grokOAuth.SetGrokDeviceHTTPClient(&http.Client{Transport: transport})
	adminService := newStubAdminService()
	h := NewAccountHandler(adminService, nil, nil, nil, nil, grokOAuth, nil, nil, nil, nil, nil, nil, nil, nil)

	started, err := grokOAuth.StartGrokDevice(context.Background())
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/grok/oauth/poll", bytes.NewBufferString(`{"loginId":"`+started.LoginID+`"}`))
	request.Header.Set("Content-Type", "application/json")
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = request
	h.PollGrokOAuthCompat(ctx)

	require.Equal(t, http.StatusOK, writer.Code)
	require.Len(t, adminService.createdAccounts, 1)
	createdInput := adminService.createdAccounts[0]
	require.Equal(t, service.PlatformGrok, createdInput.Platform)
	require.Equal(t, service.AccountTypeOAuth, createdInput.Type)
	require.Equal(t, "access-secret", createdInput.Credentials["access_token"])
	require.Equal(t, "refresh-secret", createdInput.Credentials["refresh_token"])
	require.NotContains(t, writer.Body.String(), "access-secret")
	require.NotContains(t, writer.Body.String(), "refresh-secret")
	var response map[string]any
	require.NoError(t, json.Unmarshal(writer.Body.Bytes(), &response))
	data, ok := response["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "complete", data["status"])
}
