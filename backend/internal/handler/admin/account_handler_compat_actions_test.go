package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/tlsfingerprint"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type compatAccountRepo struct {
	service.AccountRepository
	account *service.Account
}

func (r *compatAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	if r.account != nil && r.account.ID == id {
		copy := *r.account
		return &copy, nil
	}
	return nil, service.ErrAccountNotFound
}

type compatQoderUpstream struct{}

func (compatQoderUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if req.URL.Host == "center.qoder.sh" {
		body, _ := json.Marshal(map[string]any{
			"id": "qoder-user", "securityOauthToken": "job-secret",
			"refreshToken": "refresh-secret", "expireTime": int64(4102444800000),
		})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	}
	inner := `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	wrapper, _ := json.Marshal(map[string]any{"body": inner})
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + string(wrapper) + "\n\n"))}, nil
}

func (u compatQoderUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func TestAccountCompatCheckAndWarmupReturnSanitizedState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAccountHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, action := range []string{"check", "warmup"} {
		t.Run(action, func(t *testing.T) {
			router := gin.New()
			if action == "check" {
				router.POST("/api/accounts/:id/check", h.CheckCompat)
			} else {
				router.POST("/api/accounts/:id/warmup", h.WarmupCompat)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/accounts/41/"+action, nil)
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			require.Equal(t, http.StatusOK, resp.Code)
			require.NotContains(t, resp.Body.String(), "access_token")
		})
	}
}

func TestAccountCompatQoderWarmupRequiresSuccessfulTerminalEvent(t *testing.T) {
	account := &service.Account{
		ID: 51, Name: "qoder", Platform: service.PlatformQoder, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"token": "pat-secret"},
	}
	repo := &compatAccountRepo{account: account}
	upstream := compatQoderUpstream{}
	qoderGateway := service.NewQoderGatewayService(service.NewQoderTokenProvider(nil, upstream), upstream)
	accountTest := service.NewAccountTestService(repo, nil, nil, nil, nil, nil, upstream, nil, nil)
	accountTest.SetQoderGatewayService(qoderGateway)
	adminService := newStubAdminService()
	adminService.getAccountResult = account
	h := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, accountTest, nil, nil, nil, nil, nil)

	router := gin.New()
	router.POST("/api/accounts/:id/warmup", h.WarmupCompat)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/51/warmup", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	require.Equal(t, http.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"ok":true`)
	require.Contains(t, resp.Body.String(), `"usage":null`)
	require.NotContains(t, resp.Body.String(), "pat-secret")
	require.NotContains(t, resp.Body.String(), "job-secret")
	require.NotContains(t, resp.Body.String(), "refresh-secret")
}

func TestAccountProbeSucceededRejectsMissingOrForgedCompletion(t *testing.T) {
	require.False(t, accountProbeSucceeded(nil))
	require.False(t, accountProbeSucceeded([]byte("data: {\"type\":\"test_complete\",\"success\":false}\n\n")))
	require.False(t, accountProbeSucceeded([]byte("data: {\"type\":\"error\",\"error\":\"{\\\"type\\\":\\\"test_complete\\\",\\\"success\\\":true}\"}\n\n")))
	require.True(t, accountProbeSucceeded([]byte("data:{\"type\":\"test_complete\",\"success\":true}\n\n")))
}
