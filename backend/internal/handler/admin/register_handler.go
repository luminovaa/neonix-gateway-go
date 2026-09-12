package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

const registerRequestBodyLimit = 2 << 20

type registerRuntime interface {
	Start(context.Context, service.PythonRegisterStartRequest) (*service.PythonRegisterJobStatus, error)
	Status(context.Context, string) (*service.PythonRegisterJobStatus, error)
	Logs(context.Context, string) (*service.PythonRegisterLogs, error)
	Cancel(context.Context, string) (*service.PythonRegisterCancelResult, error)
	BFSLockout(context.Context) (*service.PythonRegisterBFSLockout, error)
}

type registerProxyLister interface {
	ListActive(context.Context) ([]service.Proxy, error)
}

type registerAccountPersistence interface {
	Persist(context.Context, json.RawMessage) (*service.RegisterPersistResult, error)
}

type RegisterHandler struct {
	runtime     registerRuntime
	proxies     registerProxyLister
	persistence registerAccountPersistence
	internalKey string

	mu          sync.Mutex
	activeJobID string
}

func NewRegisterHandler(runtime *service.PythonRegisterRuntime, proxies *service.ProxyService, persistence *service.RegisterPersistence) *RegisterHandler {
	return &RegisterHandler{runtime: runtime, proxies: proxies, persistence: persistence, internalKey: firstRegisterEnv("PYAUTO_INTERNAL_API_KEY", "API_SECRET")}
}

func newRegisterHandlerForTest(runtime registerRuntime, proxies registerProxyLister) *RegisterHandler {
	return &RegisterHandler{runtime: runtime, proxies: proxies}
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
	if !h.isActiveJob(request.JobID) {
		writeRegisterError(c, infraerrors.Conflict("REGISTER_CALLBACK_INVALID", "register callback does not belong to the active job"))
		return
	}
	if h.persistence == nil {
		writeRegisterError(c, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_CALLBACK_PERSIST_FAILED", "register account persistence is unavailable"))
		return
	}
	result, err := h.persistence.Persist(c.Request.Context(), request.Account)
	if err != nil {
		writeRegisterError(c, err)
		return
	}
	response.Success(c, gin.H{
		"ok": true, "created": result.Created,
		"account": gin.H{"id": result.Account.ID, "name": result.Account.Name, "platform": result.Account.Platform, "status": result.Account.Status},
	})
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
	TargetProvider    string                  `json:"target_provider"`
	RegisterMethod    string                  `json:"register_method"`
	BrowserEngine     string                  `json:"browser_engine"`
	UseProxy          *bool                   `json:"use_proxy"`
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
	Headless         *bool  `json:"headless"`
	AccountID        string `json:"account_id"`
	Region           string `json:"region"`
	ProfileARN       string `json:"profile_arn"`
	MachineID        string `json:"machine_id"`
	HolderName       string `json:"holder_name"`
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
	if request.Type != "register" && request.Type != "register_subscribe" {
		writeRegisterError(c, infraerrors.BadRequest("REGISTER_FEATURE_NOT_MIGRATED", "this automation job is not available in the Go backend yet"))
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
	h.mu.Unlock()
	workerRequest, err := h.buildWorkerRequest(c.Request.Context(), jobID, request)
	if err != nil {
		h.clearActiveJob(jobID)
		writeRegisterError(c, err)
		return
	}
	status, err := h.runtime.Start(c.Request.Context(), workerRequest)
	if err != nil {
		h.clearActiveJob(jobID)
		writeRegisterError(c, err)
		return
	}
	if isRegisterTerminalStatus(status.Status) {
		h.clearActiveJob(jobID)
	}
	response.Success(c, gin.H{"job_id": jobID, "status": status.Status})
}

func (h *RegisterHandler) Status(c *gin.Context) {
	jobID := strings.TrimSpace(c.Query("job_id"))
	status, err := h.runtime.Status(c.Request.Context(), jobID)
	if err != nil {
		if service.RegisterRuntimeErrorCode(err) == "REGISTER_JOB_NOT_FOUND" {
			h.clearActiveJob(jobID)
		}
		writeRegisterError(c, err)
		return
	}
	if isRegisterTerminalStatus(status.Status) {
		h.clearActiveJob(jobID)
	}
	response.Success(c, status)
}

func (h *RegisterHandler) Logs(c *gin.Context) {
	logs, err := h.runtime.Logs(c.Request.Context(), strings.TrimSpace(c.Query("job_id")))
	if err != nil {
		writeRegisterError(c, err)
		return
	}
	response.Success(c, logs)
}

func (h *RegisterHandler) Cancel(c *gin.Context) {
	var request struct {
		JobID string `json:"job_id"`
	}
	if err := decodeRegisterJSON(c, &request); err != nil || strings.TrimSpace(request.JobID) == "" {
		writeRegisterError(c, infraerrors.BadRequest("REGISTER_PAYLOAD_INVALID", "job_id is required"))
		return
	}
	result, err := h.runtime.Cancel(c.Request.Context(), request.JobID)
	if err != nil {
		writeRegisterError(c, err)
		return
	}
	if result.Cancelled {
		h.clearActiveJob(request.JobID)
	}
	response.Success(c, gin.H{"ok": result.Cancelled, "cancelled": result.Cancelled})
}

func (h *RegisterHandler) BFSLockout(c *gin.Context) {
	result, err := h.runtime.BFSLockout(c.Request.Context())
	if err != nil {
		writeRegisterError(c, err)
		return
	}
	response.Success(c, result)
}

func (h *RegisterHandler) PythonStatus(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	_, err := h.runtime.BFSLockout(ctx)
	response.Success(c, gin.H{"running": err == nil, "healthy": err == nil})
}

func (h *RegisterHandler) buildWorkerRequest(ctx context.Context, jobID string, request registerStartRequest) (service.PythonRegisterStartRequest, error) {
	if h == nil || h.runtime == nil || request.RegisterConfig == nil {
		return service.PythonRegisterStartRequest{}, infraerrors.BadRequest("REGISTER_PAYLOAD_INVALID", "register_config is required")
	}
	config, err := h.buildRegisterConfig(ctx, *request.RegisterConfig)
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

func (h *RegisterHandler) buildRegisterConfig(ctx context.Context, raw registerConfigRequest) (*service.PythonRegisterConfig, error) {
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
	if provider == "codebuddy" && method == "github" {
		return nil, infraerrors.BadRequest("REGISTER_FEATURE_NOT_MIGRATED", "CodeBuddy GitHub linking is not available in the Go backend yet")
	}
	if method == "google" || method == "twitter" {
		if len(raw.GoogleAccounts) == 0 {
			return nil, infraerrors.BadRequest("REGISTER_GOOGLE_ACCOUNTS_REQUIRED", "Google accounts are required for this registration method")
		}
	}
	if len(raw.GoogleAccounts) > 20000 {
		return nil, infraerrors.BadRequest("REGISTER_BATCH_TOO_LARGE", "too many Google accounts")
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
		if raw.CBCNAction == "claim" {
			return nil, infraerrors.BadRequest("REGISTER_FEATURE_NOT_MIGRATED", "CodeBuddy China claim is not available in the Go backend yet")
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
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activeJobID == jobID {
		h.activeJobID = ""
	}
}

func (h *RegisterHandler) isActiveJob(jobID string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return jobID != "" && h.activeJobID == jobID
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

func writeRegisterError(c *gin.Context, err error) {
	if code := service.RegisterRuntimeErrorCode(err); code != "REGISTER_RUNTIME_UNAVAILABLE" || errors.As(err, new(*service.RegisterRuntimeError)) {
		status := http.StatusServiceUnavailable
		var runtimeErr *service.RegisterRuntimeError
		if errors.As(err, &runtimeErr) && runtimeErr.HTTPStatus > 0 {
			status = runtimeErr.HTTPStatus
		}
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
	default:
		return "Python automation service is unavailable"
	}
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
