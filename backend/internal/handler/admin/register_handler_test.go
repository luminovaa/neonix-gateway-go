package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type registerRuntimeStub struct {
	startInput service.PythonRegisterStartRequest
	startCalls int
	status     string
}

func (r *registerRuntimeStub) Start(_ context.Context, input service.PythonRegisterStartRequest) (*service.PythonRegisterJobStatus, error) {
	r.startCalls++
	r.startInput = input
	return &service.PythonRegisterJobStatus{JobID: input.JobID, Status: "pending"}, nil
}

func (r *registerRuntimeStub) Status(_ context.Context, jobID string) (*service.PythonRegisterJobStatus, error) {
	status := r.status
	if status == "" {
		status = "running"
	}
	return &service.PythonRegisterJobStatus{JobID: jobID, Status: status}, nil
}

func (r *registerRuntimeStub) Logs(context.Context, string) (*service.PythonRegisterLogs, error) {
	return &service.PythonRegisterLogs{Logs: []string{"running"}}, nil
}

func (r *registerRuntimeStub) Cancel(context.Context, string) (*service.PythonRegisterCancelResult, error) {
	return &service.PythonRegisterCancelResult{Cancelled: true}, nil
}

func (r *registerRuntimeStub) BFSLockout(context.Context) (*service.PythonRegisterBFSLockout, error) {
	return &service.PythonRegisterBFSLockout{}, nil
}

type registerProxyStub struct {
	items []service.Proxy
}

func (r *registerProxyStub) ListActive(context.Context) ([]service.Proxy, error) { return r.items, nil }

type registerPersistenceStub struct {
	calls  int
	result *service.RegisterPersistResult
	err    error
}

func (p *registerPersistenceStub) Persist(context.Context, json.RawMessage) (*service.RegisterPersistResult, error) {
	p.calls++
	return p.result, p.err
}

func TestRegisterHandlerBuildsWorkerRequestWithBackendSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("YYDSMAIL_API_KEY", "server-mail-secret")
	t.Setenv("YYDSMAIL_BASE_URL", "https://mail.example.test/v1")
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, &registerProxyStub{items: []service.Proxy{{
		Protocol: "http", Host: "127.0.0.1", Port: 9000, Username: "proxy-user", Password: "proxy-secret", Status: service.StatusActive,
	}}})

	responseRecorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"type":"register",
		"register_config":{
			"target_provider":"grok",
			"register_method":"http",
			"count":2,
			"concurrency":2,
			"mailbox":{"domain":"example.test"}
		}
	}`)
	require.Equal(t, http.StatusOK, responseRecorder.Code, responseRecorder.Body.String())
	require.Equal(t, 1, runtime.startCalls)
	config := runtime.startInput.RegisterConfig
	require.NotNil(t, config)
	require.Equal(t, "server-mail-secret", config.Mailbox.APIKey)
	require.Equal(t, "https://mail.example.test/v1", config.Mailbox.BaseURL)
	require.Equal(t, "example.test", config.Mailbox.Domain)
	require.Equal(t, []string{"http://proxy-user:proxy-secret@127.0.0.1:9000"}, config.Proxies)
	require.Equal(t, config.Proxies[0], config.Proxy)
}

func TestRegisterHandlerRejectsDeprecatedProviderAndSecretPassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}

	for _, body := range []string{
		`{"register_config":{"target_provider":"codebuff","register_method":"google","google_accounts":[{"email":"owner@example.com","password":"secret"}]}}`,
		`{"register_config":{"target_provider":"grok","register_method":"email","mailbox":{"domain":"example.test","api_key":"browser-secret"}}}`,
	} {
		handler := newRegisterHandlerForTest(runtime, nil)
		recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", body)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	}
	require.Zero(t, runtime.startCalls)
}

func TestRegisterHandlerPreventsOverlapAndReleasesTerminalJob(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("YYDSMAIL_API_KEY", "mail-secret")
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, nil)
	body := `{"register_config":{"target_provider":"kiro","register_method":"email"}}`

	first := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", body)
	require.Equal(t, http.StatusOK, first.Code)
	jobID := handler.activeJobID
	require.NotEmpty(t, jobID)
	second := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", body)
	require.Equal(t, http.StatusConflict, second.Code)
	require.Equal(t, 1, runtime.startCalls)

	runtime.status = "done"
	status := performRegisterHandlerRequest(handler.Status, http.MethodGet, "/api/register/status?job_id="+jobID, "")
	require.Equal(t, http.StatusOK, status.Code)
	require.Empty(t, handler.activeJobID)

	third := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", body)
	require.Equal(t, http.StatusOK, third.Code)
	require.Equal(t, 2, runtime.startCalls)
}

func TestRegisterHandlerRejectsUnmigratedSensitiveJobs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	for _, jobType := range []string{"relogin", "inject", "subscribe", "qoder_auth"} {
		handler := newRegisterHandlerForTest(runtime, nil)
		body, err := json.Marshal(map[string]any{"type": jobType, "register_config": map[string]any{"target_provider": "qoder"}})
		require.NoError(t, err)
		recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", string(body))
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		require.Contains(t, recorder.Body.String(), "REGISTER_FEATURE_NOT_MIGRATED")
	}
	require.Zero(t, runtime.startCalls)
}

func TestRegisterHandlerInternalAuthRejectsWrongKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &RegisterHandler{internalKey: "expected-secret"}
	router := gin.New()
	router.POST("/api/register/result", handler.InternalAuth(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/api/register/result", nil)
	request.Header.Set("x-api-key", "wrong-secret")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Contains(t, recorder.Body.String(), "REGISTER_CALLBACK_UNAUTHORIZED")
}

func TestRegisterHandlerRejectsCallbackForInactiveJob(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &RegisterHandler{activeJobID: "register-active"}
	recorder := performRegisterHandlerRequest(
		handler.ResultCallback,
		http.MethodPost,
		"/api/register/result",
		`{"job_id":"register-stale","account":{"provider":"grok"}}`,
	)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), "REGISTER_CALLBACK_INVALID")
}

func TestRegisterHandlerResultCallbackPersistsOnceAndSanitizesResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	persistence := &registerPersistenceStub{result: &service.RegisterPersistResult{
		Created: true,
		Account: &service.Account{
			ID: 73, Name: "registered", Platform: service.PlatformGrok, Status: service.StatusActive,
			Credentials: map[string]any{"access_token": "must-not-leak"},
		},
	}}
	handler := &RegisterHandler{activeJobID: "register-active", persistence: persistence}
	recorder := performRegisterHandlerRequest(
		handler.ResultCallback,
		http.MethodPost,
		"/api/register/result",
		`{"job_id":"register-active","account":{"provider":"grok","credentials":{"accessToken":"upstream-secret"}}}`,
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, persistence.calls)
	require.Contains(t, recorder.Body.String(), `"id":73`)
	require.NotContains(t, recorder.Body.String(), "must-not-leak")
	require.NotContains(t, recorder.Body.String(), "upstream-secret")
	require.NotContains(t, recorder.Body.String(), "credentials")
}

func TestRegisterHandlerFailureCallbackDoesNotEchoRawError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &RegisterHandler{activeJobID: "register-active"}
	recorder := performRegisterHandlerRequest(
		handler.FailureCallback,
		http.MethodPost,
		"/api/register/failure",
		`{"job_id":"register-active","email":"owner@example.com","error":"provider leaked token secret-123"}`,
	)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "secret-123")
	require.NotContains(t, recorder.Body.String(), "provider leaked token")
}

func performRegisterHandlerRequest(handler gin.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	if body != "" {
		ctx.Request.Header.Set("Content-Type", "application/json")
	}
	handler(ctx)
	return recorder
}
