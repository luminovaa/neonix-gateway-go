package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddy"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/tlsfingerprint"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type codeBuddyHandlerUpstream struct {
	responses []*http.Response
}

func (u *codeBuddyHandlerUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	response := u.responses[0]
	u.responses = u.responses[1:]
	return response, nil
}

func (u *codeBuddyHandlerUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func codeBuddyHandlerResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func startCompletedCodeBuddyHandlerSession(t *testing.T, login *service.CodeBuddyDeviceLoginService) string {
	t.Helper()
	started, err := login.Start(context.Background(), "")
	require.NoError(t, err)
	return started.LoginID
}

type codeBuddyMemoryAdminService struct {
	*stubAdminService
	nextID int64
}

func newCodeBuddyMemoryAdminService(accounts []service.Account) *codeBuddyMemoryAdminService {
	stub := newStubAdminService()
	stub.accounts = append([]service.Account(nil), accounts...)
	stub.accountSchedulerScoreFilterAccounts = nil
	return &codeBuddyMemoryAdminService{stubAdminService: stub, nextID: 900}
}

func (s *codeBuddyMemoryAdminService) CreateAccount(_ context.Context, input *service.CreateAccountInput) (*service.Account, error) {
	s.createdAccounts = append(s.createdAccounts, input)
	account := service.Account{ID: s.nextID, Name: input.Name, Platform: input.Platform, Type: input.Type, Credentials: cloneCodeBuddyHandlerMap(input.Credentials), Extra: cloneCodeBuddyHandlerMap(input.Extra), Status: service.StatusActive, Schedulable: true}
	s.nextID++
	s.accounts = append(s.accounts, account)
	return &account, nil
}

func (s *codeBuddyMemoryAdminService) UpdateAccount(_ context.Context, id int64, input *service.UpdateAccountInput) (*service.Account, error) {
	s.updateAccountCalls++
	s.lastUpdateAccountInput = input
	if s.updateAccountErr != nil {
		return nil, s.updateAccountErr
	}
	for index := range s.accounts {
		if s.accounts[index].ID != id {
			continue
		}
		s.accounts[index].Type = input.Type
		s.accounts[index].Credentials = cloneCodeBuddyHandlerMap(input.Credentials)
		s.accounts[index].Status = input.Status
		return &s.accounts[index], nil
	}
	return nil, service.ErrAccountNotFound
}

func cloneCodeBuddyHandlerMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func pollCodeBuddyHandler(t *testing.T, handler *AccountHandler, loginID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"loginId": loginID})
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/codebuddy/oauth/poll", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	c.Request = request
	handler.PollCodeBuddyDeviceCompat(c)
	return writer
}

func TestPollCodeBuddyDeviceCompatCreatesAndSanitizesAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &codeBuddyHandlerUpstream{responses: []*http.Response{
		codeBuddyHandlerResponse(`{"code":0,"data":{"state":"state","authUrl":"https://www.codebuddy.ai/device/login"}}`),
		codeBuddyHandlerResponse(`{"code":0,"data":{"accessToken":"access-secret","refreshToken":"refresh-secret","expiresIn":3600,"domain":"www.codebuddy.ai"}}`),
		codeBuddyHandlerResponse(`{"code":0,"data":{"uid":"uid-new","enterpriseId":"ent-new","nickname":"New Owner"}}`),
	}}
	login := service.NewCodeBuddyDeviceLoginService(upstream)
	adminService := newCodeBuddyMemoryAdminService(nil)
	handler := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler.SetCodeBuddyDeviceLoginService(login)

	response := pollCodeBuddyHandler(t, handler, startCompletedCodeBuddyHandlerSession(t, login))
	require.Equal(t, http.StatusOK, response.Code)
	require.Len(t, adminService.createdAccounts, 1)
	created := adminService.createdAccounts[0]
	require.Equal(t, service.PlatformCodeBuddy, created.Platform)
	require.Equal(t, service.AccountTypeOAuth, created.Type)
	require.Equal(t, "access-secret", created.Credentials["accessToken"])
	require.Equal(t, "refresh-secret", created.Credentials["refreshToken"])
	require.Equal(t, "uid-new", created.Credentials["userId"])
	require.Contains(t, response.Body.String(), `"created":true`)
	require.NotContains(t, response.Body.String(), "access-secret")
	require.NotContains(t, response.Body.String(), "refresh-secret")
}

func TestPollCodeBuddyDeviceCompatDeduplicatesPreservesMetadataAndRefreshToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &codeBuddyHandlerUpstream{responses: []*http.Response{
		codeBuddyHandlerResponse(`{"code":0,"data":{"state":"state","authUrl":"https://www.codebuddy.ai/device/login"}}`),
		codeBuddyHandlerResponse(`{"code":0,"data":{"accessToken":"new-access","expiresIn":3600,"domain":"www.codebuddy.ai"}}`),
		codeBuddyHandlerResponse(`{"code":0,"data":{"uid":"uid-existing","enterpriseId":"ent-existing","nickname":"Owner"}}`),
	}}
	existing := service.Account{
		ID: 77, Name: "Preserved Name", Platform: service.PlatformCodeBuddy, Type: service.AccountTypeOAuth, Status: service.StatusError,
		Credentials: map[string]any{"userId": "uid-existing", "refresh_token": "old-refresh", "custom": "keep"},
		Extra:       map[string]any{"user_metadata": "keep"}, GroupIDs: []int64{4}, Schedulable: false,
	}
	adminService := newCodeBuddyMemoryAdminService([]service.Account{existing})
	login := service.NewCodeBuddyDeviceLoginService(upstream)
	handler := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler.SetCodeBuddyDeviceLoginService(login)

	response := pollCodeBuddyHandler(t, handler, startCompletedCodeBuddyHandlerSession(t, login))
	require.Equal(t, http.StatusOK, response.Code)
	require.Empty(t, adminService.createdAccounts)
	require.Equal(t, 1, adminService.updateAccountCalls)
	require.Equal(t, "new-access", adminService.lastUpdateAccountInput.Credentials["accessToken"])
	require.Equal(t, "old-refresh", adminService.lastUpdateAccountInput.Credentials["refresh_token"])
	require.Equal(t, "keep", adminService.lastUpdateAccountInput.Credentials["custom"])
	require.Equal(t, service.StatusActive, adminService.lastUpdateAccountInput.Status)
	require.Contains(t, response.Body.String(), `"created":false`)
	require.NotContains(t, response.Body.String(), "new-access")
	require.NotContains(t, response.Body.String(), "old-refresh")
}

func TestFindCodeBuddyDeviceAccountUsesEnterpriseLabelDomainFallback(t *testing.T) {
	accounts := []service.Account{
		{ID: 1, Platform: service.PlatformCodeBuddy, Credentials: map[string]any{"enterpriseId": "ent", "accountLabel": "Owner", "domain": "www.workbuddy.ai"}},
		{ID: 2, Platform: service.PlatformCodeBuddy, Credentials: map[string]any{"enterpriseId": "ent", "accountLabel": "Owner", "domain": "www.codebuddy.ai"}},
	}
	result := &service.CodeBuddyDeviceTokenResult{
		Tokens:      codebuddy.Tokens{Domain: "www.codebuddy.ai"},
		AccountInfo: codebuddy.AccountInfo{EnterpriseID: "ent", Nickname: "Owner"},
	}
	require.Equal(t, int64(2), findCodeBuddyDeviceAccount(accounts, result).ID)
}
