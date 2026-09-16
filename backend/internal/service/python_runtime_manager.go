package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const (
	pythonRuntimeManagerBodyLimit      = 64 << 10
	pythonRuntimeManagerAcquireTimeout = 40 * time.Second
	pythonRuntimeManagerRequestTimeout = 15 * time.Second
)

var (
	pythonRuntimeLeasePattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,160}$`)
	pythonRuntimeLeaseFallback atomic.Uint64
)

// PythonRuntimeManager controls the lightweight supervisor which owns the
// Python automation child and CAPTCHA solver. It is an HTTP boundary on
// purpose: the Go service never imports or executes files from the automation
// repository.
type PythonRuntimeManager struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

type PythonRuntimeProcessStatus struct {
	Running bool `json:"running"`
	Port    int  `json:"port,omitempty"`
}

type PythonRuntimeManagerStatus struct {
	Python             *PythonRuntimeProcessStatus `json:"python"`
	Captcha            *PythonRuntimeProcessStatus `json:"captcha,omitempty"`
	Leases             int                         `json:"leases"`
	IdleTimeoutSeconds float64                     `json:"idleTimeoutSeconds,omitempty"`
}

type PythonRuntimeManagerError struct {
	Status int
	Cause  error
}

func (e *PythonRuntimeManagerError) Error() string {
	if e == nil {
		return "python runtime manager unavailable"
	}
	if e.Status > 0 {
		return fmt.Sprintf("python runtime manager returned HTTP %d", e.Status)
	}
	return "python runtime manager unavailable"
}

func (e *PythonRuntimeManagerError) Unwrap() error { return e.Cause }

func NewPythonRuntimeManager() *PythonRuntimeManager {
	baseURL := strings.TrimSpace(os.Getenv("RUNTIME_MANAGER_URL"))
	if baseURL == "" {
		return nil
	}
	apiKey := strings.TrimSpace(os.Getenv("RUNTIME_MANAGER_API_KEY"))
	return NewPythonRuntimeManagerWithConfig(baseURL, apiKey, &http.Client{Timeout: pythonRuntimeManagerAcquireTimeout})
}

func NewPythonRuntimeManagerWithConfig(baseURL, apiKey string, client *http.Client) *PythonRuntimeManager {
	if client == nil {
		client = &http.Client{Timeout: pythonRuntimeManagerAcquireTimeout}
	}
	return &PythonRuntimeManager{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		client:  client,
	}
}

func (m *PythonRuntimeManager) Configured() bool {
	return m != nil && m.baseURL != ""
}

func (m *PythonRuntimeManager) Acquire(ctx context.Context, leaseID string, captcha bool) (*PythonRuntimeManagerStatus, error) {
	if !validPythonRuntimeLeaseID(leaseID) {
		return nil, &PythonRuntimeManagerError{Cause: errors.New("invalid runtime lease ID")}
	}
	requestCtx, cancel := boundedPythonRuntimeContext(ctx, pythonRuntimeManagerAcquireTimeout)
	defer cancel()
	var status PythonRuntimeManagerStatus
	if err := m.doJSON(requestCtx, http.MethodPost, "/acquire", map[string]any{
		"lease_id": leaseID,
		"python":   true,
		"captcha":  captcha,
	}, &status); err != nil {
		return nil, err
	}
	if status.Python == nil || !status.Python.Running {
		releasePythonRuntimeLease(m, leaseID)
		return nil, &PythonRuntimeManagerError{Cause: errors.New("runtime manager did not start the Python child")}
	}
	return &status, nil
}

func (m *PythonRuntimeManager) Release(ctx context.Context, leaseID string) error {
	if !validPythonRuntimeLeaseID(leaseID) {
		return &PythonRuntimeManagerError{Cause: errors.New("invalid runtime lease ID")}
	}
	requestCtx, cancel := boundedPythonRuntimeContext(ctx, pythonRuntimeManagerRequestTimeout)
	defer cancel()
	var status PythonRuntimeManagerStatus
	return m.doJSON(requestCtx, http.MethodPost, "/release", map[string]string{"lease_id": leaseID}, &status)
}

func (m *PythonRuntimeManager) Status(ctx context.Context) (*PythonRuntimeManagerStatus, error) {
	requestCtx, cancel := boundedPythonRuntimeContext(ctx, 3*time.Second)
	defer cancel()
	var status PythonRuntimeManagerStatus
	if err := m.doJSON(requestCtx, http.MethodGet, "/status", nil, &status); err != nil {
		return nil, err
	}
	if status.Python == nil {
		return nil, &PythonRuntimeManagerError{Cause: errors.New("invalid runtime manager status")}
	}
	return &status, nil
}

func PythonRuntimeManagerHTTPStatus(err error) int {
	var managerErr *PythonRuntimeManagerError
	if errors.As(err, &managerErr) {
		return managerErr.Status
	}
	return 0
}

func (m *PythonRuntimeManager) doJSON(ctx context.Context, method, path string, payload, output any) error {
	if !m.Configured() || m.client == nil || m.apiKey == "" {
		return &PythonRuntimeManagerError{}
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return &PythonRuntimeManagerError{Cause: err}
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, body)
	if err != nil {
		return &PythonRuntimeManagerError{Cause: err}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-api-key", m.apiKey)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := m.client.Do(request)
	if err != nil {
		return &PythonRuntimeManagerError{Cause: err}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, pythonRuntimeManagerBodyLimit+1))
	if err != nil || len(responseBody) > pythonRuntimeManagerBodyLimit {
		return &PythonRuntimeManagerError{Status: response.StatusCode, Cause: err}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &PythonRuntimeManagerError{Status: response.StatusCode}
	}
	if output == nil || len(responseBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return &PythonRuntimeManagerError{Status: response.StatusCode, Cause: err}
	}
	return nil
}

func boundedPythonRuntimeContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, timeout)
}

func validPythonRuntimeLeaseID(value string) bool {
	return pythonRuntimeLeasePattern.MatchString(value)
}

func newPythonRuntimeLeaseID(kind string) string {
	kind = strings.Trim(strings.ToLower(kind), "-")
	if kind == "" {
		kind = "request"
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err == nil {
		return "neonix-" + kind + "-" + hex.EncodeToString(random)
	}
	return fmt.Sprintf("neonix-%s-%d-%d", kind, time.Now().UnixNano(), pythonRuntimeLeaseFallback.Add(1))
}

func releasePythonRuntimeLease(manager *PythonRuntimeManager, leaseID string) {
	if manager == nil || !manager.Configured() || !validPythonRuntimeLeaseID(leaseID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pythonRuntimeManagerRequestTimeout)
	defer cancel()
	_ = manager.Release(ctx, leaseID)
}
