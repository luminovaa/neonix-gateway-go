package service

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodeBuddyTokenProviderAcceptsUnixMillisecondExpiry(t *testing.T) {
	now := time.Date(2026, time.September, 14, 8, 0, 0, 0, time.UTC)
	account := codeBuddyCLIAccount()
	account.Credentials["expiresAt"] = now.Add(time.Hour).UnixMilli()

	var upstreamCalls atomic.Int32
	provider := NewCodeBuddyTokenProvider(nil, codeBuddyUpstreamStub{do: func(*http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return codeBuddyResponse(http.StatusInternalServerError, nil, `{"code":500}`), nil
	}})
	provider.now = func() time.Time { return now }

	token, err := provider.AccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "access-secret", token)
	require.Zero(t, upstreamCalls.Load(), "a valid millisecond expiry must not trigger refresh")
}

func TestCodeBuddyTokenProviderClassifiesRevokedRefreshSession(t *testing.T) {
	account := codeBuddyCLIAccount()
	account.Credentials["expiresAt"] = time.Now().Add(-time.Minute).UnixMilli()
	provider := NewCodeBuddyTokenProvider(nil, codeBuddyUpstreamStub{do: func(*http.Request) (*http.Response, error) {
		return codeBuddyResponse(http.StatusOK, nil, `{"code":12153,"msg":"offline user session not found"}`), nil
	}})

	_, err := provider.AccessToken(context.Background(), account)
	var tokenErr *CodeBuddyTokenError
	require.ErrorAs(t, err, &tokenErr)
	require.True(t, tokenErr.Revoked)
	require.False(t, tokenErr.Transient)
	require.Equal(t, "CodeBuddy session expired; sign in again", tokenErr.Error())
	require.NotContains(t, tokenErr.Error(), "refresh-secret")
}

func TestCodeBuddyTokenProviderClassifiesTransientRefreshResponses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			account := codeBuddyCLIAccount()
			account.Credentials["expiresAt"] = time.Now().Add(-time.Minute).UnixMilli()
			provider := NewCodeBuddyTokenProvider(nil, codeBuddyUpstreamStub{do: func(*http.Request) (*http.Response, error) {
				return codeBuddyResponse(status, nil, `{"code":90001,"msg":"temporary failure"}`), nil
			}})

			_, err := provider.AccessToken(context.Background(), account)
			var tokenErr *CodeBuddyTokenError
			require.ErrorAs(t, err, &tokenErr)
			require.True(t, tokenErr.Transient)
			require.False(t, tokenErr.Revoked)
			require.Equal(t, status, tokenErr.Status)
			require.Equal(t, "CodeBuddy token service is temporarily unavailable", tokenErr.Error())
			require.NotContains(t, tokenErr.Error(), "temporary failure")
		})
	}
}
