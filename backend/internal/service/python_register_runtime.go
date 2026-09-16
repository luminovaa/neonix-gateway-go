package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const registerRuntimeBodyLimit = 1 << 20

// RegisterRuntimeBodyLimit is the maximum JSON payload accepted from the
// Python registration worker. Request builders use the same bound so a job is
// never started with a payload the runtime contract cannot safely carry.
const RegisterRuntimeBodyLimit = registerRuntimeBodyLimit

var registerJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type RegisterRuntimeError struct {
	Code       string
	HTTPStatus int
	Cause      error
}

func (e *RegisterRuntimeError) Error() string {
	if e == nil || e.Code == "" {
		return "REGISTER_RUNTIME_UNAVAILABLE"
	}
	return e.Code
}

func (e *RegisterRuntimeError) Unwrap() error { return e.Cause }

type PythonRegisterRuntime struct {
	baseURL        string
	apiKey         string
	client         *http.Client
	runtimeManager *PythonRuntimeManager
}

type PythonRegisterRuntimeStatus struct {
	ManagerConfigured bool `json:"manager_configured"`
	ManagerRunning    bool `json:"manager_running"`
	ChildRunning      bool `json:"child_running"`
	ChildReady        bool `json:"child_ready"`
	Leases            int  `json:"leases"`
}

type PythonBrowserCapability struct {
	Available          bool     `json:"available"`
	PackageInstalled   bool     `json:"packageInstalled"`
	BinaryAvailable    bool     `json:"binaryAvailable"`
	Reason             string   `json:"reason,omitempty"`
	Version            string   `json:"version,omitempty"`
	Platform           string   `json:"platform,omitempty"`
	Path               string   `json:"path,omitempty"`
	SupportedProviders []string `json:"supportedProviders,omitempty"`
}

type PythonBrowserCapabilities map[string]PythonBrowserCapability

type PythonRegisterStartRequest struct {
	JobID             string                         `json:"job_id"`
	Type              string                         `json:"type"`
	RegisterConfig    *PythonRegisterConfig          `json:"register_config,omitempty"`
	SubscribeConfig   *PythonRegisterSubscribeConfig `json:"subscribe_config,omitempty"`
	QoderResetConfig  *PythonQoderResetConfig        `json:"qoder_reset_config,omitempty"`
	QoderAuthConfig   *PythonQoderAuthConfig         `json:"qoder_auth_config,omitempty"`
	QoderUpdateConfig *PythonQoderUpdateConfig       `json:"qoder_update_config,omitempty"`
	AccessToken       string                         `json:"access_token,omitempty"`
	Provider          string                         `json:"provider,omitempty"`
}

// PythonRegisterConfig is an internal worker DTO. Browser handlers must build it
// from validated public input and backend-held secrets instead of decoding a
// request body directly into this type.
type PythonRegisterConfig struct {
	Mailbox           *PythonRegisterMailboxConfig   `json:"mailbox,omitempty"`
	GoogleAccounts    []PythonRegisterAccount        `json:"google_accounts,omitempty"`
	ReloginAccounts   []PythonRegisterAccount        `json:"relogin_accounts,omitempty"`
	InjectAccounts    []PythonRegisterAccount        `json:"inject_accounts,omitempty"`
	TargetProvider    string                         `json:"target_provider"`
	RegisterMethod    string                         `json:"register_method"`
	BrowserEngine     string                         `json:"browser_engine,omitempty"`
	Count             int                            `json:"count,omitempty"`
	Headless          bool                           `json:"headless"`
	Proxy             string                         `json:"proxy,omitempty"`
	Proxies           []string                       `json:"proxies,omitempty"`
	Subscription      *PythonRegisterSubscribeConfig `json:"subscription,omitempty"`
	Concurrency       int                            `json:"concurrency,omitempty"`
	MaxRetries        int                            `json:"max_retries,omitempty"`
	GitHubCountry     string                         `json:"github_country,omitempty"`
	GitHubCreateRepo  bool                           `json:"github_create_repo"`
	GitHubAccountIDs  []string                       `json:"github_account_ids,omitempty"`
	GitHubAccounts    []PythonRegisterAccount        `json:"github_accounts,omitempty"`
	QoderClaim        bool                           `json:"qoder_claim"`
	QoderResetMachine bool                           `json:"qoder_reset_machine"`
	QoderInjectTrial  bool                           `json:"qoder_inject_trial"`
	QoderForceLocal   bool                           `json:"qoder_force_local"`
	QoderProxyPool    bool                           `json:"qoder_proxy_pool"`
	FiveSIMAPIKey     string                         `json:"fivesim_api_key,omitempty"`
	FiveSIMCountry    string                         `json:"fivesim_country,omitempty"`
	FiveSIMService    string                         `json:"fivesim_service,omitempty"`
	SMSProvider       string                         `json:"sms_provider,omitempty"`
	HeroSMSAPIKey     string                         `json:"herosms_api_key,omitempty"`
	HeroSMSCountry    string                         `json:"herosms_country,omitempty"`
	HeroSMSService    string                         `json:"herosms_service,omitempty"`
	CBCNAction        string                         `json:"cbcn_action,omitempty"`
	CBCNAccounts      []PythonRegisterAccount        `json:"cbcn_accounts,omitempty"`
	OutlookCountry    string                         `json:"outlook_country,omitempty"`
	OutlookDomain     string                         `json:"outlook_domain,omitempty"`
}

type PythonRegisterAccount struct {
	ID               string           `json:"id,omitempty"`
	Email            string           `json:"email,omitempty"`
	Username         string           `json:"username,omitempty"`
	Password         string           `json:"password,omitempty"`
	AccessToken      string           `json:"accessToken,omitempty"`
	RefreshToken     string           `json:"refreshToken,omitempty"`
	Cookies          []map[string]any `json:"cookies,omitempty"`
	UserAgent        string           `json:"userAgent,omitempty"`
	Proxy            string           `json:"proxy,omitempty"`
	Provider         string           `json:"provider,omitempty"`
	Credentials      map[string]any   `json:"credentials,omitempty"`
	IDP              string           `json:"idp,omitempty"`
	Nickname         string           `json:"nickname,omitempty"`
	Tags             []string         `json:"tags,omitempty"`
	CreatedAt        int64            `json:"createdAt,omitempty"`
	GitHubCreatedAt  int64            `json:"githubCreatedAt,omitempty"`
	GitHubEligibleAt int64            `json:"githubEligibleAt,omitempty"`
}

type PythonRegisterMailboxConfig struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url,omitempty"`
	Domain  string `json:"domain,omitempty"`
}

type PythonRegisterCaptchaConfig struct {
	APIKey string `json:"api_key"`
}

type PythonRegisterSubscribeConfig struct {
	BIN              string                       `json:"bin"`
	AccountID        string                       `json:"account_id,omitempty"`
	Region           string                       `json:"region,omitempty"`
	ProfileARN       string                       `json:"profile_arn,omitempty"`
	MachineID        string                       `json:"machine_id,omitempty"`
	HolderName       string                       `json:"holder_name,omitempty"`
	SubscriptionType string                       `json:"subscription_type,omitempty"`
	Captcha          *PythonRegisterCaptchaConfig `json:"captcha,omitempty"`
	Headless         bool                         `json:"headless"`
	Proxy            string                       `json:"proxy,omitempty"`
	MaxVCCAttempts   int                          `json:"max_vcc_attempts,omitempty"`
	VCCFile          string                       `json:"vcc_file,omitempty"`
}

type PythonQoderResetConfig struct {
	PatchMainJS bool   `json:"patch_main_js"`
	DataDir     string `json:"data_dir,omitempty"`
	AppDir      string `json:"app_dir,omitempty"`
}

type PythonQoderAuthConfig struct {
	Email        string `json:"email"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	DBPath       string `json:"db_path,omitempty"`
}

type PythonQoderUpdateConfig struct {
	KillProcesses bool `json:"kill_processes"`
}

type PythonRegisterJobStatus struct {
	JobID  string   `json:"job_id"`
	Status string   `json:"status"`
	Logs   []string `json:"logs,omitempty"`
	// Result is an internal worker payload and may contain credentials. Callers
	// must project it to a public, sanitized shape before writing an HTTP
	// response. Keeping the raw value here lets the Go control plane preserve
	// the legacy Register progress contract without ever returning worker
	// credentials to the browser.
	Result          json.RawMessage `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
	BFSBlockedUntil int64           `json:"bfs_blocked_until,omitempty"`
}

type PythonRegisterLogs struct {
	Logs []string `json:"logs"`
}

type PythonRegisterCancelResult struct {
	Cancelled bool `json:"cancelled"`
}

type PythonRegisterBFSLockout struct {
	BFSBlockedUntil int64 `json:"bfs_blocked_until"`
}

func NewPythonRegisterRuntime() *PythonRegisterRuntime {
	port := strings.TrimSpace(os.Getenv("PYAUTO_PORT"))
	if port == "" {
		port = "7788"
	}
	apiKey := strings.TrimSpace(os.Getenv("PYAUTO_INTERNAL_API_KEY"))
	runtime := NewPythonRegisterRuntimeWithConfig(
		strings.TrimSpace(os.Getenv("PYAUTO_BASE_URL")),
		apiKey,
		&http.Client{Timeout: 65 * time.Second},
		port,
	)
	runtime.runtimeManager = NewPythonRuntimeManager()
	return runtime
}

func NewPythonRegisterRuntimeWithConfig(baseURL, apiKey string, client *http.Client, port string) *PythonRegisterRuntime {
	if strings.TrimSpace(baseURL) == "" {
		if strings.TrimSpace(port) == "" {
			port = "7788"
		}
		baseURL = "http://127.0.0.1:" + port
	}
	if client == nil {
		client = &http.Client{Timeout: 65 * time.Second}
	}
	return &PythonRegisterRuntime{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), apiKey: strings.TrimSpace(apiKey), client: client}
}

func (r *PythonRegisterRuntime) WithRuntimeManager(manager *PythonRuntimeManager) *PythonRegisterRuntime {
	if r != nil {
		r.runtimeManager = manager
	}
	return r
}

func (r *PythonRegisterRuntime) Start(ctx context.Context, input PythonRegisterStartRequest) (*PythonRegisterJobStatus, error) {
	if !validRegisterJobID(input.JobID) || !validRegisterJobType(input.Type) {
		return nil, registerRuntimeError("REGISTER_PAYLOAD_INVALID", 0, nil)
	}
	leaseID := registerRuntimeLeaseID(input.JobID)
	if err := r.acquireLease(ctx, leaseID, true); err != nil {
		return nil, err
	}
	release := true
	defer func() {
		if release {
			releasePythonRuntimeLease(r.runtimeManager, leaseID)
		}
	}()

	var result PythonRegisterJobStatus
	if err := r.doJSON(ctx, http.MethodPost, "/job/start", input, &result); err != nil {
		return nil, err
	}
	if !validRegisterJobStatus(&result, input.JobID) {
		return nil, registerRuntimeError("REGISTER_RUNTIME_PROTOCOL_INVALID", http.StatusBadGateway, nil)
	}
	if !isPythonRegisterTerminalStatus(result.Status) {
		release = false
	}
	return &result, nil
}

func (r *PythonRegisterRuntime) Status(ctx context.Context, jobID string) (*PythonRegisterJobStatus, error) {
	if !validRegisterJobID(jobID) {
		return nil, registerRuntimeError("REGISTER_PAYLOAD_INVALID", 0, nil)
	}
	var result PythonRegisterJobStatus
	if err := r.doJSON(ctx, http.MethodGet, "/job/"+url.PathEscape(jobID)+"/status", nil, &result); err != nil {
		if RegisterRuntimeErrorCode(err) == "REGISTER_JOB_NOT_FOUND" {
			releasePythonRuntimeLease(r.runtimeManager, registerRuntimeLeaseID(jobID))
		}
		return nil, err
	}
	if !validRegisterJobStatus(&result, jobID) {
		return nil, registerRuntimeError("REGISTER_RUNTIME_PROTOCOL_INVALID", http.StatusBadGateway, nil)
	}
	if isPythonRegisterTerminalStatus(result.Status) {
		releasePythonRuntimeLease(r.runtimeManager, registerRuntimeLeaseID(jobID))
	}
	return &result, nil
}

func (r *PythonRegisterRuntime) Logs(ctx context.Context, jobID string) (*PythonRegisterLogs, error) {
	if !validRegisterJobID(jobID) {
		return nil, registerRuntimeError("REGISTER_PAYLOAD_INVALID", 0, nil)
	}
	var result PythonRegisterLogs
	if err := r.doJSON(ctx, http.MethodGet, "/job/"+url.PathEscape(jobID)+"/logs", nil, &result); err != nil {
		if RegisterRuntimeErrorCode(err) == "REGISTER_JOB_NOT_FOUND" {
			releasePythonRuntimeLease(r.runtimeManager, registerRuntimeLeaseID(jobID))
		}
		return nil, err
	}
	return &result, nil
}

func (r *PythonRegisterRuntime) Cancel(ctx context.Context, jobID string) (*PythonRegisterCancelResult, error) {
	if !validRegisterJobID(jobID) {
		return nil, registerRuntimeError("REGISTER_PAYLOAD_INVALID", 0, nil)
	}
	defer releasePythonRuntimeLease(r.runtimeManager, registerRuntimeLeaseID(jobID))
	var result PythonRegisterCancelResult
	if err := r.doJSON(ctx, http.MethodPost, "/job/"+url.PathEscape(jobID)+"/cancel", struct{}{}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (r *PythonRegisterRuntime) BFSLockout(ctx context.Context) (*PythonRegisterBFSLockout, error) {
	leaseID := newPythonRuntimeLeaseID("register-probe")
	if err := r.acquireLease(ctx, leaseID, false); err != nil {
		return nil, err
	}
	defer releasePythonRuntimeLease(r.runtimeManager, leaseID)
	var result PythonRegisterBFSLockout
	if err := r.doJSON(ctx, http.MethodGet, "/job/bfs-lockout", nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (r *PythonRegisterRuntime) RuntimeStatus(ctx context.Context) PythonRegisterRuntimeStatus {
	result := PythonRegisterRuntimeStatus{ManagerConfigured: r != nil && r.runtimeManager != nil && r.runtimeManager.Configured()}
	type managerProbe struct {
		running      bool
		childRunning bool
		leases       int
	}
	managerDone := make(chan managerProbe, 1)
	if result.ManagerConfigured {
		go func() {
			status, err := r.runtimeManager.Status(ctx)
			if err != nil || status == nil || status.Python == nil {
				managerDone <- managerProbe{}
				return
			}
			managerDone <- managerProbe{running: true, childRunning: status.Python.Running, leases: status.Leases}
		}()
	} else {
		managerDone <- managerProbe{}
	}
	result.ChildReady = r.workerReady(ctx)
	manager := <-managerDone
	result.ManagerRunning = manager.running
	result.ChildRunning = manager.childRunning
	result.Leases = manager.leases
	if !result.ManagerConfigured {
		result.ChildRunning = result.ChildReady
	}
	return result
}

func (r *PythonRegisterRuntime) Capabilities(ctx context.Context) (PythonBrowserCapabilities, error) {
	var result PythonBrowserCapabilities
	if err := r.doJSON(ctx, http.MethodGet, "/capabilities", nil, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, registerRuntimeError("REGISTER_RUNTIME_PROTOCOL_INVALID", http.StatusBadGateway, nil)
	}
	return result, nil
}

func (r *PythonRegisterRuntime) acquireLease(ctx context.Context, leaseID string, captcha bool) error {
	if r == nil || r.runtimeManager == nil || !r.runtimeManager.Configured() {
		return nil
	}
	_, err := r.runtimeManager.Acquire(ctx, leaseID, captcha)
	if err == nil {
		return nil
	}
	managerStatus := PythonRuntimeManagerHTTPStatus(err)
	if managerStatus == http.StatusUnauthorized || managerStatus == http.StatusForbidden {
		return registerRuntimeError("REGISTER_RUNTIME_UNAUTHORIZED", managerStatus, err)
	}
	return registerRuntimeError("REGISTER_RUNTIME_UNAVAILABLE", 0, err)
}

func (r *PythonRegisterRuntime) workerReady(ctx context.Context) bool {
	if r == nil || r.client == nil || r.baseURL == "" {
		return false
	}
	requestCtx, cancel := boundedPythonRuntimeContext(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, r.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	response, err := r.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode >= 200 && response.StatusCode < 300
}

func registerRuntimeLeaseID(jobID string) string {
	return "neonix-register-" + jobID
}

func isPythonRegisterTerminalStatus(status string) bool {
	switch status {
	case "done", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func (r *PythonRegisterRuntime) doJSON(ctx context.Context, method, path string, payload, output any) error {
	if r == nil || r.client == nil || r.baseURL == "" || r.apiKey == "" {
		return registerRuntimeError("REGISTER_RUNTIME_UNAVAILABLE", 0, nil)
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return registerRuntimeError("REGISTER_PAYLOAD_INVALID", 0, err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, body)
	if err != nil {
		return registerRuntimeError("REGISTER_RUNTIME_UNAVAILABLE", 0, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-api-key", r.apiKey)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return registerRuntimeError("REGISTER_RUNTIME_UNAVAILABLE", 0, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, registerRuntimeBodyLimit+1))
	if err != nil || len(responseBody) > registerRuntimeBodyLimit {
		return registerRuntimeError("REGISTER_RUNTIME_UNAVAILABLE", response.StatusCode, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return registerRuntimeError(registerRuntimeCodeForStatus(response.StatusCode), response.StatusCode, nil)
	}
	if output == nil || len(responseBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return registerRuntimeError("REGISTER_RUNTIME_PROTOCOL_INVALID", response.StatusCode, err)
	}
	return nil
}

func validRegisterJobID(value string) bool { return registerJobIDPattern.MatchString(value) }

// ValidRegisterJobID validates the identifier accepted by fixed Python worker paths.
func ValidRegisterJobID(value string) bool { return validRegisterJobID(value) }

func validRegisterJobType(value string) bool {
	switch value {
	case "register", "subscribe", "register_subscribe", "relogin", "qoder_reset", "qoder_auth", "qoder_disable_update", "inject":
		return true
	default:
		return false
	}
}

func validRegisterJobStatus(status *PythonRegisterJobStatus, expectedJobID string) bool {
	if status == nil || status.JobID != expectedJobID {
		return false
	}
	switch status.Status {
	case "pending", "running", "done", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func registerRuntimeCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "REGISTER_PAYLOAD_INVALID"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "REGISTER_RUNTIME_UNAUTHORIZED"
	case http.StatusNotFound:
		return "REGISTER_JOB_NOT_FOUND"
	case http.StatusConflict:
		return "REGISTER_JOB_CONFLICT"
	case http.StatusTooManyRequests:
		return "REGISTER_BFS_LOCKOUT"
	default:
		return "REGISTER_RUNTIME_UNAVAILABLE"
	}
}

// RegisterRuntimePublicStatus maps a worker-side failure to the status that is
// safe for the operator API. In particular, a worker 401/403 is an internal
// service configuration failure and must not look like an expired operator
// session to the browser.
func RegisterRuntimePublicStatus(err error) int {
	switch RegisterRuntimeErrorCode(err) {
	case "REGISTER_PAYLOAD_INVALID":
		return http.StatusBadRequest
	case "REGISTER_JOB_NOT_FOUND":
		return http.StatusNotFound
	case "REGISTER_JOB_CONFLICT":
		return http.StatusConflict
	case "REGISTER_BFS_LOCKOUT":
		return http.StatusTooManyRequests
	case "REGISTER_RUNTIME_PROTOCOL_INVALID":
		return http.StatusBadGateway
	default:
		return http.StatusServiceUnavailable
	}
}

func registerRuntimeError(code string, status int, cause error) error {
	return &RegisterRuntimeError{Code: code, HTTPStatus: status, Cause: cause}
}

func RegisterRuntimeErrorCode(err error) string {
	var runtimeErr *RegisterRuntimeError
	if errors.As(err, &runtimeErr) && runtimeErr.Code != "" {
		return runtimeErr.Code
	}
	return "REGISTER_RUNTIME_UNAVAILABLE"
}
