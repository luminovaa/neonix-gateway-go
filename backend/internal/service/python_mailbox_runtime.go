package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	mailboxMinTimeout       = 3 * time.Second
	mailboxMaxTimeout       = 30 * time.Second
	mailboxRuntimeBodyLimit = 1 << 20
)

type MailboxPollInput struct {
	Email         string
	ClientID      string
	RefreshToken  string
	Timeout       time.Duration
	SenderFilter  string
	SubjectFilter string
}

type MailboxPollResult struct {
	Status     string `json:"status,omitempty"`
	Email      string `json:"email,omitempty"`
	OTP        string `json:"otp,omitempty"`
	URL        string `json:"url,omitempty"`
	Subject    string `json:"subject,omitempty"`
	From       string `json:"from,omitempty"`
	Folder     string `json:"folder,omitempty"`
	ReceivedAt string `json:"receivedAt,omitempty"`
	Message    string `json:"message,omitempty"`
}

type MailboxRuntimeError struct {
	Code       string
	HTTPStatus int
	Cause      error
}

func (e *MailboxRuntimeError) Error() string {
	if e == nil {
		return "mailbox runtime error"
	}
	return e.Code
}

func (e *MailboxRuntimeError) Unwrap() error { return e.Cause }

type PythonMailboxRuntime struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewPythonMailboxRuntime() *PythonMailboxRuntime {
	port := strings.TrimSpace(os.Getenv("PYAUTO_PORT"))
	if port == "" {
		port = "7788"
	}
	apiKey := strings.TrimSpace(os.Getenv("PYAUTO_INTERNAL_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("API_SECRET"))
	}
	return NewPythonMailboxRuntimeWithConfig(
		strings.TrimRight(strings.TrimSpace(os.Getenv("PYAUTO_BASE_URL")), "/"),
		apiKey,
		&http.Client{Timeout: mailboxMaxTimeout + 5*time.Second},
		port,
	)
}

func NewPythonMailboxRuntimeWithConfig(baseURL, apiKey string, client *http.Client, port string) *PythonMailboxRuntime {
	if strings.TrimSpace(baseURL) == "" {
		if strings.TrimSpace(port) == "" {
			port = "7788"
		}
		baseURL = "http://127.0.0.1:" + port
	}
	if client == nil {
		client = &http.Client{Timeout: mailboxMaxTimeout + 5*time.Second}
	}
	return &PythonMailboxRuntime{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), apiKey: strings.TrimSpace(apiKey), client: client}
}

func (r *PythonMailboxRuntime) Poll(ctx context.Context, input MailboxPollInput) (*MailboxPollResult, error) {
	if r == nil || r.client == nil || r.baseURL == "" || r.apiKey == "" {
		return nil, &MailboxRuntimeError{Code: "MAILBOX_RUNTIME_UNAVAILABLE"}
	}
	if strings.TrimSpace(input.Email) == "" || strings.TrimSpace(input.ClientID) == "" || strings.TrimSpace(input.RefreshToken) == "" {
		return nil, &MailboxRuntimeError{Code: "MAILBOX_CREDENTIAL_INVALID"}
	}
	timeout := input.Timeout
	if timeout < mailboxMinTimeout || timeout > mailboxMaxTimeout {
		return nil, &MailboxRuntimeError{Code: "MAILBOX_TIMEOUT_INVALID"}
	}
	payload := map[string]any{
		"email": input.Email, "clientId": input.ClientID, "refreshToken": input.RefreshToken,
		"timeoutMs": int(timeout / time.Millisecond),
	}
	if sender := strings.TrimSpace(input.SenderFilter); sender != "" {
		payload["senderFilter"] = truncateMailboxFilter(sender)
	}
	if subject := strings.TrimSpace(input.SubjectFilter); subject != "" {
		payload["subjectFilter"] = truncateMailboxFilter(subject)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &MailboxRuntimeError{Code: "MAILBOX_POLL_FAILED", Cause: err}
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, r.baseURL+"/api/mailbox/poll", bytes.NewReader(body))
	if err != nil {
		return nil, &MailboxRuntimeError{Code: "MAILBOX_POLL_FAILED", Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if r.apiKey != "" {
		request.Header.Set("x-api-key", r.apiKey)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, &MailboxRuntimeError{Code: "MAILBOX_RUNTIME_UNAVAILABLE", Cause: err}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, mailboxRuntimeBodyLimit))
	if err != nil {
		return nil, &MailboxRuntimeError{Code: "MAILBOX_POLL_FAILED", Cause: err}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusBadRequest {
		return nil, &MailboxRuntimeError{Code: "MAILBOX_CREDENTIAL_INVALID", HTTPStatus: response.StatusCode}
	}
	if response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusGatewayTimeout {
		return nil, &MailboxRuntimeError{Code: "MAILBOX_RUNTIME_UNAVAILABLE", HTTPStatus: response.StatusCode}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &MailboxRuntimeError{Code: "MAILBOX_POLL_FAILED", HTTPStatus: response.StatusCode}
	}
	var result MailboxPollResult
	if len(responseBody) > 0 {
		if err := json.Unmarshal(responseBody, &result); err != nil {
			return nil, &MailboxRuntimeError{Code: "MAILBOX_POLL_FAILED", Cause: err}
		}
	}
	return &result, nil
}

func truncateMailboxFilter(value string) string {
	const max = 200
	if len([]rune(value)) <= max {
		return value
	}
	return string([]rune(value)[:max])
}

func MailboxErrorCode(err error) string {
	var runtimeErr *MailboxRuntimeError
	if errors.As(err, &runtimeErr) && runtimeErr.Code != "" {
		return runtimeErr.Code
	}
	return "MAILBOX_POLL_FAILED"
}

func ParseMailboxTimeout(value int) (time.Duration, error) {
	if value < int(mailboxMinTimeout/time.Millisecond) || value > int(mailboxMaxTimeout/time.Millisecond) {
		return 0, fmt.Errorf("%w: timeout must be between %d and %d ms", errors.New("MAILBOX_TIMEOUT_INVALID"), mailboxMinTimeout/time.Millisecond, mailboxMaxTimeout/time.Millisecond)
	}
	return time.Duration(value) * time.Millisecond, nil
}
