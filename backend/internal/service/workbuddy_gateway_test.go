package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestClassifyWorkBuddyFailureUsesExplicitProviderCodes(t *testing.T) {
	quota := classifyWorkBuddyHTTPFailure(http.StatusTooManyRequests, nil, []byte(`{"code":14018,"msg":"quota exhausted"}`))
	require.Equal(t, GatewayFailureScopeAccount, quota.Scope)
	require.Equal(t, NextAccountRetry, quota.NextAccountAction)
	require.Equal(t, http.StatusTooManyRequests, quota.ClientStatusCode)
	require.True(t, quota.SameAccountRetryDeadline.After(time.Now()))

	reset := time.Now().Add(2 * time.Minute).In(time.FixedZone("UTC+8", 8*60*60))
	rate := classifyWorkBuddyHTTPFailure(http.StatusTooManyRequests, nil, []byte(`{"code":6004,"msg":"reset `+reset.Format("2006-01-02 15:04:05")+` UTC+8"}`))
	require.Equal(t, GatewayFailureScopeModel, rate.Scope)
	require.True(t, rate.ModelCooldown)
	require.True(t, rate.SameAccountRetryDeadline.After(time.Now()))

	invalid := classifyWorkBuddyHTTPFailure(http.StatusBadRequest, nil, []byte(`{"code":11148,"msg":"tool calls and tool results do not match"}`))
	require.Equal(t, GatewayFailureScopeRequest, invalid.Scope)
	require.Equal(t, NextAccountStop, invalid.NextAccountAction)
}

func TestWorkBuddyStreamDropsEmptyDeltasButPreservesReasoningAndTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := codeBuddyServiceWithChat(t, func(*http.Request) (*http.Response, error) {
		return codeBuddySSE(
			`{"choices":[{"delta":{}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"plan"}}]}`,
			`{"choices":[{"delta":{}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		), nil
	})
	account := codeBuddyCLIAccount()
	account.Platform = PlatformWorkBuddy
	account.Credentials["domain"] = "www.workbuddy.ai"
	c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	_, err := service.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"cb/gpt-5.4","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	body := recorder.Body.String()
	require.Contains(t, body, "plan")
	require.Contains(t, body, "call_1")
	// The initial heartbeat is not emitted before the first semantic chunk.
	require.NotContains(t, strings.Split(body, "plan")[0], `"delta":{}`)
	require.Contains(t, body, "[DONE]")
}
