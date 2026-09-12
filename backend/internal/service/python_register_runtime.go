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
	baseURL string
	apiKey  string
	client  *http.Client
}

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
	ID          string         `json:"id,omitempty"`
	Email       string         `json:"email,omitempty"`
	Password    string         `json:"password,omitempty"`
	Provider    string         `json:"provider,omitempty"`
	Credentials map[string]any `json:"credentials,omitempty"`
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
	JobID           string          `json:"job_id"`
	Status          string          `json:"status"`
	Logs            []string        `json:"logs,omitempty"`
	Result          json.RawMessage `json:"-"`
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
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("API_SECRET"))
	}
	return NewPythonRegisterRuntimeWithConfig(
		strings.TrimSpace(os.Getenv("PYAUTO_BASE_URL")),
		apiKey,
		&http.Client{Timeout: 65 * time.Second},
		port,
	)
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

func (r *PythonRegisterRuntime) Start(ctx context.Context, input PythonRegisterStartRequest) (*PythonRegisterJobStatus, error) {
	if !validRegisterJobID(input.JobID) || !validRegisterJobType(input.Type) {
		return nil, registerRuntimeError("REGISTER_PAYLOAD_INVALID", 0, nil)
	}
	var result PythonRegisterJobStatus
	if err := r.doJSON(ctx, http.MethodPost, "/job/start", input, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (r *PythonRegisterRuntime) Status(ctx context.Context, jobID string) (*PythonRegisterJobStatus, error) {
	if !validRegisterJobID(jobID) {
		return nil, registerRuntimeError("REGISTER_PAYLOAD_INVALID", 0, nil)
	}
	var result PythonRegisterJobStatus
	if err := r.doJSON(ctx, http.MethodGet, "/job/"+url.PathEscape(jobID)+"/status", nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (r *PythonRegisterRuntime) Logs(ctx context.Context, jobID string) (*PythonRegisterLogs, error) {
	if !validRegisterJobID(jobID) {
		return nil, registerRuntimeError("REGISTER_PAYLOAD_INVALID", 0, nil)
	}
	var result PythonRegisterLogs
	if err := r.doJSON(ctx, http.MethodGet, "/job/"+url.PathEscape(jobID)+"/logs", nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (r *PythonRegisterRuntime) Cancel(ctx context.Context, jobID string) (*PythonRegisterCancelResult, error) {
	if !validRegisterJobID(jobID) {
		return nil, registerRuntimeError("REGISTER_PAYLOAD_INVALID", 0, nil)
	}
	var result PythonRegisterCancelResult
	if err := r.doJSON(ctx, http.MethodPost, "/job/"+url.PathEscape(jobID)+"/cancel", struct{}{}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (r *PythonRegisterRuntime) BFSLockout(ctx context.Context) (*PythonRegisterBFSLockout, error) {
	var result PythonRegisterBFSLockout
	if err := r.doJSON(ctx, http.MethodGet, "/job/bfs-lockout", nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
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
		return registerRuntimeError("REGISTER_RUNTIME_UNAVAILABLE", response.StatusCode, err)
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

func registerRuntimeCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "REGISTER_PAYLOAD_INVALID"
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
