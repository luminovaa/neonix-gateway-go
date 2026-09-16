package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddy"
	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func codeBuddyDeviceEnvelope(data string) *http.Response {
	return codeBuddyResponse(http.StatusOK, nil, `{"code":0,"msg":"ok","data":`+data+`}`)
}

func TestCodeBuddyDeviceLoginStartPendingSuccessAndSingleUse(t *testing.T) {
	var calls atomic.Int32
	upstream := codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			require.Equal(t, "CLI", req.URL.Query().Get("platform"))
			return codeBuddyDeviceEnvelope(`{"state":"upstream-state","authUrl":"https://www.codebuddy.ai/device/login"}`), nil
		case "/v2/plugin/auth/token":
			require.Equal(t, "upstream-state", req.URL.Query().Get("state"))
			if calls.Add(1) == 1 {
				return codeBuddyResponse(http.StatusOK, nil, `{"code":1001,"msg":"waiting"}`), nil
			}
			return codeBuddyDeviceEnvelope(`{"accessToken":"device-access","refreshToken":"device-refresh","expiresIn":3600,"domain":"www.codebuddy.ai"}`), nil
		case "/v2/plugin/login/account":
			require.Equal(t, "Bearer device-access", req.Header.Get("Authorization"))
			return codeBuddyDeviceEnvelope(`{"uid":"user-1","enterpriseId":"enterprise-1","nickname":"Owner"}`), nil
		default:
			t.Fatalf("unexpected CodeBuddy URL: %s", req.URL.String())
			return nil, nil
		}
	}}
	service := NewCodeBuddyDeviceLoginService(upstream)
	started, err := service.Start(context.Background(), "")
	require.NoError(t, err)
	require.NotEmpty(t, started.LoginID)
	require.Equal(t, started.LoginID, started.State, "compatibility state must expose only the opaque local ID")
	require.Equal(t, "https://www.codebuddy.ai/device/login", started.AuthorizationURL)
	require.NotEqual(t, "upstream-state", started.LoginID)

	pending, err := service.Poll(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.True(t, pending.Pending)
	require.Equal(t, codeBuddyDeviceInterval, pending.RetryAfter)

	complete, err := service.Poll(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.False(t, complete.Pending)
	require.Equal(t, "device-access", complete.Result.Tokens.AccessToken)
	require.Equal(t, "device-refresh", complete.Result.Tokens.RefreshToken)
	require.Equal(t, "user-1", complete.Result.AccountInfo.UserID)

	_, err = service.Poll(context.Background(), started.LoginID)
	requireInfraErrorCode(t, err, "CODEBUDDY_DEVICE_SESSION_IN_USE")
	service.Release(started.LoginID)
	replayed, err := service.Poll(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.Equal(t, "device-access", replayed.Result.Tokens.AccessToken)
	require.Equal(t, int32(2), calls.Load(), "released completion reuses the server-side token bundle")
	service.Consume(started.LoginID)
	_, err = service.Poll(context.Background(), started.LoginID)
	requireInfraErrorCode(t, err, "CODEBUDDY_DEVICE_SESSION_NOT_FOUND")
}

func TestCodeBuddyDeviceLoginExpiryCancelAndInFlightCancel(t *testing.T) {
	base := time.Date(2026, time.September, 14, 8, 0, 0, 0, time.UTC)
	now := base
	pollStarted := make(chan struct{})
	releasePoll := make(chan struct{})
	upstream := codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return codeBuddyDeviceEnvelope(`{"state":"upstream-state","authUrl":"https://www.codebuddy.ai/device/login"}`), nil
		case "/v2/plugin/auth/token":
			close(pollStarted)
			<-releasePoll
			return codeBuddyDeviceEnvelope(`{"accessToken":"cancelled-access","refreshToken":"cancelled-refresh"}`), nil
		default:
			return codeBuddyDeviceEnvelope(`{}`), nil
		}
	}}
	service := NewCodeBuddyDeviceLoginService(upstream)
	service.now = func() time.Time { return now }

	expiring, err := service.Start(context.Background(), "")
	require.NoError(t, err)
	now = base.Add(codeBuddyDeviceSessionTTL + time.Second)
	_, err = service.Poll(context.Background(), expiring.LoginID)
	requireInfraErrorCode(t, err, "CODEBUDDY_DEVICE_SESSION_EXPIRED")

	now = base
	cancelled, err := service.Start(context.Background(), "")
	require.NoError(t, err)
	service.Cancel(cancelled.LoginID)
	_, err = service.Poll(context.Background(), cancelled.LoginID)
	requireInfraErrorCode(t, err, "CODEBUDDY_DEVICE_SESSION_NOT_FOUND")

	inFlight, err := service.Start(context.Background(), "")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, pollErr := service.Poll(context.Background(), inFlight.LoginID)
		done <- pollErr
	}()
	<-pollStarted
	service.Cancel(inFlight.LoginID)
	close(releasePoll)
	requireInfraErrorCode(t, <-done, "CODEBUDDY_DEVICE_SESSION_CANCELLED")
}

func TestCodeBuddyDeviceLoginConcurrentPollIsSingleFlight(t *testing.T) {
	pollStarted := make(chan struct{})
	releasePoll := make(chan struct{})
	var polls atomic.Int32
	upstream := codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return codeBuddyDeviceEnvelope(`{"state":"state","authUrl":"https://www.codebuddy.ai/device/login"}`), nil
		case "/v2/plugin/auth/token":
			polls.Add(1)
			close(pollStarted)
			<-releasePoll
			return codeBuddyResponse(http.StatusOK, nil, `{"code":1001,"msg":"waiting"}`), nil
		default:
			return nil, errors.New("unexpected request")
		}
	}}
	service := NewCodeBuddyDeviceLoginService(upstream)
	started, err := service.Start(context.Background(), "")
	require.NoError(t, err)
	firstDone := make(chan error, 1)
	go func() {
		_, pollErr := service.Poll(context.Background(), started.LoginID)
		firstDone <- pollErr
	}()
	<-pollStarted
	second, err := service.Poll(context.Background(), started.LoginID)
	require.NoError(t, err)
	require.True(t, second.Pending)
	require.Equal(t, int32(1), polls.Load())
	close(releasePoll)
	require.NoError(t, <-firstDone)
}

func TestCodeBuddyDeviceLoginRejectsCNUnsafeURLsAndOversizedBodies(t *testing.T) {
	var calls atomic.Int32
	service := NewCodeBuddyDeviceLoginService(codeBuddyUpstreamStub{do: func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return codeBuddyDeviceEnvelope(`{"state":"state","authUrl":"https://evil.example/login"}`), nil
	}})
	_, err := service.Start(context.Background(), "copilot.tencent.com")
	requireInfraErrorCode(t, err, "CODEBUDDY_CHINA_DEVICE_UNSUPPORTED")
	require.Zero(t, calls.Load())

	_, err = service.Start(context.Background(), "")
	requireInfraErrorCode(t, err, "CODEBUDDY_DEVICE_RESPONSE_INCOMPLETE")
	require.Equal(t, int32(1), calls.Load())

	require.False(t, validCodeBuddyAuthURL("https://codebuddy.ai@evil.example/login", codebuddyAIHostsForTest()))
	require.False(t, validCodeBuddyAuthURL("https://www.codebuddy.ai/login#token", codebuddyAIHostsForTest()))
	require.True(t, validCodeBuddyAuthURL("https://accounts.codebuddy.ai/login", codebuddyAIHostsForTest()))

	oversized := NewCodeBuddyDeviceLoginService(codeBuddyUpstreamStub{do: func(*http.Request) (*http.Response, error) {
		return codeBuddyResponse(http.StatusOK, nil, strings.Repeat("x", codeBuddyResponseBodyMax+1)), nil
	}})
	_, err = oversized.Start(context.Background(), "")
	requireInfraErrorCode(t, err, "CODEBUDDY_RESPONSE_TOO_LARGE")
}

func codebuddyAIHostsForTest() codebuddy.Hosts {
	return codebuddy.AIHosts
}

func requireInfraErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	var infraErr *infraerrors.Error
	require.ErrorAs(t, err, &infraErr)
	require.Equal(t, code, infraErr.Reason)
}
