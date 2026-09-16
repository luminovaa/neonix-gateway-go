package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type mailboxCancelledTransport struct{}

func (mailboxCancelledTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestPythonMailboxRuntimeHonorsCancellation(t *testing.T) {
	runtime := NewPythonMailboxRuntimeWithConfig("http://worker.test", "key", &http.Client{Transport: mailboxCancelledTransport{}}, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := runtime.Poll(ctx, MailboxPollInput{Email: "a@example.com", ClientID: "client", RefreshToken: "secret", Timeout: 3 * time.Second})
	require.Error(t, err)
	require.Less(t, time.Since(started), time.Second)
	require.Equal(t, "MAILBOX_RUNTIME_UNAVAILABLE", MailboxErrorCode(err))
}

type mailboxRuntimeRoundTripper struct {
	status int
	body   string
	seen   *http.Request
	data   []byte
}

func (r *mailboxRuntimeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen = req
	r.data, _ = io.ReadAll(req.Body)
	return &http.Response{StatusCode: r.status, Body: io.NopCloser(strings.NewReader(r.body)), Header: make(http.Header), Request: req}, nil
}

func TestPythonMailboxRuntimePollBuildsWorkerRequest(t *testing.T) {
	transport := &mailboxRuntimeRoundTripper{status: http.StatusOK, body: `{"status":"complete","email":"owner@example.com","otp":"123456","url":"https://example.test/verify"}`}
	runtime := NewPythonMailboxRuntimeWithConfig("http://worker.test", "worker-secret", &http.Client{Transport: transport}, "")
	result, err := runtime.Poll(context.Background(), MailboxPollInput{
		Email: "owner@example.com", ClientID: "client-id", RefreshToken: "refresh-secret", Timeout: 5 * time.Second,
		SenderFilter: "  sender@example.com  ", SubjectFilter: "verify",
	})
	require.NoError(t, err)
	require.Equal(t, "123456", result.OTP)
	require.Equal(t, "https://example.test/verify", result.URL)
	require.Equal(t, "worker-secret", transport.seen.Header.Get("x-api-key"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(transport.data, &body))
	require.Equal(t, float64(5000), body["timeoutMs"])
	require.Equal(t, "refresh-secret", body["refreshToken"])
}

func TestPythonMailboxRuntimeClassifiesWorkerFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
	}{
		{name: "credential", status: http.StatusUnauthorized, code: "MAILBOX_CREDENTIAL_INVALID"},
		{name: "runtime", status: http.StatusServiceUnavailable, code: "MAILBOX_RUNTIME_UNAVAILABLE"},
		{name: "failed", status: http.StatusBadGateway, code: "MAILBOX_POLL_FAILED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := NewPythonMailboxRuntimeWithConfig("http://worker.test", "key", &http.Client{Transport: &mailboxRuntimeRoundTripper{status: test.status, body: `{}`}}, "")
			_, err := runtime.Poll(context.Background(), MailboxPollInput{Email: "a@example.com", ClientID: "client", RefreshToken: "secret", Timeout: 3 * time.Second})
			require.Error(t, err)
			require.Equal(t, test.code, MailboxErrorCode(err))
			require.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestPythonMailboxRuntimeRejectsInvalidInputAndTimeout(t *testing.T) {
	runtime := NewPythonMailboxRuntimeWithConfig("http://worker.test", "key", &http.Client{}, "")
	_, err := runtime.Poll(context.Background(), MailboxPollInput{Email: "a@example.com", ClientID: "client", Timeout: 3 * time.Second})
	require.Equal(t, "MAILBOX_CREDENTIAL_INVALID", MailboxErrorCode(err))
	_, err = runtime.Poll(context.Background(), MailboxPollInput{Email: "a@example.com", ClientID: "client", RefreshToken: "secret", Timeout: 2 * time.Second})
	require.Equal(t, "MAILBOX_TIMEOUT_INVALID", MailboxErrorCode(err))
	_, err = ParseMailboxTimeout(30_001)
	require.Error(t, err)
	require.Contains(t, err.Error(), "MAILBOX_TIMEOUT_INVALID")
}
