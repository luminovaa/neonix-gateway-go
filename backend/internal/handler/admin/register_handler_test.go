package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type registerRuntimeStub struct {
	startInput         service.PythonRegisterStartRequest
	startCalls         int
	status             string
	startStatus        *service.PythonRegisterJobStatus
	statusResult       json.RawMessage
	logs               []string
	statusCalls        int
	logCalls           int
	cancelCalls        int
	statusErr          error
	logsErr            error
	cancelErr          error
	runtimeStatus      service.PythonRegisterRuntimeStatus
	runtimeStatusCalls int
}

func (r *registerRuntimeStub) Start(_ context.Context, input service.PythonRegisterStartRequest) (*service.PythonRegisterJobStatus, error) {
	r.startCalls++
	r.startInput = input
	if r.startStatus != nil {
		return r.startStatus, nil
	}
	return &service.PythonRegisterJobStatus{JobID: input.JobID, Status: "pending"}, nil
}

func (r *registerRuntimeStub) Status(_ context.Context, jobID string) (*service.PythonRegisterJobStatus, error) {
	r.statusCalls++
	if r.statusErr != nil {
		return nil, r.statusErr
	}
	status := r.status
	if status == "" {
		status = "running"
	}
	return &service.PythonRegisterJobStatus{JobID: jobID, Status: status, Result: r.statusResult, Logs: r.logs, Error: "raw worker failure with access_token=secret"}, nil
}

func (r *registerRuntimeStub) Logs(context.Context, string) (*service.PythonRegisterLogs, error) {
	r.logCalls++
	if r.logsErr != nil {
		return nil, r.logsErr
	}
	return &service.PythonRegisterLogs{Logs: append([]string{"running"}, r.logs...)}, nil
}

func (r *registerRuntimeStub) Cancel(context.Context, string) (*service.PythonRegisterCancelResult, error) {
	r.cancelCalls++
	if r.cancelErr != nil {
		return nil, r.cancelErr
	}
	return &service.PythonRegisterCancelResult{Cancelled: true}, nil
}

func (r *registerRuntimeStub) BFSLockout(context.Context) (*service.PythonRegisterBFSLockout, error) {
	return &service.PythonRegisterBFSLockout{}, nil
}

func (r *registerRuntimeStub) RuntimeStatus(context.Context) service.PythonRegisterRuntimeStatus {
	r.runtimeStatusCalls++
	return r.runtimeStatus
}

func (r *registerRuntimeStub) Capabilities(context.Context) (service.PythonBrowserCapabilities, error) {
	return service.PythonBrowserCapabilities{"camoufox": {Available: true, PackageInstalled: true, BinaryAvailable: true}}, nil
}

type registerProxyStub struct {
	items []service.Proxy
}

func (r *registerProxyStub) ListActive(context.Context) ([]service.Proxy, error) { return r.items, nil }

type registerPersistenceStub struct {
	mu                sync.Mutex
	calls             int
	result            *service.RegisterPersistResult
	err               error
	entered           chan struct{}
	release           chan struct{}
	reloginCandidates []service.PythonRegisterAccount
	reloginErr        error
	reloginSummary    service.RegisterReloginCandidateSummary
	injectCandidates  []service.PythonRegisterAccount
	injectErr         error
	injectSummary     service.RegisterQoderInjectCandidateSummary
	cbcnCandidates    []service.PythonRegisterAccount
	lastJobType       string
	lastJobID         string
	githubPicker      []service.RegisterGitHubPickerAccount
	githubReserved    []service.PythonRegisterAccount
	reservedIDs       []string
	reservedJobID     string
	releases          []registerReleaseCall
}

type registerReleaseCall struct {
	jobID, email, reason string
}

func (p *registerPersistenceStub) PersistForJobID(_ context.Context, _ json.RawMessage, jobType, jobID string) (*service.RegisterPersistResult, error) {
	p.mu.Lock()
	p.calls++
	p.lastJobType = jobType
	p.lastJobID = jobID
	p.mu.Unlock()
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
	}
	if p.release != nil {
		<-p.release
	}
	return p.result, p.err
}

func (p *registerPersistenceStub) ListGitHubPickerAccounts(context.Context) ([]service.RegisterGitHubPickerAccount, error) {
	return append([]service.RegisterGitHubPickerAccount(nil), p.githubPicker...), nil
}

func (p *registerPersistenceStub) ReserveGitHubAccounts(_ context.Context, ids []string, jobID string) ([]service.PythonRegisterAccount, error) {
	p.mu.Lock()
	p.reservedIDs = append([]string(nil), ids...)
	p.reservedJobID = jobID
	p.mu.Unlock()
	return append([]service.PythonRegisterAccount(nil), p.githubReserved...), nil
}

func (p *registerPersistenceStub) ReleaseGitHubReservations(_ context.Context, jobID, email, reason string) error {
	p.mu.Lock()
	p.releases = append(p.releases, registerReleaseCall{jobID: jobID, email: email, reason: reason})
	p.mu.Unlock()
	return nil
}

func (p *registerPersistenceStub) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *registerPersistenceStub) ListGrokReloginCandidates(context.Context) ([]service.PythonRegisterAccount, error) {
	if p.reloginErr != nil {
		return nil, p.reloginErr
	}
	return append([]service.PythonRegisterAccount(nil), p.reloginCandidates...), nil
}

func (p *registerPersistenceStub) GrokReloginCandidateSummary(context.Context) (service.RegisterReloginCandidateSummary, error) {
	if p.reloginErr != nil {
		return service.RegisterReloginCandidateSummary{}, p.reloginErr
	}
	return p.reloginSummary, nil
}

func (p *registerPersistenceStub) ListQoderInjectCandidates(context.Context) ([]service.PythonRegisterAccount, error) {
	if p.injectErr != nil {
		return nil, p.injectErr
	}
	return append([]service.PythonRegisterAccount(nil), p.injectCandidates...), nil
}

func (p *registerPersistenceStub) QoderInjectCandidateSummary(context.Context) (service.RegisterQoderInjectCandidateSummary, error) {
	if p.injectErr != nil {
		return service.RegisterQoderInjectCandidateSummary{}, p.injectErr
	}
	return p.injectSummary, nil
}

func (p *registerPersistenceStub) ListCodeBuddyChinaClaimCandidates(context.Context) ([]service.PythonRegisterAccount, error) {
	return append([]service.PythonRegisterAccount(nil), p.cbcnCandidates...), nil
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
			"proxy":"http://browser-supplied.example:3128",
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
	require.NotContains(t, config.Proxy, "browser-supplied.example")
}

func TestRegisterHandlerAcceptsCurrentWebNullProxyFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("YYDSMAIL_API_KEY", "server-mail-secret")
	t.Setenv("SUBSCRIPTION_BIN", "4111111111111111")
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, nil)
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"type":"register_subscribe",
		"register_config":{
			"target_provider":"kiro",
			"register_method":"email",
			"proxy":null
		},
		"subscribe_config":{"proxy":null}
	}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, runtime.startCalls)
	require.NotNil(t, runtime.startInput.RegisterConfig)
	require.NotNil(t, runtime.startInput.SubscribeConfig)
}

func TestRegisterHandlerUseProxyFalseSkipsProxyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("YYDSMAIL_API_KEY", "server-mail-secret")
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, &registerProxyStub{items: []service.Proxy{{
		Protocol: "http", Host: "127.0.0.1", Port: 9000, Username: "proxy-user", Password: "proxy-secret", Status: service.StatusActive,
	}}})
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{"register_config":{"target_provider":"grok","register_method":"http","use_proxy":false}}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Empty(t, runtime.startInput.RegisterConfig.Proxy)
	require.Empty(t, runtime.startInput.RegisterConfig.Proxies)
}

func TestRegisterHandlerFiltersExpiredProxies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("YYDSMAIL_API_KEY", "server-mail-secret")
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, &registerProxyStub{items: []service.Proxy{
		{Protocol: "http", Host: "expired.test", Port: 9000, Status: service.StatusActive, ExpiresAt: &past},
		{Protocol: "http", Host: "active.test", Port: 9001, Status: service.StatusActive, ExpiresAt: &future},
	}})
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{"register_config":{"target_provider":"grok","register_method":"http"}}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, []string{"http://active.test:9001"}, runtime.startInput.RegisterConfig.Proxies)
}

func TestRegisterSystemStatusUsesBackendConfigurationAndRuntimeCapabilities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("YYDSMAIL_API_KEY", "mail-secret")
	t.Setenv("YYDSMAIL_BASE_URL", "https://mail.example.test/v1")
	t.Setenv("YESCAPTCHA_API_KEY", "captcha-secret")
	t.Setenv("SUBSCRIPTION_BIN", "4111111111111111")
	t.Setenv("FIVESIM_API_KEY", "five-secret")
	runtime := &registerRuntimeStub{runtimeStatus: service.PythonRegisterRuntimeStatus{ChildReady: true, ChildRunning: true}}
	handler := newRegisterHandlerForTest(runtime, &registerProxyStub{items: []service.Proxy{{
		Protocol: "http", Host: "active.test", Port: 9001, Status: service.StatusActive,
	}}})

	recorder := performRegisterHandlerRequest(handler.SystemStatus, http.MethodGet, "/api/register/system-status", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	var payload map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	payload = envelope.Data
	require.Equal(t, true, payload["pythonHealthy"])
	require.Equal(t, float64(1), payload["proxyPoolCount"])
	require.Equal(t, true, payload["mailConfigured"])
	require.Equal(t, true, payload["captchaConfigured"])
	require.Equal(t, true, payload["subscriptionConfigured"])
	require.Equal(t, true, payload["fivesimConfigured"])
	require.Equal(t, false, payload["herosmsConfigured"])
	require.Equal(t, "https://mail.example.test/v1", payload["mailBaseUrl"])
	require.NotNil(t, payload["browserEngines"])
}

func TestRegisterSMSPricesNormalizesFiveSIMCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("FIVESIM_API_KEY", "configured")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"codebuddy":{"japan":{"any":{"cost":2.5,"count":3}},"canada":{"a":{"cost":3,"count":1},"b":{"cost":2,"count":4}},"empty":{"any":{"cost":1,"count":0}}}}`))
	}))
	defer server.Close()
	original := fiveSIMPricesURL
	fiveSIMPricesURL = server.URL
	defer func() { fiveSIMPricesURL = original }()

	handler := newRegisterHandlerForTest(&registerRuntimeStub{}, nil)
	recorder := performRegisterHandlerRequest(handler.SMSPrices, http.MethodGet, "/api/register/sms/prices?provider=fivesim", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "private, max-age=60", recorder.Header().Get("Cache-Control"))
	var payload struct {
		Data struct {
			Provider  string               `json:"provider"`
			Countries []registerSMSCountry `json:"countries"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, "fivesim", payload.Data.Provider)
	require.Equal(t, []registerSMSCountry{{Country: "canada", Name: "canada", Cost: 2, Count: 4}, {Country: "japan", Name: "japan", Cost: 2.5, Count: 3}}, payload.Data.Countries)
}

func TestRegisterSMSPricesRejectsUnsupportedProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := newRegisterHandlerForTest(&registerRuntimeStub{}, nil)
	recorder := performRegisterHandlerRequest(handler.SMSPrices, http.MethodGet, "/api/register/sms/prices?provider=unknown", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "REGISTER_SMS_PROVIDER_INVALID")
}

func TestSanitizeRegisterLogsRedactsPartialSecretForms(t *testing.T) {
	logs := sanitizeRegisterLogs([]string{
		"api key: sk-partial-secret",
		"refresh token=refresh-partial-secret",
		"Authorization: Bearer-access-partial-secret",
		"proxy=http://user:proxy-partial-secret@example.test:8080",
	})
	joined := strings.Join(logs, "\n")
	for _, secret := range []string{"sk-partial-secret", "refresh-partial-secret", "access-partial-secret", "proxy-partial-secret"} {
		require.NotContains(t, joined, secret)
	}
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

func TestRegisterHandlerPythonStatusReportsManagerAndChildSeparately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{runtimeStatus: service.PythonRegisterRuntimeStatus{
		ManagerConfigured: true, ManagerRunning: true, ChildRunning: false, ChildReady: false, Leases: 0,
	}}
	handler := newRegisterHandlerForTest(runtime, nil)
	recorder := performRegisterHandlerRequest(handler.PythonStatus, http.MethodGet, "/api/register/python-status", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, runtime.runtimeStatusCalls)
	require.Contains(t, recorder.Body.String(), `"manager_running":true`)
	require.Contains(t, recorder.Body.String(), `"child_running":false`)
	require.Contains(t, recorder.Body.String(), `"child_ready":false`)
	require.Contains(t, recorder.Body.String(), `"running":false`)
	require.Contains(t, recorder.Body.String(), `"healthy":false`)
}

func TestRegisterHandlerScopesJobOperationsToActiveJob(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, nil)
	handler.activeJobID = "active-job"

	status := performRegisterHandlerRequest(handler.Status, http.MethodGet, "/api/register/status?job_id=other-job", "")
	logs := performRegisterHandlerRequest(handler.Logs, http.MethodGet, "/api/register/logs?job_id=other-job", "")
	cancel := performRegisterHandlerRequest(handler.Cancel, http.MethodPost, "/api/register/cancel", "{\"job_id\":\"other-job\"}")

	for _, recorder := range []*httptest.ResponseRecorder{status, logs, cancel} {
		require.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
		require.Contains(t, recorder.Body.String(), "REGISTER_JOB_NOT_FOUND")
	}
	require.Zero(t, runtime.statusCalls)
	require.Zero(t, runtime.logCalls)
	require.Zero(t, runtime.cancelCalls)
}

func TestRegisterHandlerClearsActiveJobWhenWorkerForgetsIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name   string
		invoke func(*RegisterHandler) *httptest.ResponseRecorder
		setup  func(*registerRuntimeStub)
	}{
		{
			name: "status",
			setup: func(runtime *registerRuntimeStub) {
				runtime.statusErr = &service.RegisterRuntimeError{Code: "REGISTER_JOB_NOT_FOUND"}
			},
			invoke: func(handler *RegisterHandler) *httptest.ResponseRecorder {
				return performRegisterHandlerRequest(handler.Status, http.MethodGet, "/api/register/status?job_id=forgotten-job", "")
			},
		},
		{
			name: "logs",
			setup: func(runtime *registerRuntimeStub) {
				runtime.logsErr = &service.RegisterRuntimeError{Code: "REGISTER_JOB_NOT_FOUND"}
			},
			invoke: func(handler *RegisterHandler) *httptest.ResponseRecorder {
				return performRegisterHandlerRequest(handler.Logs, http.MethodGet, "/api/register/logs?job_id=forgotten-job", "")
			},
		},
		{
			name: "cancel",
			setup: func(runtime *registerRuntimeStub) {
				runtime.cancelErr = &service.RegisterRuntimeError{Code: "REGISTER_JOB_NOT_FOUND"}
			},
			invoke: func(handler *RegisterHandler) *httptest.ResponseRecorder {
				return performRegisterHandlerRequest(handler.Cancel, http.MethodPost, "/api/register/cancel", `{"job_id":"forgotten-job"}`)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &registerRuntimeStub{}
			test.setup(runtime)
			handler := newRegisterHandlerForTest(runtime, nil)
			handler.activeJobID = "forgotten-job"
			recorder := test.invoke(handler)
			require.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
			require.Empty(t, handler.activeJobID)
		})
	}
}

func TestRegisterHandlerStatusAndLogsRedactSecretsAndDropWorkerResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{
		status:       "running",
		statusResult: json.RawMessage("{\"credentials\":{\"access_token\":\"result-secret\"}}"),
		logs: []string{
			"mailbox api_key=mail-secret",
			"proxy=http://user:proxy-secret@example.test:8080",
			"access_token=result-secret",
		},
	}
	handler := newRegisterHandlerForTest(runtime, nil)
	handler.activeJobID = "active-job"

	status := performRegisterHandlerRequest(handler.Status, http.MethodGet, "/api/register/status?job_id=active-job", "")
	logs := performRegisterHandlerRequest(handler.Logs, http.MethodGet, "/api/register/logs?job_id=active-job", "")

	for _, recorder := range []*httptest.ResponseRecorder{status, logs} {
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.NotContains(t, recorder.Body.String(), "mail-secret")
		require.NotContains(t, recorder.Body.String(), "proxy-secret")
		require.NotContains(t, recorder.Body.String(), "result-secret")
		require.NotContains(t, recorder.Body.String(), "credentials")
	}
}

func TestRegisterHandlerRejectsWorkerStartJobIDMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("YYDSMAIL_API_KEY", "mail-secret")
	runtime := &registerRuntimeStub{startStatus: &service.PythonRegisterJobStatus{JobID: "different-job", Status: "pending"}}
	handler := newRegisterHandlerForTest(runtime, nil)
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", "{\"register_config\":{\"target_provider\":\"kiro\",\"register_method\":\"email\"}}")

	require.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "REGISTER_RUNTIME_PROTOCOL_INVALID")
	require.Empty(t, handler.activeJobID)
}

func TestRegisterHandlerRejectsUnmigratedSensitiveJobs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	for _, jobType := range []string{"subscribe", "qoder_auth", "qoder_reset"} {
		handler := newRegisterHandlerForTest(runtime, nil)
		body, err := json.Marshal(map[string]any{"type": jobType, "register_config": map[string]any{"target_provider": "qoder"}})
		require.NoError(t, err)
		recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", string(body))
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		require.Contains(t, recorder.Body.String(), "REGISTER_FEATURE_NOT_MIGRATED")
	}
	require.Zero(t, runtime.startCalls)
}

func TestRegisterHandlerBuildsQoderInjectFromBackendCandidates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	persistence := &registerPersistenceStub{injectCandidates: []service.PythonRegisterAccount{{
		ID: "legacy-qoder-1", Email: "owner@example.com", Provider: "qoder",
		Credentials: map[string]any{"token": "pt-server-secret"}, Tags: []string{"keep"},
	}}}
	handler := newRegisterHandlerForTest(runtime, &registerProxyStub{items: []service.Proxy{{
		Protocol: "http", Host: "127.0.0.1", Port: 9000, Status: service.StatusActive,
	}}})
	handler.persistence = persistence
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"type":"inject",
		"register_config":{"target_provider":"qoder","proxy":null,"concurrency":99,"qoder_force_local":false}
	}`)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, runtime.startCalls)
	require.Equal(t, "inject", runtime.startInput.Type)
	require.Equal(t, "qoder", runtime.startInput.Provider)
	config := runtime.startInput.RegisterConfig
	require.Equal(t, "qoder", config.TargetProvider)
	require.Equal(t, "http", config.RegisterMethod)
	require.Equal(t, 1, config.Concurrency)
	require.False(t, config.QoderForceLocal)
	require.True(t, config.QoderInjectTrial)
	require.Len(t, config.InjectAccounts, 1)
	require.Equal(t, "pt-server-secret", config.InjectAccounts[0].Credentials["token"])
	require.Equal(t, []string{"http://127.0.0.1:9000"}, config.Proxies)
	require.NotContains(t, recorder.Body.String(), "pt-server-secret")
}

func TestRegisterHandlerRejectsBrowserSuppliedInjectAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, nil)
	handler.persistence = &registerPersistenceStub{injectCandidates: []service.PythonRegisterAccount{{ID: "server-account"}}}
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"type":"inject",
		"register_config":{"target_provider":"qoder","inject_accounts":[{"email":"attacker@example.com","credentials":{"token":"pt-browser"}}]}
	}`)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "REGISTER_PAYLOAD_INVALID")
	require.Zero(t, runtime.startCalls)
	require.NotContains(t, recorder.Body.String(), "pt-browser")
}

func TestRegisterHandlerBuildsCodeBuddyGitHubWorkerRequestFromServerSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	persistence := &registerPersistenceStub{githubReserved: []service.PythonRegisterAccount{{
		ID: "github-7", Email: "owner@example.com", Username: "octocat", Password: "server-password",
		Cookies: []map[string]any{{"name": "session", "value": "server-cookie"}}, UserAgent: "server-agent", Provider: "github",
	}}}
	handler := newRegisterHandlerForTest(runtime, nil)
	handler.persistence = persistence
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"type":"register",
		"register_config":{"target_provider":"codebuddy","register_method":"github","github_account_ids":["github-7"],"use_proxy":false}
	}`)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, []string{"github-7"}, persistence.reservedIDs)
	require.NotEmpty(t, persistence.reservedJobID)
	require.Equal(t, persistence.reservedJobID, runtime.startInput.JobID)
	require.Len(t, runtime.startInput.RegisterConfig.GitHubAccounts, 1)
	require.Equal(t, "server-password", runtime.startInput.RegisterConfig.GitHubAccounts[0].Password)
	require.Equal(t, "server-cookie", runtime.startInput.RegisterConfig.GitHubAccounts[0].Cookies[0]["value"])
	require.NotContains(t, recorder.Body.String(), "server-password")
	require.NotContains(t, recorder.Body.String(), "server-cookie")
}

func TestRegisterHandlerBuildsCodeBuddyChinaClaimFromBackendAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	persistence := &registerPersistenceStub{cbcnCandidates: []service.PythonRegisterAccount{{
		ID: "cbcn-1", Email: "owner@example.com", AccessToken: "access-secret", RefreshToken: "refresh-secret", Provider: service.PlatformCodeBuddyChina,
	}}}
	handler := newRegisterHandlerForTest(runtime, nil)
	handler.persistence = persistence
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"type":"register",
		"register_config":{"target_provider":"codebuddy-china","register_method":"sms","cbcn_action":"claim","count":1,"use_proxy":false}
	}`)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	config := runtime.startInput.RegisterConfig
	require.Equal(t, "claim", config.CBCNAction)
	require.Len(t, config.CBCNAccounts, 1)
	require.Equal(t, "refresh-secret", config.CBCNAccounts[0].RefreshToken)
	require.Empty(t, config.FiveSIMAPIKey)
	require.Empty(t, config.HeroSMSAPIKey)
	require.NotContains(t, recorder.Body.String(), "refresh-secret")
	require.NotContains(t, recorder.Body.String(), "access-secret")
}

func TestRegisterHandlerRejectsBrowserSuppliedCodeBuddyChinaClaimAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, nil)
	handler.persistence = &registerPersistenceStub{cbcnCandidates: []service.PythonRegisterAccount{{ID: "server-account", RefreshToken: "server-secret"}}}
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"register_config":{"target_provider":"codebuddy-china","register_method":"sms","cbcn_action":"claim","cbcn_accounts":[{"id":"attacker","refreshToken":"browser-secret"}]}
	}`)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "REGISTER_PAYLOAD_INVALID")
	require.Zero(t, runtime.startCalls)
	require.NotContains(t, recorder.Body.String(), "browser-secret")
}

func TestRegisterHandlerRejectsUnknownCodeBuddyChinaAction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, nil)
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"register_config":{"target_provider":"codebuddy-china","register_method":"sms","cbcn_action":"unsafe"}
	}`)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "REGISTER_CBCN_ACTION_INVALID")
	require.Zero(t, runtime.startCalls)
}

func TestRegisterHandlerRejectsBrowserSuppliedGitHubSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, nil)
	handler.persistence = &registerPersistenceStub{}
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"register_config":{
			"target_provider":"codebuddy","register_method":"github",
			"github_account_ids":["github-7"],
			"github_accounts":[{"id":"github-7","password":"browser-secret","cookies":[{"name":"session","value":"browser-cookie"}]}]
		}
	}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "REGISTER_PAYLOAD_INVALID")
	require.Zero(t, runtime.startCalls)
	require.NotContains(t, recorder.Body.String(), "browser-secret")
	require.NotContains(t, recorder.Body.String(), "browser-cookie")
}

func TestRegisterHandlerReleasesIncompleteGitHubReservationsAtSuccessfulTerminalStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{status: "done"}
	persistence := &registerPersistenceStub{}
	handler := newRegisterHandlerForTest(runtime, nil)
	handler.persistence = persistence
	handler.activeJobID = "github-job"
	handler.activeJobType = "register"

	recorder := performRegisterHandlerRequest(handler.Status, http.MethodGet, "/api/register/status?job_id=github-job", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Empty(t, handler.activeJobID)
	require.Equal(t, []registerReleaseCall{{jobID: "github-job", reason: "REGISTER_GITHUB_LINK_INCOMPLETE"}}, persistence.releases)
}

func TestRegisterHandlerRejectsUnsupportedInjectProviderAndEmptyCandidateSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{name: "wrong provider", body: `{"type":"inject","register_config":{"target_provider":"grok"}}`, code: "REGISTER_FEATURE_NOT_MIGRATED"},
		{name: "empty", body: `{"type":"inject","register_config":{"target_provider":"qoder"}}`, code: "REGISTER_INJECT_ACCOUNT_NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &registerRuntimeStub{}
			handler := newRegisterHandlerForTest(runtime, nil)
			handler.persistence = &registerPersistenceStub{}
			recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", tc.body)
			require.Contains(t, recorder.Body.String(), tc.code)
			require.Zero(t, runtime.startCalls)
		})
	}
}

func TestRegisterHandlerBuildsGrokReloginFromBackendCandidates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	persistence := &registerPersistenceStub{reloginCandidates: []service.PythonRegisterAccount{{
		ID: "legacy-grok-1", Email: "owner@example.com", Password: "  server password  ",
		Provider: "grok", Credentials: map[string]any{"refresh_token": "server-refresh"},
	}}}
	handler := newRegisterHandlerForTest(runtime, nil)
	handler.persistence = persistence
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"type":"relogin",
		"register_config":{
			"target_provider":"grok","register_method":"browser","proxy":null,"concurrency":2,"max_retries":4
		}
	}`)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, runtime.startCalls)
	require.Equal(t, "relogin", runtime.startInput.Type)
	require.Equal(t, "grok", runtime.startInput.Provider)
	require.Equal(t, "grok", runtime.startInput.RegisterConfig.TargetProvider)
	require.Equal(t, "http", runtime.startInput.RegisterConfig.RegisterMethod)
	require.Equal(t, 2, runtime.startInput.RegisterConfig.Concurrency)
	require.Equal(t, 4, runtime.startInput.RegisterConfig.MaxRetries)
	require.Len(t, runtime.startInput.RegisterConfig.ReloginAccounts, 1)
	require.Equal(t, "owner@example.com", runtime.startInput.RegisterConfig.ReloginAccounts[0].Email)
	require.Equal(t, "  server password  ", runtime.startInput.RegisterConfig.ReloginAccounts[0].Password)
	require.NotContains(t, recorder.Body.String(), "server password")
	require.NotContains(t, recorder.Body.String(), "server-refresh")
}

func TestRegisterHandlerRejectsBrowserSuppliedReloginAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &registerRuntimeStub{}
	handler := newRegisterHandlerForTest(runtime, nil)
	handler.persistence = &registerPersistenceStub{reloginCandidates: []service.PythonRegisterAccount{{ID: "server-account", Password: "server-secret"}}}
	recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", `{
		"type":"relogin",
		"register_config":{"target_provider":"grok","relogin_accounts":[{"email":"attacker@example.com","password":"browser-secret"}]}
	}`)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "REGISTER_PAYLOAD_INVALID")
	require.Zero(t, runtime.startCalls)
	require.NotContains(t, recorder.Body.String(), "browser-secret")
}

func TestRegisterHandlerRejectsUnsupportedReloginProviderAndEmptyCandidateSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{name: "m365", body: `{"type":"relogin","register_config":{"target_provider":"m365"}}`, code: "REGISTER_FEATURE_NOT_MIGRATED"},
		{name: "empty", body: `{"type":"relogin","register_config":{"target_provider":"grok"}}`, code: "REGISTER_RELOGIN_ACCOUNT_NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &registerRuntimeStub{}
			handler := newRegisterHandlerForTest(runtime, nil)
			handler.persistence = &registerPersistenceStub{}
			recorder := performRegisterHandlerRequest(handler.Start, http.MethodPost, "/api/register/start", tc.body)
			require.Contains(t, recorder.Body.String(), tc.code)
			require.Zero(t, runtime.startCalls)
		})
	}
}

func TestRegisterHandlerGrokExpiredCountReturnsOnlyAggregate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := newRegisterHandlerForTest(&registerRuntimeStub{}, nil)
	handler.persistence = &registerPersistenceStub{reloginSummary: service.RegisterReloginCandidateSummary{Count: 3, TotalProvider: 7}}
	recorder := performRegisterHandlerRequest(handler.GrokExpiredCount, http.MethodGet, "/api/register/grok-expired-count", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"code":0,"message":"success","data":{"count":3,"totalGrok":7}}`, recorder.Body.String())
}

func TestRegisterHandlerQoderInjectableCountReturnsOnlyAggregate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := newRegisterHandlerForTest(&registerRuntimeStub{}, nil)
	handler.persistence = &registerPersistenceStub{injectSummary: service.RegisterQoderInjectCandidateSummary{Count: 4, TotalProvider: 9}}
	recorder := performRegisterHandlerRequest(handler.QoderInjectableCount, http.MethodGet, "/api/register/qoder-injectable-count", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"code":0,"message":"success","data":{"count":4,"totalQoder":9}}`, recorder.Body.String())
}

func TestBoundRegisterWorkerAccountsKeepsRequestBounded(t *testing.T) {
	accounts := []service.PythonRegisterAccount{
		{ID: "1", Credentials: map[string]any{"token": strings.Repeat("a", 64)}},
		{ID: "2", Credentials: map[string]any{"token": strings.Repeat("b", 64)}},
	}
	one, err := json.Marshal(accounts[0])
	require.NoError(t, err)
	bounded := boundRegisterWorkerAccounts(accounts, len(one)+2)
	require.Len(t, bounded, 1)
	require.Equal(t, "1", bounded[0].ID)
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
	require.Equal(t, 1, persistence.callCount())
	require.Contains(t, recorder.Body.String(), `"id":73`)
	require.NotContains(t, recorder.Body.String(), "must-not-leak")
	require.NotContains(t, recorder.Body.String(), "upstream-secret")
	require.NotContains(t, recorder.Body.String(), "credentials")

	replay := performRegisterHandlerRequest(
		handler.ResultCallback,
		http.MethodPost,
		"/api/register/result",
		"{\"job_id\":\"register-active\",\"account\":{\"provider\":\"grok\",\"credentials\":{\"accessToken\":\"upstream-secret\"}}}",
	)
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	require.Equal(t, 1, persistence.callCount())
	require.Equal(t, recorder.Body.String(), replay.Body.String())
}

func TestRegisterHandlerResultCallbackUsesServerOwnedJobType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	persistence := &registerPersistenceStub{result: &service.RegisterPersistResult{
		Account: &service.Account{ID: 76, Name: "qoder", Platform: service.PlatformQoder, Status: service.StatusDisabled},
	}}
	handler := &RegisterHandler{
		activeJobID: "inject-active", activeJobType: "inject", persistence: persistence,
		callbacks: make(map[string]*registerCallbackEntry),
	}
	recorder := performRegisterHandlerRequest(
		handler.ResultCallback, http.MethodPost, "/api/register/result",
		`{"job_id":"inject-active","account":{"provider":"qoder","status":"active","credentials":{"token":"pt-worker-secret"}}}`,
	)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "inject", persistence.lastJobType)
	require.NotContains(t, recorder.Body.String(), "pt-worker-secret")
}

func TestRegisterHandlerAcceptsLateCallbackAfterTerminalStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	persistence := &registerPersistenceStub{result: &service.RegisterPersistResult{
		Account: &service.Account{ID: 77, Name: "qoder", Platform: service.PlatformQoder, Status: service.StatusDisabled},
	}}
	handler := &RegisterHandler{
		activeJobID: "inject-done", activeJobType: "inject", persistence: persistence,
		callbacks: make(map[string]*registerCallbackEntry), terminalJobs: make(map[string]registerTerminalJob),
	}
	handler.clearActiveJob("inject-done")
	require.Empty(t, handler.activeJobID)

	recorder := performRegisterHandlerRequest(
		handler.ResultCallback, http.MethodPost, "/api/register/result",
		`{"job_id":"inject-done","account":{"provider":"qoder","credentials":{"token":"pt-worker-secret"}}}`,
	)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "inject", persistence.lastJobType)
	require.NotContains(t, recorder.Body.String(), "pt-worker-secret")
}

func TestRegisterHandlerCoalescesConcurrentCallbackReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	persistence := &registerPersistenceStub{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		result: &service.RegisterPersistResult{
			Created: true,
			Account: &service.Account{ID: 74, Name: "registered", Platform: service.PlatformGrok, Status: service.StatusActive},
		},
	}
	handler := &RegisterHandler{activeJobID: "register-active", persistence: persistence, callbacks: make(map[string]*registerCallbackEntry)}
	body := "{\"job_id\":\"register-active\",\"account\":{\"provider\":\"grok\",\"credentials\":{\"accessToken\":\"upstream-secret\"}}}"

	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- performRegisterHandlerRequest(handler.ResultCallback, http.MethodPost, "/api/register/result", body)
	}()
	<-persistence.entered
	go func() {
		responses <- performRegisterHandlerRequest(handler.ResultCallback, http.MethodPost, "/api/register/result", body)
	}()
	close(persistence.release)

	first := <-responses
	second := <-responses
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	require.Equal(t, first.Body.String(), second.Body.String())
	require.Equal(t, 1, persistence.callCount())
}

func TestRegisterHandlerRetriesCallbackAfterPersistenceFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	persistence := &registerPersistenceStub{err: fmt.Errorf("database error containing password=must-not-leak")}
	handler := &RegisterHandler{activeJobID: "register-active", persistence: persistence, callbacks: make(map[string]*registerCallbackEntry)}
	body := `{"job_id":"register-active","account":{"provider":"grok","password":"callback-secret","credentials":{"accessToken":"upstream-secret"}}}`

	first := performRegisterHandlerRequest(handler.ResultCallback, http.MethodPost, "/api/register/result", body)
	require.Equal(t, http.StatusInternalServerError, first.Code, first.Body.String())
	require.NotContains(t, first.Body.String(), "must-not-leak")
	require.NotContains(t, first.Body.String(), "callback-secret")
	require.NotContains(t, first.Body.String(), "upstream-secret")

	persistence.err = nil
	persistence.result = &service.RegisterPersistResult{Account: &service.Account{
		ID: 75, Name: "registered", Platform: service.PlatformGrok, Status: service.StatusActive,
	}}
	second := performRegisterHandlerRequest(handler.ResultCallback, http.MethodPost, "/api/register/result", body)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	require.Equal(t, 2, persistence.callCount())
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
