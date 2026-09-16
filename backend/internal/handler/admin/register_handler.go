package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/luminovaa/neonix-gateway-go/internal/util/logredact"
)

const (
	registerRequestBodyLimit   = 2 << 20
	registerCallbackCacheLimit = 20000
)

type registerRuntime interface {
	Start(context.Context, service.PythonRegisterStartRequest) (*service.PythonRegisterJobStatus, error)
	Status(context.Context, string) (*service.PythonRegisterJobStatus, error)
	Logs(context.Context, string) (*service.PythonRegisterLogs, error)
	Cancel(context.Context, string) (*service.PythonRegisterCancelResult, error)
	BFSLockout(context.Context) (*service.PythonRegisterBFSLockout, error)
	RuntimeStatus(context.Context) service.PythonRegisterRuntimeStatus
	Capabilities(context.Context) (service.PythonBrowserCapabilities, error)
}

type registerProxyLister interface {
	ListActive(context.Context) ([]service.Proxy, error)
}

type registerAccountPersistence interface {
	PersistForJobID(context.Context, json.RawMessage, string, string) (*service.RegisterPersistResult, error)
	ListGrokReloginCandidates(context.Context) ([]service.PythonRegisterAccount, error)
	GrokReloginCandidateSummary(context.Context) (service.RegisterReloginCandidateSummary, error)
	ListQoderInjectCandidates(context.Context) ([]service.PythonRegisterAccount, error)
	QoderInjectCandidateSummary(context.Context) (service.RegisterQoderInjectCandidateSummary, error)
	ListCodeBuddyChinaClaimCandidates(context.Context) ([]service.PythonRegisterAccount, error)
	ListGitHubPickerAccounts(context.Context) ([]service.RegisterGitHubPickerAccount, error)
	ReserveGitHubAccounts(context.Context, []string, string) ([]service.PythonRegisterAccount, error)
	ReleaseGitHubReservations(context.Context, string, string, string) error
}

type RegisterHandler struct {
	runtime     registerRuntime
	proxies     registerProxyLister
	persistence registerAccountPersistence
	internalKey string

	mu            sync.Mutex
	activeJobID   string
	activeJobType string
	callbacks     map[string]*registerCallbackEntry
	terminalJobs  map[string]registerTerminalJob
}

type registerTerminalJob struct {
	jobType   string
	expiresAt time.Time
}

type registerCallbackResult struct {
	Created bool
	Account registerPublicAccount
}

type registerPublicAccount struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Status   string `json:"status"`
}

type registerCallbackEntry struct {
	done      chan struct{}
	jobType   string
	finished  bool
	expiresAt time.Time
	result    registerCallbackResult
	err       error
}

func NewRegisterHandler(runtime *service.PythonRegisterRuntime, proxies *service.ProxyService, persistence *service.RegisterPersistence) *RegisterHandler {
	return &RegisterHandler{runtime: runtime, proxies: proxies, persistence: persistence, internalKey: firstRegisterEnv("PYAUTO_INTERNAL_API_KEY"), callbacks: make(map[string]*registerCallbackEntry), terminalJobs: make(map[string]registerTerminalJob)}
}

func newRegisterHandlerForTest(runtime registerRuntime, proxies registerProxyLister) *RegisterHandler {
	return &RegisterHandler{runtime: runtime, proxies: proxies, callbacks: make(map[string]*registerCallbackEntry), terminalJobs: make(map[string]registerTerminalJob)}
}

func (h *RegisterHandler) InternalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := strings.TrimSpace(c.GetHeader("x-api-key"))
		if h == nil || h.internalKey == "" || key == "" || len(key) > 4096 || subtle.ConstantTimeCompare([]byte(key), []byte(h.internalKey)) != 1 {
			response.ErrorFrom(c, infraerrors.Unauthorized("REGISTER_CALLBACK_UNAUTHORIZED", "internal service authentication required"))
			c.Abort()
			return
		}
		c.Next()
	}
}

func (h *RegisterHandler) ResultCallback(c *gin.Context) {
	var request struct {
		JobID   string          `json:"job_id"`
		Account json.RawMessage `json:"account"`
	}
	if err := decodeRegisterJSON(c, &request); err != nil || !service.ValidRegisterJobID(request.JobID) || len(request.Account) == 0 {
		writeRegisterError(c, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "invalid register result callback"))
		return
	}
	entry, owner, err := h.beginCallback(request.JobID, request.Account)
	if err != nil {
		writeRegisterError(c, err)
		return
	}
	if !owner {
		select {
		case <-entry.done:
			if entry.err != nil {
				writeRegisterError(c, entry.err)
				return
			}
			writeRegisterCallbackResponse(c, entry.result)
		case <-c.Request.Context().Done():
			writeRegisterError(c, infraerrors.New(http.StatusRequestTimeout, "REGISTER_CALLBACK_WAIT_CANCELLED", "register callback wait was cancelled"))
		}
		return
	}
	if h.persistence == nil {
		err = infraerrors.New(http.StatusServiceUnavailable, "REGISTER_CALLBACK_PERSIST_FAILED", "register account persistence is unavailable")
		h.finishCallback(request.JobID, request.Account, entry, registerCallbackResult{}, err)
		writeRegisterError(c, err)
		return
	}
	result, err := h.persistence.PersistForJobID(c.Request.Context(), request.Account, entry.jobType, request.JobID)
	if err != nil {
		h.finishCallback(request.JobID, request.Account, entry, registerCallbackResult{}, err)
		writeRegisterError(c, err)
		return
	}
	if result == nil || result.Account == nil {
		err = infraerrors.New(http.StatusInternalServerError, "REGISTER_CALLBACK_PERSIST_FAILED", "register account persistence returned an invalid result")
		h.finishCallback(request.JobID, request.Account, entry, registerCallbackResult{}, err)
		writeRegisterError(c, err)
		return
	}
	publicAccount := registerPublicAccount{ID: result.Account.ID, Name: result.Account.Name, Platform: result.Account.Platform, Status: result.Account.Status}
	callbackResult := registerCallbackResult{Created: result.Created, Account: publicAccount}
	h.finishCallback(request.JobID, request.Account, entry, callbackResult, nil)
	writeRegisterCallbackResponse(c, callbackResult)
}

func (h *RegisterHandler) FailureCallback(c *gin.Context) {
	var request struct {
		JobID string `json:"job_id"`
		Email string `json:"email"`
		Error string `json:"error"`
	}
	if err := decodeRegisterJSON(c, &request); err != nil || !service.ValidRegisterJobID(request.JobID) || len(request.Email) > 320 || len(request.Error) > 8192 {
		writeRegisterError(c, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "invalid register failure callback"))
		return
	}
	if !h.isActiveJob(request.JobID) {
		writeRegisterError(c, infraerrors.Conflict("REGISTER_CALLBACK_INVALID", "register callback does not belong to the active job"))
		return
	}
	if h.persistence != nil {
		if err := h.persistence.ReleaseGitHubReservations(c.Request.Context(), request.JobID, strings.TrimSpace(request.Email), "REGISTER_GITHUB_LINK_FAILED"); err != nil {
			writeRegisterError(c, err)
			return
		}
	}
	// The worker already keeps this failure in its bounded job status. Do not
	// echo or log provider error text because it can contain credentials.
	response.Success(c, gin.H{"ok": true})
}

type registerStartRequest struct {
	Type            string                    `json:"type"`
	RegisterConfig  *registerConfigRequest    `json:"register_config"`
	SubscribeConfig *registerSubscribeRequest `json:"subscribe_config"`
}

type registerConfigRequest struct {
	TargetProvider string `json:"target_provider"`
	RegisterMethod string `json:"register_method"`
	BrowserEngine  string `json:"browser_engine"`
	UseProxy       *bool  `json:"use_proxy"`
	// Proxy is accepted for compatibility with the current Neonix web client,
	// which sends proxy:null. The control plane deliberately ignores its value
	// and resolves worker proxies from the server-owned proxy pool.
	Proxy             *string                 `json:"proxy"`
	GoogleAccounts    []registerGoogleAccount `json:"google_accounts"`
	GitHubAccountIDs  []string                `json:"github_account_ids"`
	Count             int                     `json:"count"`
	Headless          *bool                   `json:"headless"`
	Concurrency       int                     `json:"concurrency"`
	MaxRetries        int                     `json:"max_retries"`
	Mailbox           *registerMailboxRequest `json:"mailbox"`
	GitHubCountry     string                  `json:"github_country"`
	GitHubCreateRepo  *bool                   `json:"github_create_repo"`
	QoderClaim        *bool                   `json:"qoder_claim"`
	QoderResetMachine *bool                   `json:"qoder_reset_machine"`
	QoderForceLocal   *bool                   `json:"qoder_force_local"`
	SMSProvider       string                  `json:"sms_provider"`
	FiveSIMCountry    string                  `json:"fivesim_country"`
	HeroSMSCountry    string                  `json:"herosms_country"`
	CBCNAction        string                  `json:"cbcn_action"`
	OutlookCountry    string                  `json:"outlook_country"`
	OutlookDomain     string                  `json:"outlook_domain"`
}

type registerGoogleAccount struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type registerMailboxRequest struct {
	Domain string `json:"domain"`
}

type registerSubscribeRequest struct {
	SubscriptionType string `json:"subscription_type"`
	UseProxy         *bool  `json:"use_proxy"`
	// See registerConfigRequest.Proxy. Subscription proxy selection is also
	// server-owned; this field only keeps the strict decoder wire-compatible.
	Proxy      *string `json:"proxy"`
	Headless   *bool   `json:"headless"`
	AccountID  string  `json:"account_id"`
	Region     string  `json:"region"`
	ProfileARN string  `json:"profile_arn"`
	MachineID  string  `json:"machine_id"`
	HolderName string  `json:"holder_name"`
}

func (h *RegisterHandler) Start(c *gin.Context) {
	var request registerStartRequest
	if err := decodeRegisterJSON(c, &request); err != nil {
		writeRegisterError(c, infraerrors.BadRequest("REGISTER_PAYLOAD_INVALID", "invalid register request"))
		return
	}
	if request.Type == "" {
		request.Type = "register"
	}
	if request.Type != "register" && request.Type != "register_subscribe" && request.Type != "relogin" && request.Type != "inject" {
		writeRegisterError(c, infraerrors.BadRequest("REGISTER_FEATURE_NOT_MIGRATED", "this automation job is not available in the Go backend yet"))
		return
	}

	if h == nil || h.runtime == nil {
		writeRegisterError(c, serviceRuntimeUnavailable())
		return
	}
	h.mu.Lock()
	if h.activeJobID != "" {
		h.mu.Unlock()
		writeRegisterError(c, infraerrors.Conflict("REGISTER_JOB_CONFLICT", "another registration job is already active"))
		return
	}
	jobID, err := newRegisterJobID()
	if err != nil {
		h.mu.Unlock()
		writeRegisterError(c, infraerrors.New(http.StatusInternalServerError, "REGISTER_JOB_ID_FAILED", "failed to create register job"))
		return
	}
	h.activeJobID = jobID
	h.activeJobType = request.Type
	h.mu.Unlock()
	workerRequest, err := h.buildWorkerRequest(c.Request.Context(), jobID, request)
	if err != nil {
		h.releaseGitHubReservations(c.Request.Context(), jobID, "REGISTER_JOB_START_FAILED")
		h.clearActiveJob(jobID)
		writeRegisterError(c, err)
		return
	}
	status, err := h.runtime.Start(c.Request.Context(), workerRequest)
	if err != nil {
		h.releaseGitHubReservations(c.Request.Context(), jobID, "REGISTER_JOB_START_FAILED")
		h.clearActiveJob(jobID)
		writeRegisterError(c, err)
		return
	}
	if status == nil || status.JobID != jobID {
		h.releaseGitHubReservations(c.Request.Context(), jobID, "REGISTER_RUNTIME_PROTOCOL_INVALID")
		h.clearActiveJob(jobID)
		writeRegisterError(c, serviceRuntimeProtocolInvalid())
		return
	}
	if isRegisterTerminalStatus(status.Status) {
		reason := "REGISTER_GITHUB_LINK_INCOMPLETE"
		if status.Status != "done" {
			reason = "REGISTER_JOB_TERMINATED"
		}
		h.releaseGitHubReservations(c.Request.Context(), jobID, reason)
		h.clearActiveJob(jobID)
	}
	response.Success(c, gin.H{"job_id": jobID, "status": status.Status})
}

func (h *RegisterHandler) Status(c *gin.Context) {
	jobID := strings.TrimSpace(c.Query("job_id"))
	if err := h.requireActiveJob(jobID); err != nil {
		writeRegisterError(c, err)
		return
	}
	status, err := h.runtime.Status(c.Request.Context(), jobID)
	if err != nil {
		if service.RegisterRuntimeErrorCode(err) == "REGISTER_JOB_NOT_FOUND" {
			h.releaseGitHubReservations(c.Request.Context(), jobID, "REGISTER_JOB_NOT_FOUND")
			h.clearActiveJob(jobID)
		}
		writeRegisterError(c, err)
		return
	}
	if status == nil || status.JobID != jobID {
		writeRegisterError(c, serviceRuntimeProtocolInvalid())
		return
	}
	if isRegisterTerminalStatus(status.Status) {
		reason := "REGISTER_GITHUB_LINK_INCOMPLETE"
		if status.Status != "done" {
			reason = "REGISTER_JOB_TERMINATED"
		}
		h.releaseGitHubReservations(c.Request.Context(), jobID, reason)
		h.clearActiveJob(jobID)
	}
	response.Success(c, publicRegisterStatus(status))
}

func (h *RegisterHandler) Logs(c *gin.Context) {
	jobID := strings.TrimSpace(c.Query("job_id"))
	if err := h.requireActiveJob(jobID); err != nil {
		writeRegisterError(c, err)
		return
	}
	logs, err := h.runtime.Logs(c.Request.Context(), jobID)
	if err != nil {
		if service.RegisterRuntimeErrorCode(err) == "REGISTER_JOB_NOT_FOUND" {
			h.releaseGitHubReservations(c.Request.Context(), jobID, "REGISTER_JOB_NOT_FOUND")
			h.clearActiveJob(jobID)
		}
		writeRegisterError(c, err)
		return
	}
	response.Success(c, gin.H{"logs": sanitizeRegisterLogs(logs.Logs)})
}

func (h *RegisterHandler) Cancel(c *gin.Context) {
	var request struct {
		JobID string `json:"job_id"`
	}
	if err := decodeRegisterJSON(c, &request); err != nil || strings.TrimSpace(request.JobID) == "" {
		writeRegisterError(c, infraerrors.BadRequest("REGISTER_PAYLOAD_INVALID", "job_id is required"))
		return
	}
	request.JobID = strings.TrimSpace(request.JobID)
	if err := h.requireActiveJob(request.JobID); err != nil {
		writeRegisterError(c, err)
		return
	}
	result, err := h.runtime.Cancel(c.Request.Context(), request.JobID)
	if err != nil {
		if service.RegisterRuntimeErrorCode(err) == "REGISTER_JOB_NOT_FOUND" {
			h.clearActiveJob(request.JobID)
		}
		writeRegisterError(c, err)
		return
	}
	if result.Cancelled {
		h.releaseGitHubReservations(c.Request.Context(), request.JobID, "REGISTER_JOB_CANCELLED")
		h.clearActiveJob(request.JobID)
	}
	response.Success(c, gin.H{"ok": result.Cancelled, "cancelled": result.Cancelled})
}

func (h *RegisterHandler) BFSLockout(c *gin.Context) {
	if h == nil || h.runtime == nil {
		writeRegisterError(c, serviceRuntimeUnavailable())
		return
	}
	result, err := h.runtime.BFSLockout(c.Request.Context())
	if err != nil {
		writeRegisterError(c, err)
		return
	}
	response.Success(c, result)
}

func (h *RegisterHandler) PythonStatus(c *gin.Context) {
	if h == nil || h.runtime == nil {
		response.Success(c, gin.H{
			"running": false, "healthy": false, "manager_configured": false,
			"manager_running": false, "child_running": false, "child_ready": false, "leases": 0,
		})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	status := h.runtime.RuntimeStatus(ctx)
	response.Success(c, gin.H{
		"running": status.ChildRunning, "healthy": status.ChildReady,
		"manager_configured": status.ManagerConfigured, "manager_running": status.ManagerRunning,
		"child_running": status.ChildRunning, "child_ready": status.ChildReady, "leases": status.Leases,
	})
}

func (h *RegisterHandler) SystemStatus(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 4*time.Second)
	defer cancel()
	status := service.PythonRegisterRuntimeStatus{}
	capabilities := unavailableBrowserCapabilities("Python automation service unavailable")
	if h != nil && h.runtime != nil {
		status = h.runtime.RuntimeStatus(ctx)
		if status.ChildReady {
			if value, err := h.runtime.Capabilities(ctx); err == nil {
				capabilities = value
			}
		}
	}
	proxyCount := 0
	if h != nil {
		if proxies, err := h.activeProxyURLs(ctx); err == nil {
			proxyCount = len(proxies)
		}
	}
	response.Success(c, gin.H{
		"pythonHealthy": status.ChildReady, "proxyPoolCount": proxyCount,
		"mailConfigured":         firstRegisterEnv("YYDSMAIL_API_KEY", "MAIL_API_KEY") != "",
		"captchaConfigured":      firstRegisterEnv("YESCAPTCHA_API_KEY", "YES_CAPTCHA_API_KEY", "CAPTCHA_API_KEY") != "",
		"subscriptionConfigured": firstRegisterEnv("SUBSCRIPTION_BIN", "PYAUTO_SUBSCRIPTION_BIN", "BIN") != "",
		"fivesimConfigured":      firstRegisterEnv("FIVESIM_API_KEY") != "",
		"herosmsConfigured":      firstRegisterEnv("HEROSMS_API_KEY") != "",
		"mailBaseUrl":            stringDefault(firstRegisterEnv("YYDSMAIL_BASE_URL", "MAIL_BASE_URL"), "https://maliapi.215.im/v1"),
		"captchaProvider":        "yescaptcha", "browserEngines": capabilities,
	})
}

func unavailableBrowserCapabilities(reason string) service.PythonBrowserCapabilities {
	result := make(service.PythonBrowserCapabilities, 2)
	for _, engine := range []string{"camoufox", "cloakbrowser"} {
		result[engine] = service.PythonBrowserCapability{Reason: reason}
	}
	return result
}

type registerSMSCountry struct {
	Country string  `json:"country"`
	Name    string  `json:"name"`
	Cost    float64 `json:"cost"`
	Count   int     `json:"count"`
}

var registerSMSHTTPClient = &http.Client{Timeout: 8 * time.Second}
var fiveSIMPricesURL = "https://5sim.net/v1/guest/prices?product=codebuddy"
var heroSMSAPIURL = "https://hero-sms.com/stubs/handler_api.php"

func (h *RegisterHandler) SMSPrices(c *gin.Context) {
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	if provider == "" {
		provider = "fivesim"
	}
	var countries []registerSMSCountry
	var err error
	switch provider {
	case "fivesim":
		countries, err = fetchFiveSIMPrices(c.Request.Context())
	case "herosms":
		countries, err = fetchHeroSMSPrices(c.Request.Context())
	default:
		err = infraerrors.BadRequest("REGISTER_SMS_PROVIDER_INVALID", "unsupported SMS provider")
	}
	if err != nil {
		writeRegisterError(c, err)
		return
	}
	c.Header("Cache-Control", "private, max-age=60")
	response.Success(c, gin.H{"provider": provider, "countries": countries})
}

func fetchFiveSIMPrices(ctx context.Context) ([]registerSMSCountry, error) {
	if firstRegisterEnv("FIVESIM_API_KEY") == "" {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_SMS_NOT_CONFIGURED", "5SIM is not configured on the backend")
	}
	var raw struct {
		CodeBuddy map[string]map[string]struct {
			Cost  float64 `json:"cost"`
			Count int     `json:"count"`
		} `json:"codebuddy"`
	}
	if err := fetchRegisterSMSJSON(ctx, fiveSIMPricesURL, &raw); err != nil {
		return nil, err
	}
	result := make([]registerSMSCountry, 0, len(raw.CodeBuddy))
	for country, operators := range raw.CodeBuddy {
		chosen, ok := operators["any"]
		if !ok || chosen.Count <= 0 {
			ok = false
			for _, item := range operators {
				if item.Count > 0 && (!ok || item.Cost < chosen.Cost) {
					chosen, ok = item, true
				}
			}
		}
		if ok {
			result = append(result, registerSMSCountry{Country: country, Name: country, Cost: chosen.Cost, Count: chosen.Count})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Country < result[j].Country })
	return result, nil
}

func fetchHeroSMSPrices(ctx context.Context) ([]registerSMSCountry, error) {
	apiKey := firstRegisterEnv("HEROSMS_API_KEY")
	if apiKey == "" {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_SMS_NOT_CONFIGURED", "HeroSMS is not configured on the backend")
	}
	serviceName := stringDefault(firstRegisterEnv("HEROSMS_SERVICE"), "ot")
	pricesURL := heroSMSAPIURL + "?" + url.Values{"api_key": {apiKey}, "action": {"getPrices"}, "service": {serviceName}}.Encode()
	namesURL := heroSMSAPIURL + "?" + url.Values{"api_key": {apiKey}, "action": {"getCountries"}}.Encode()
	var prices map[string]map[string]struct {
		Cost  float64 `json:"cost"`
		Count int     `json:"count"`
	}
	if err := fetchRegisterSMSJSON(ctx, pricesURL, &prices); err != nil {
		return nil, err
	}
	var countryRows map[string]struct {
		ID      any    `json:"id"`
		English string `json:"eng"`
	}
	_ = fetchRegisterSMSJSON(ctx, namesURL, &countryRows)
	names := map[string]string{}
	for _, row := range countryRows {
		names[fmt.Sprint(row.ID)] = row.English
	}
	result := make([]registerSMSCountry, 0, len(prices))
	for country, services := range prices {
		item, ok := services[serviceName]
		if !ok || item.Count <= 0 {
			continue
		}
		name := strings.TrimSpace(names[country])
		if name == "" {
			name = country
		}
		result = append(result, registerSMSCountry{Country: country, Name: name, Cost: item.Cost, Count: item.Count})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func fetchRegisterSMSJSON(ctx context.Context, target string, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return infraerrors.New(http.StatusBadGateway, "REGISTER_SMS_PRICES_FAILED", "SMS price catalog is unavailable")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := registerSMSHTTPClient.Do(req)
	if err != nil {
		return infraerrors.New(http.StatusBadGateway, "REGISTER_SMS_PRICES_FAILED", "SMS price catalog is unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return infraerrors.New(http.StatusBadGateway, "REGISTER_SMS_PRICES_FAILED", "SMS price catalog is unavailable")
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(output); err != nil {
		return infraerrors.New(http.StatusBadGateway, "REGISTER_SMS_PRICES_FAILED", "SMS price catalog returned an invalid response")
	}
	return nil
}

func (h *RegisterHandler) GrokExpiredCount(c *gin.Context) {
	if h == nil || h.persistence == nil {
		writeRegisterError(c, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable"))
		return
	}
	summary, err := h.persistence.GrokReloginCandidateSummary(c.Request.Context())
	if err != nil {
		writeRegisterError(c, err)
		return
	}
	response.Success(c, gin.H{"count": summary.Count, "totalGrok": summary.TotalProvider})
}

func (h *RegisterHandler) QoderInjectableCount(c *gin.Context) {
	if h == nil || h.persistence == nil {
		writeRegisterError(c, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable"))
		return
	}
	summary, err := h.persistence.QoderInjectCandidateSummary(c.Request.Context())
	if err != nil {
		writeRegisterError(c, err)
		return
	}
	response.Success(c, gin.H{"count": summary.Count, "totalQoder": summary.TotalProvider})
}

func (h *RegisterHandler) GitHubAccounts(c *gin.Context) {
	if h == nil || h.persistence == nil {
		writeRegisterError(c, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable"))
		return
	}
	accounts, err := h.persistence.ListGitHubPickerAccounts(c.Request.Context())
	if err != nil {
		writeRegisterError(c, err)
		return
	}
	response.Success(c, gin.H{"accounts": accounts})
}

func (h *RegisterHandler) buildWorkerRequest(ctx context.Context, jobID string, request registerStartRequest) (service.PythonRegisterStartRequest, error) {
	if h == nil || h.runtime == nil {
		return service.PythonRegisterStartRequest{}, serviceRuntimeUnavailable()
	}
	if request.Type == "relogin" {
		config, err := h.buildGrokReloginConfig(ctx, request.RegisterConfig)
		if err != nil {
			return service.PythonRegisterStartRequest{}, err
		}
		return service.PythonRegisterStartRequest{
			JobID: jobID, Type: "relogin", RegisterConfig: config, Provider: "grok",
		}, nil
	}
	if request.Type == "inject" {
		config, err := h.buildQoderInjectConfig(ctx, request.RegisterConfig)
		if err != nil {
			return service.PythonRegisterStartRequest{}, err
		}
		return service.PythonRegisterStartRequest{
			JobID: jobID, Type: "inject", RegisterConfig: config, Provider: "qoder",
		}, nil
	}
	if request.RegisterConfig == nil {
		return service.PythonRegisterStartRequest{}, infraerrors.BadRequest("REGISTER_PAYLOAD_INVALID", "register_config is required")
	}
	config, err := h.buildRegisterConfig(ctx, *request.RegisterConfig, jobID)
	if err != nil {
		return service.PythonRegisterStartRequest{}, err
	}
	result := service.PythonRegisterStartRequest{JobID: jobID, Type: request.Type, RegisterConfig: config, Provider: "BuilderId"}
	if request.Type == "register_subscribe" {
		subscription, err := h.buildSubscribeConfig(ctx, request.SubscribeConfig)
		if err != nil {
			return service.PythonRegisterStartRequest{}, err
		}
		result.SubscribeConfig = subscription
	}
	return result, nil
}

func (h *RegisterHandler) buildGrokReloginConfig(ctx context.Context, raw *registerConfigRequest) (*service.PythonRegisterConfig, error) {
	if h == nil || h.persistence == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
	}
	if raw == nil {
		raw = &registerConfigRequest{}
	}
	provider := strings.ToLower(strings.TrimSpace(raw.TargetProvider))
	if provider == "" {
		provider = "grok"
	}
	if provider != "grok" {
		return nil, infraerrors.BadRequest("REGISTER_FEATURE_NOT_MIGRATED", "re-login is currently available only for Grok")
	}
	concurrency := raw.Concurrency
	if concurrency == 0 {
		concurrency = 1
	}
	maxRetries := raw.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	if concurrency < 1 || concurrency > 100 || maxRetries < 1 || maxRetries > 10 {
		return nil, infraerrors.BadRequest("REGISTER_LIMIT_INVALID", "invalid concurrency or retry limit")
	}
	headless := true
	if raw.Headless != nil {
		headless = *raw.Headless
	}
	accounts, err := h.persistence.ListGrokReloginCandidates(ctx)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, infraerrors.NotFound("REGISTER_RELOGIN_ACCOUNT_NOT_FOUND", "no Grok account is eligible for re-login")
	}
	config := &service.PythonRegisterConfig{
		TargetProvider: "grok", RegisterMethod: "http", ReloginAccounts: accounts,
		Headless: headless, Concurrency: concurrency, MaxRetries: maxRetries,
	}
	if boolDefault(raw.UseProxy, true) {
		proxies, err := h.activeProxyURLs(ctx)
		if err != nil {
			return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_PROXY_LOAD_FAILED", "failed to load the proxy pool")
		}
		config.Proxies = proxies
		if len(proxies) > 0 {
			config.Proxy = proxies[0]
		}
	}
	return config, nil
}

func (h *RegisterHandler) buildQoderInjectConfig(ctx context.Context, raw *registerConfigRequest) (*service.PythonRegisterConfig, error) {
	if h == nil || h.persistence == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
	}
	if raw == nil {
		raw = &registerConfigRequest{}
	}
	provider := strings.ToLower(strings.TrimSpace(raw.TargetProvider))
	if provider == "" {
		provider = "qoder"
	}
	if provider != "qoder" {
		return nil, infraerrors.BadRequest("REGISTER_FEATURE_NOT_MIGRATED", "inject is currently available only for Qoder")
	}
	accounts, err := h.persistence.ListQoderInjectCandidates(ctx)
	if err != nil {
		return nil, err
	}
	accounts = boundRegisterWorkerAccounts(accounts, registerRequestBodyLimit-(256<<10))
	if len(accounts) == 0 {
		return nil, infraerrors.NotFound("REGISTER_INJECT_ACCOUNT_NOT_FOUND", "no Qoder account is eligible for inject")
	}
	config := &service.PythonRegisterConfig{
		TargetProvider: "qoder", RegisterMethod: "http", InjectAccounts: accounts,
		Headless: true, Concurrency: 1, Count: len(accounts), QoderInjectTrial: true,
		QoderForceLocal: boolDefault(raw.QoderForceLocal, true),
	}
	if boolDefault(raw.UseProxy, true) {
		proxies, err := h.activeProxyURLs(ctx)
		if err != nil {
			return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_PROXY_LOAD_FAILED", "failed to load the proxy pool")
		}
		config.Proxies = proxies
		config.QoderProxyPool = len(proxies) > 0
		if len(proxies) > 0 {
			config.Proxy = proxies[0]
		}
	}
	return config, nil
}

func boundRegisterWorkerAccounts(accounts []service.PythonRegisterAccount, budget int) []service.PythonRegisterAccount {
	if len(accounts) == 0 || budget <= 0 {
		return nil
	}
	result := make([]service.PythonRegisterAccount, 0, len(accounts))
	used := 2 // JSON array brackets.
	for i := range accounts {
		encoded, err := json.Marshal(accounts[i])
		if err != nil {
			continue
		}
		required := len(encoded)
		if len(result) > 0 {
			required++
		}
		if used+required > budget {
			break
		}
		result = append(result, accounts[i])
		used += required
	}
	return result
}

func (h *RegisterHandler) buildRegisterConfig(ctx context.Context, raw registerConfigRequest, jobID string) (*service.PythonRegisterConfig, error) {
	provider := strings.ToLower(strings.TrimSpace(raw.TargetProvider))
	if provider == "" {
		provider = "kiro"
	}
	if provider == "bai" || provider == "bb" || provider == "codebuff" {
		return nil, infraerrors.BadRequest("REGISTER_PROVIDER_DEPRECATED", "the selected provider is deprecated")
	}
	if !allowedRegisterProvider(provider) {
		return nil, infraerrors.BadRequest("REGISTER_PROVIDER_INVALID", "unsupported register provider")
	}
	method := strings.ToLower(strings.TrimSpace(raw.RegisterMethod))
	if method == "" {
		if len(raw.GoogleAccounts) > 0 {
			method = "google"
		} else {
			method = "email"
		}
	}
	if !allowedRegisterMethod(method) {
		return nil, infraerrors.BadRequest("REGISTER_METHOD_INVALID", "unsupported register method")
	}
	if err := validateRegisterProviderMethod(provider, method); err != nil {
		return nil, err
	}
	if provider == "codebuddy" && method == "github" && len(raw.GitHubAccountIDs) == 0 {
		return nil, infraerrors.BadRequest("REGISTER_GITHUB_ACCOUNTS_REQUIRED", "select at least one GitHub identity")
	}
	if method == "google" || method == "twitter" {
		if len(raw.GoogleAccounts) == 0 {
			return nil, infraerrors.BadRequest("REGISTER_GOOGLE_ACCOUNTS_REQUIRED", "Google accounts are required for this registration method")
		}
	}
	if len(raw.GoogleAccounts) > 20000 {
		return nil, infraerrors.BadRequest("REGISTER_BATCH_TOO_LARGE", "too many Google accounts")
	}
	if len(raw.GitHubAccountIDs) > 20000 {
		return nil, infraerrors.BadRequest("REGISTER_BATCH_TOO_LARGE", "too many GitHub identities")
	}
	googleAccounts := make([]service.PythonRegisterAccount, 0, len(raw.GoogleAccounts))
	for _, account := range raw.GoogleAccounts {
		email := strings.TrimSpace(account.Email)
		if email == "" || len(email) > 320 || account.Password == "" || len(account.Password) > 4096 {
			return nil, infraerrors.BadRequest("REGISTER_GOOGLE_ACCOUNT_INVALID", "invalid Google account entry")
		}
		googleAccounts = append(googleAccounts, service.PythonRegisterAccount{Email: email, Password: account.Password})
	}

	count := raw.Count
	if count == 0 {
		count = 1
	}
	concurrency := raw.Concurrency
	if concurrency == 0 {
		concurrency = 1
	}
	maxRetries := raw.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	if count < 1 || count > 20000 || concurrency < 1 || concurrency > 100 || maxRetries < 1 || maxRetries > 10 {
		return nil, infraerrors.BadRequest("REGISTER_LIMIT_INVALID", "invalid count, concurrency, or retry limit")
	}
	engine := strings.TrimSpace(raw.BrowserEngine)
	if engine == "" {
		engine = "camoufox"
	}
	if engine != "camoufox" && engine != "cloakbrowser" && engine != "obscura" {
		return nil, infraerrors.BadRequest("REGISTER_BROWSER_INVALID", "unsupported browser engine")
	}

	headless := true
	if raw.Headless != nil {
		headless = *raw.Headless
	}
	config := &service.PythonRegisterConfig{
		TargetProvider: provider, RegisterMethod: method, BrowserEngine: engine, Count: count, Headless: headless,
		Concurrency: concurrency, MaxRetries: maxRetries, GoogleAccounts: googleAccounts, GitHubCountry: boundedString(raw.GitHubCountry, 80),
		GitHubCreateRepo: boolDefault(raw.GitHubCreateRepo, true), QoderClaim: boolDefault(raw.QoderClaim, true),
		QoderResetMachine: boolDefault(raw.QoderResetMachine, true), QoderForceLocal: boolDefault(raw.QoderForceLocal, true),
	}

	if boolDefault(raw.UseProxy, true) {
		proxies, err := h.activeProxyURLs(ctx)
		if err != nil {
			return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_PROXY_LOAD_FAILED", "failed to load the proxy pool")
		}
		config.Proxies = proxies
		if len(proxies) > 0 {
			config.Proxy = proxies[0]
		}
	}

	if provider == "outlook" {
		if len(googleAccounts) > 0 {
			return nil, infraerrors.BadRequest("REGISTER_PAYLOAD_INVALID", "Outlook registration does not accept Google accounts")
		}
		config.RegisterMethod = "email"
		config.OutlookCountry = stringDefault(boundedString(raw.OutlookCountry, 10), "US")
		config.OutlookDomain = stringDefault(boundedString(raw.OutlookDomain, 80), "@outlook.com")
		return config, nil
	}
	if provider == "codebuddy-china" {
		if len(googleAccounts) > 0 {
			return nil, infraerrors.BadRequest("REGISTER_PAYLOAD_INVALID", "CodeBuddy China registration does not accept Google accounts")
		}
		cbcnAction := strings.ToLower(strings.TrimSpace(raw.CBCNAction))
		if cbcnAction == "" {
			cbcnAction = "register"
		}
		if cbcnAction != "register" && cbcnAction != "claim" {
			return nil, infraerrors.BadRequest("REGISTER_CBCN_ACTION_INVALID", "unsupported CodeBuddy China action")
		}
		if cbcnAction == "claim" {
			if h.persistence == nil {
				return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
			}
			accounts, err := h.persistence.ListCodeBuddyChinaClaimCandidates(ctx)
			if err != nil {
				return nil, err
			}
			if len(accounts) == 0 {
				return nil, infraerrors.NotFound("REGISTER_CBCN_CLAIM_ACCOUNT_NOT_FOUND", "no CodeBuddy China account has a refresh token")
			}
			bounded := boundRegisterWorkerAccounts(accounts, service.RegisterRuntimeBodyLimit/2)
			if len(bounded) != len(accounts) {
				return nil, infraerrors.BadRequest("REGISTER_BATCH_TOO_LARGE", "CodeBuddy China claim accounts exceed the worker request limit")
			}
			config.RegisterMethod = "sms"
			config.CBCNAction = "claim"
			config.CBCNAccounts = bounded
			config.Count = len(bounded)
			return config, nil
		}
		config.RegisterMethod = "sms"
		config.CBCNAction = "register"
		config.SMSProvider = stringDefault(raw.SMSProvider, "fivesim")
		switch config.SMSProvider {
		case "fivesim":
			config.FiveSIMAPIKey = firstRegisterEnv("FIVESIM_API_KEY")
			if config.FiveSIMAPIKey == "" {
				return nil, infraerrors.BadRequest("REGISTER_SMS_NOT_CONFIGURED", "5SIM is not configured on the backend")
			}
			config.FiveSIMCountry = stringDefault(boundedString(raw.FiveSIMCountry, 40), "hongkong")
			config.FiveSIMService = "codebuddy"
		case "herosms":
			config.HeroSMSAPIKey = firstRegisterEnv("HEROSMS_API_KEY")
			if config.HeroSMSAPIKey == "" {
				return nil, infraerrors.BadRequest("REGISTER_SMS_NOT_CONFIGURED", "HeroSMS is not configured on the backend")
			}
			config.HeroSMSCountry = stringDefault(boundedString(raw.HeroSMSCountry, 8), "60")
			config.HeroSMSService = stringDefault(firstRegisterEnv("HEROSMS_SERVICE"), "ot")
		default:
			return nil, infraerrors.BadRequest("REGISTER_SMS_PROVIDER_INVALID", "unsupported SMS provider")
		}
		return config, nil
	}

	if method == "email" || provider == "github" || provider == "grok" && method == "http" || provider == "qoder" && method == "http" {
		mailAPIKey := firstRegisterEnv("YYDSMAIL_API_KEY", "MAIL_API_KEY")
		if mailAPIKey == "" {
			return nil, infraerrors.BadRequest("REGISTER_MAILBOX_NOT_CONFIGURED", "mailbox service is not configured on the backend")
		}
		config.Mailbox = &service.PythonRegisterMailboxConfig{
			APIKey: mailAPIKey, BaseURL: stringDefault(firstRegisterEnv("YYDSMAIL_BASE_URL", "MAIL_BASE_URL"), "https://maliapi.215.im/v1"),
		}
		if raw.Mailbox != nil {
			config.Mailbox.Domain = boundedString(raw.Mailbox.Domain, 255)
		}
	}
	if provider == "codebuddy" && method == "github" {
		if h.persistence == nil {
			return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
		}
		accounts, err := h.persistence.ReserveGitHubAccounts(ctx, raw.GitHubAccountIDs, jobID)
		if err != nil {
			return nil, err
		}
		bounded := boundRegisterWorkerAccounts(accounts, service.RegisterRuntimeBodyLimit/2)
		if len(bounded) != len(accounts) {
			h.releaseGitHubReservations(ctx, jobID, "REGISTER_PAYLOAD_TOO_LARGE")
			return nil, infraerrors.BadRequest("REGISTER_BATCH_TOO_LARGE", "selected GitHub identities exceed the worker request limit")
		}
		config.GitHubAccountIDs = append([]string(nil), raw.GitHubAccountIDs...)
		config.GitHubAccounts = bounded
		config.Count = len(bounded)
	}
	return config, nil
}

func (h *RegisterHandler) buildSubscribeConfig(ctx context.Context, raw *registerSubscribeRequest) (*service.PythonRegisterSubscribeConfig, error) {
	bin := firstRegisterEnv("SUBSCRIPTION_BIN", "PYAUTO_SUBSCRIPTION_BIN", "BIN")
	if bin == "" {
		return nil, infraerrors.BadRequest("REGISTER_SUBSCRIPTION_NOT_CONFIGURED", "subscription BIN is not configured on the backend")
	}
	if raw == nil {
		raw = &registerSubscribeRequest{}
	}
	headless := true
	if raw.Headless != nil {
		headless = *raw.Headless
	}
	result := &service.PythonRegisterSubscribeConfig{
		BIN: bin, Region: stringDefault(boundedString(raw.Region, 80), "us-east-1"), HolderName: stringDefault(boundedString(raw.HolderName, 120), "John Smith"),
		SubscriptionType: boundedString(raw.SubscriptionType, 80), AccountID: boundedString(raw.AccountID, 256), ProfileARN: boundedString(raw.ProfileARN, 512),
		MachineID: boundedString(raw.MachineID, 256), Headless: headless, MaxVCCAttempts: 20,
	}
	if captcha := firstRegisterEnv("YESCAPTCHA_API_KEY", "YES_CAPTCHA_API_KEY", "CAPTCHA_API_KEY"); captcha != "" {
		result.Captcha = &service.PythonRegisterCaptchaConfig{APIKey: captcha}
	}
	if boolDefault(raw.UseProxy, true) {
		proxies, err := h.activeProxyURLs(ctx)
		if err != nil {
			return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_PROXY_LOAD_FAILED", "failed to load the proxy pool")
		}
		if len(proxies) > 0 {
			result.Proxy = proxies[0]
		}
	}
	return result, nil
}

func (h *RegisterHandler) activeProxyURLs(ctx context.Context) ([]string, error) {
	if h.proxies == nil {
		return nil, nil
	}
	items, err := h.proxies.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(items))
	for i := range items {
		if items[i].IsExpired(time.Now()) {
			continue
		}
		if value := items[i].URL(); value != "" {
			result = append(result, value)
		}
	}
	return result, nil
}

func (h *RegisterHandler) clearActiveJob(jobID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for terminalJobID, terminal := range h.terminalJobs {
		if !terminal.expiresAt.After(now) {
			delete(h.terminalJobs, terminalJobID)
		}
	}
	for callbackKey, callback := range h.callbacks {
		if callback.finished && !callback.expiresAt.After(now) {
			delete(h.callbacks, callbackKey)
		}
	}
	if h.activeJobID == jobID {
		if h.activeJobType != "" {
			if h.terminalJobs == nil {
				h.terminalJobs = make(map[string]registerTerminalJob)
			}
			h.terminalJobs[jobID] = registerTerminalJob{jobType: h.activeJobType, expiresAt: time.Now().Add(15 * time.Minute)}
		}
		h.activeJobID = ""
		h.activeJobType = ""
	}
}

func (h *RegisterHandler) releaseGitHubReservations(ctx context.Context, jobID, reasonCode string) {
	if h == nil || h.persistence == nil || strings.TrimSpace(jobID) == "" {
		return
	}
	_ = h.persistence.ReleaseGitHubReservations(ctx, jobID, "", reasonCode)
}

func (h *RegisterHandler) isActiveJob(jobID string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return jobID != "" && h.activeJobID == jobID
}

func (h *RegisterHandler) beginCallback(jobID string, account json.RawMessage) (*registerCallbackEntry, bool, error) {
	if h == nil {
		return nil, false, infraerrors.Conflict("REGISTER_CALLBACK_INVALID", "register callback does not belong to the active job")
	}
	key := registerCallbackKey(jobID, account)
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for terminalJobID, terminal := range h.terminalJobs {
		if !terminal.expiresAt.After(now) {
			delete(h.terminalJobs, terminalJobID)
		}
	}
	jobType := ""
	if jobID != "" && h.activeJobID == jobID {
		jobType = h.activeJobType
		if jobType == "" {
			jobType = "register"
		}
	} else if terminal, ok := h.terminalJobs[jobID]; ok {
		jobType = terminal.jobType
	}
	if jobType == "" {
		return nil, false, infraerrors.Conflict("REGISTER_CALLBACK_INVALID", "register callback does not belong to the active job")
	}
	if entry, ok := h.callbacks[key]; ok {
		return entry, false, nil
	}
	if len(h.callbacks) >= registerCallbackCacheLimit {
		return nil, false, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_CALLBACK_LIMIT_REACHED", "register callback limit reached")
	}
	if h.callbacks == nil {
		h.callbacks = make(map[string]*registerCallbackEntry)
	}
	entry := &registerCallbackEntry{done: make(chan struct{}), jobType: jobType}
	h.callbacks[key] = entry
	return entry, true, nil
}

func (h *RegisterHandler) finishCallback(jobID string, account json.RawMessage, entry *registerCallbackEntry, result registerCallbackResult, err error) {
	if h == nil || entry == nil {
		return
	}
	key := registerCallbackKey(jobID, account)
	h.mu.Lock()
	defer h.mu.Unlock()
	current, ok := h.callbacks[key]
	if !ok || current != entry {
		return
	}
	entry.result = result
	entry.err = err
	entry.finished = true
	entry.expiresAt = time.Now().Add(15 * time.Minute)
	close(entry.done)
	if err != nil {
		delete(h.callbacks, key)
	}
}

func writeRegisterCallbackResponse(c *gin.Context, result registerCallbackResult) {
	response.Success(c, gin.H{"ok": true, "created": result.Created, "account": result.Account})
}

func registerCallbackKey(jobID string, account json.RawMessage) string {
	canonical := account
	var decoded any
	if json.Unmarshal(account, &decoded) == nil {
		if encoded, err := json.Marshal(decoded); err == nil {
			canonical = encoded
		}
	}
	payload := make([]byte, 0, len(jobID)+1+len(canonical))
	payload = append(payload, jobID...)
	payload = append(payload, 0)
	payload = append(payload, canonical...)
	value := sha256.Sum256(payload)
	return hex.EncodeToString(value[:])
}

func (h *RegisterHandler) requireActiveJob(jobID string) error {
	if !service.ValidRegisterJobID(jobID) {
		return infraerrors.BadRequest("REGISTER_PAYLOAD_INVALID", "job_id is required")
	}
	if h == nil || h.runtime == nil {
		return serviceRuntimeUnavailable()
	}
	if !h.isActiveJob(jobID) {
		return serviceRuntimeJobNotFound()
	}
	return nil
}

func decodeRegisterJSON(c *gin.Context, target any) error {
	if c.Request == nil || c.Request.Body == nil {
		return io.EOF
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, registerRequestBodyLimit)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

func publicRegisterStatus(status *service.PythonRegisterJobStatus) gin.H {
	result := gin.H{
		"job_id": status.JobID,
		"status": status.Status,
		"logs":   sanitizeRegisterLogs(status.Logs),
	}
	if status.BFSBlockedUntil > 0 {
		result["bfs_blocked_until"] = status.BFSBlockedUntil
	}
	if status.Status == "failed" && strings.TrimSpace(status.Error) != "" {
		result["error"] = "registration automation failed; review the sanitized job logs"
	}
	return result
}

func sanitizeRegisterLogs(logs []string) []string {
	if len(logs) == 0 {
		return []string{}
	}
	result := make([]string, 0, len(logs))
	for _, line := range logs {
		line = strings.TrimSpace(logredact.RedactText(line,
			"api_key", "api key", "apikey", "key", "token", "access token", "refresh token",
			"personal access token", "secret", "cookie", "session cookie", "authorization",
			"client_id", "client id", "mailbox", "bin", "proxy", "body",
		))
		if len(line) > 4096 {
			line = line[:4096]
		}
		result = append(result, line)
	}
	return result
}

func writeRegisterError(c *gin.Context, err error) {
	var runtimeErr *service.RegisterRuntimeError
	if errors.As(err, &runtimeErr) {
		code := service.RegisterRuntimeErrorCode(err)
		status := service.RegisterRuntimePublicStatus(err)
		response.ErrorFrom(c, infraerrors.New(status, code, registerErrorMessage(code)))
		return
	}
	response.ErrorFrom(c, err)
}

func registerErrorMessage(code string) string {
	switch code {
	case "REGISTER_JOB_NOT_FOUND":
		return "registration job not found"
	case "REGISTER_JOB_CONFLICT":
		return "another registration job is already active"
	case "REGISTER_BFS_LOCKOUT":
		return "Grok registration is temporarily locked"
	case "REGISTER_PAYLOAD_INVALID":
		return "invalid register request"
	case "REGISTER_RUNTIME_UNAUTHORIZED":
		return "Python automation authentication is misconfigured"
	case "REGISTER_RUNTIME_PROTOCOL_INVALID":
		return "Python automation returned an invalid response"
	default:
		return "Python automation service is unavailable"
	}
}

func serviceRuntimeUnavailable() error {
	return &service.RegisterRuntimeError{Code: "REGISTER_RUNTIME_UNAVAILABLE"}
}

func serviceRuntimeProtocolInvalid() error {
	return &service.RegisterRuntimeError{Code: "REGISTER_RUNTIME_PROTOCOL_INVALID", HTTPStatus: http.StatusBadGateway}
}

func serviceRuntimeJobNotFound() error {
	return &service.RegisterRuntimeError{Code: "REGISTER_JOB_NOT_FOUND", HTTPStatus: http.StatusNotFound}
}

func allowedRegisterProvider(value string) bool {
	switch value {
	case "github", "kiro", "deepseek", "oc", "antigravity", "qoder", "codebuddy", "codebuddy-china", "outlook", "grok", "m365":
		return true
	default:
		return false
	}
}

func allowedRegisterMethod(value string) bool {
	switch value {
	case "email", "google", "github", "twitter", "http", "sms":
		return true
	default:
		return false
	}
}

func validateRegisterProviderMethod(provider, method string) error {
	allowed := map[string]map[string]bool{
		"github":          {"github": true},
		"grok":            {"email": true, "http": true},
		"qoder":           {"email": true, "google": true, "http": true},
		"codebuddy":       {"google": true, "github": true, "twitter": true},
		"codebuddy-china": {"sms": true},
		"outlook":         {"email": true},
		"deepseek":        {"email": true, "google": true},
		"kiro":            {"email": true, "google": true},
		"oc":              {"email": true, "google": true},
		"antigravity":     {"google": true},
		"m365":            {"email": true},
	}
	if methods, ok := allowed[provider]; !ok || !methods[method] {
		return infraerrors.BadRequest("REGISTER_METHOD_INVALID", "registration method is not supported for the selected provider")
	}
	return nil
}

func isRegisterTerminalStatus(value string) bool {
	switch value {
	case "done", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func newRegisterJobID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func firstRegisterEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func boolDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func stringDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func boundedString(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return value[:max]
	}
	return value
}
