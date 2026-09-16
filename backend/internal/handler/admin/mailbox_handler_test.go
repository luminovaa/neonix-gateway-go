package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type mailboxHandlerRuntimeRoundTripper struct{}

func (mailboxHandlerRuntimeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"status":"complete","otp":"654321","url":"javascript:alert(1)"}`)),
		Header:     make(http.Header), Request: req,
	}, nil
}

func TestPollMailboxCompatUsesPythonWorkerAndRejectsUnsafeURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminService := newStubAdminService()
	adminService.getAccountResult = &service.Account{
		ID: 42, Name: "owner@example.com", Platform: "outlook", Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"email": "owner@example.com", "refresh_token": "refresh-secret", "client_id": "client-id"},
		Status:      service.StatusActive, CreatedAt: time.Now(),
	}
	h := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h.mailboxRuntime = service.NewPythonMailboxRuntimeWithConfig("http://worker.test", "worker-secret", &http.Client{Transport: mailboxHandlerRuntimeRoundTripper{}}, "")

	request := httptest.NewRequest(http.MethodPost, "/api/mailbox/poll", bytes.NewBufferString(`{"accountId":"42","timeoutMs":3000}`))
	request.Header.Set("Content-Type", "application/json")
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = request
	h.PollMailboxCompat(ctx)

	require.Equal(t, http.StatusOK, writer.Code)
	require.NotContains(t, writer.Body.String(), "refresh-secret")
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(writer.Body.Bytes(), &envelope))
	outerData, ok := envelope["data"].(map[string]any)
	require.True(t, ok)
	data, ok := outerData["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "654321", data["otp"])
	require.Empty(t, data["url"])
}

func TestPollMailboxCompatValidatesProviderAndSingleFlight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminService := newStubAdminService()
	adminService.getAccountResult = &service.Account{ID: 7, Name: "owner@example.com", Platform: "openai", Credentials: map[string]any{"refresh_token": "secret"}}
	h := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/mailbox/poll", bytes.NewBufferString(`{"accountId":"7","timeoutMs":3000}`))
	request.Header.Set("Content-Type", "application/json")
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = request
	h.PollMailboxCompat(ctx)
	require.Equal(t, http.StatusNotFound, writer.Code)

	require.True(t, h.claimMailboxPoll(7))
	require.False(t, h.claimMailboxPoll(7))
	h.releaseMailboxPoll(7)
	require.True(t, h.claimMailboxPoll(7))
	h.releaseMailboxPoll(7)
}

func TestIsMailboxConfiguredDoesNotTreatCopilotScopeAsIMAP(t *testing.T) {
	require.False(t, isMailboxConfigured(service.PlatformM365, map[string]any{
		"refresh_token": "refresh", "scope": "openid profile https://substrate.office.com/sydney/M365Chat.Read",
	}))
	require.True(t, isMailboxConfigured(service.PlatformM365, map[string]any{
		"refresh_token": "refresh", "scope": "openid offline_access https://outlook.office.com/IMAP.AccessAsUser.All",
	}))
}
