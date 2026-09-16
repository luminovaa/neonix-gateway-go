package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type codeBuddyChinaAccountRepoStub struct {
	AccountRepository
	account *Account
}

func (r *codeBuddyChinaAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account == nil || r.account.ID != id {
		return nil, ErrAccountNotFound
	}
	return r.account, nil
}

func codeBuddyChinaAccount(credentials map[string]any) *Account {
	if credentials == nil {
		credentials = map[string]any{"apiKey": "china-secret"}
	}
	return &Account{
		ID: 91, Name: "codebuddy-china", Platform: PlatformCodeBuddyChina, Type: AccountTypeAPIKey,
		Concurrency: 1, Credentials: credentials,
	}
}

func TestCodeBuddyChinaUsesFixedEndpointHeadersAndPreservesCompatiblePayload(t *testing.T) {
	upstream := codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodPost, req.Method)
		require.Equal(t, "https://www.codebuddy.cn/v2/chat/completions", req.URL.String())
		require.Equal(t, "Bearer china-secret", req.Header.Get("Authorization"))
		require.Equal(t, "XMLHttpRequest", req.Header.Get("X-Requested-With"))
		require.Equal(t, "www.codebuddy.cn", req.Header.Get("X-Domain"))
		require.Equal(t, "SaaS", req.Header.Get("X-Product"))
		require.NotEmpty(t, req.Header.Get("X-Conversation-ID"))
		require.NotEmpty(t, req.Header.Get("X-Request-ID"))

		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.NotContains(t, string(body), "china-secret")
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		require.Equal(t, "glm-5.2", payload["model"])
		require.Equal(t, true, payload["stream"])
		require.Equal(t, "high", payload["reasoning_effort"])
		require.Len(t, payload["messages"], 1)
		require.Len(t, payload["tools"], 1)

		return codeBuddySSE(
			`{"id":"cbc-1","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`{"id":"cbc-1","choices":[{"delta":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		), nil
	}}
	svc := NewCodeBuddyChinaGatewayService(upstream)
	c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	body := []byte(`{"model":"cbc/glm-5.2","reasoning_effort":"high","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, codeBuddyChinaAccount(nil), body)
	require.NoError(t, err)
	require.Equal(t, "glm-5.2", result.UpstreamModel)
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Contains(t, recorder.Body.String(), "hello")
	require.Contains(t, recorder.Body.String(), "thinking")
	require.NotContains(t, recorder.Body.String(), "china-secret")
}

func TestCodeBuddyChinaAcceptsPersistedCredentialAliases(t *testing.T) {
	aliases := []string{"apiKey", "api_key", "token", "accessToken", "access_token", "sessionToken", "session_token"}
	for _, alias := range aliases {
		t.Run(alias, func(t *testing.T) {
			upstream := codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "Bearer alias-secret", req.Header.Get("Authorization"))
				return codeBuddySSE(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`), nil
			}}
			svc := NewCodeBuddyChinaGatewayService(upstream)
			c, _ := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
			_, err := svc.ForwardAsChatCompletions(context.Background(), c, codeBuddyChinaAccount(map[string]any{alias: "alias-secret"}), []byte(`{"model":"cbc/deepseek-v3","messages":[{"role":"user","content":"hi"}]}`))
			require.NoError(t, err)
		})
	}
}

func TestCodeBuddyChinaStreamingProtocolsAndFragmentedTools(t *testing.T) {
	newService := func() *CodeBuddyChinaGatewayService {
		return NewCodeBuddyChinaGatewayService(codeBuddyUpstreamStub{do: func(*http.Request) (*http.Response, error) {
			return codeBuddySSE(
				`{"id":"cbc-stream","choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"run","arguments":"{"}}]}}]}`,
				`{"id":"cbc-stream","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`,
			), nil
		}})
	}

	t.Run("chat buffered", func(t *testing.T) {
		c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
		_, err := newService().ForwardAsChatCompletions(context.Background(), c, codeBuddyChinaAccount(nil), []byte(`{"model":"cbc/deepseek-v3","messages":[{"role":"user","content":"run"}]}`))
		require.NoError(t, err)
		require.Contains(t, recorder.Body.String(), `"arguments":"{\"x\":1}"`)
		require.Contains(t, recorder.Body.String(), `"finish_reason":"tool_calls"`)
	})

	t.Run("messages stream", func(t *testing.T) {
		c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/messages")
		_, err := newService().ForwardAsMessages(context.Background(), c, codeBuddyChinaAccount(nil), []byte(`{"model":"cbc/deepseek-v3","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"run"}]}`))
		require.NoError(t, err)
		require.Contains(t, recorder.Body.String(), "event: message_start")
		require.Contains(t, recorder.Body.String(), `"type":"input_json_delta"`)
		require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: message_stop"))
	})

	t.Run("messages buffered", func(t *testing.T) {
		c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/messages")
		_, err := newService().ForwardAsMessages(context.Background(), c, codeBuddyChinaAccount(nil), []byte(`{"model":"cbc/deepseek-v3","max_tokens":20,"messages":[{"role":"user","content":"run"}]}`))
		require.NoError(t, err)
		require.Contains(t, recorder.Body.String(), `"type":"message"`)
		require.Contains(t, recorder.Body.String(), `"type":"tool_use"`)
		require.Contains(t, recorder.Body.String(), `"stop_reason":"tool_use"`)
	})

	t.Run("responses stream", func(t *testing.T) {
		c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/responses")
		_, err := newService().ForwardAsResponses(context.Background(), c, codeBuddyChinaAccount(nil), []byte(`{"model":"cbc/deepseek-v3","stream":true,"input":"run","tools":[{"type":"function","name":"run","parameters":{"type":"object"}}]}`))
		require.NoError(t, err)
		require.Contains(t, recorder.Body.String(), "event: response.created")
		require.Contains(t, recorder.Body.String(), "event: response.function_call_arguments.delta")
		require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.completed"))
	})

	t.Run("responses buffered", func(t *testing.T) {
		c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/responses")
		_, err := newService().ForwardAsResponses(context.Background(), c, codeBuddyChinaAccount(nil), []byte(`{"model":"cbc/deepseek-v3","input":"run","tools":[{"type":"function","name":"run","parameters":{"type":"object"}}]}`))
		require.NoError(t, err)
		require.Contains(t, recorder.Body.String(), `"object":"response"`)
		require.Contains(t, recorder.Body.String(), `"type":"function_call"`)
		require.Contains(t, recorder.Body.String(), `"arguments":"{\"x\":1}"`)
	})
}

func TestCodeBuddyChinaEmptyStreamAndCancellationPreserveFailoverBoundary(t *testing.T) {
	t.Run("empty stream can fail over", func(t *testing.T) {
		svc := NewCodeBuddyChinaGatewayService(codeBuddyUpstreamStub{do: func(*http.Request) (*http.Response, error) { return codeBuddySSE(), nil }})
		c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
		_, err := svc.ForwardAsChatCompletions(context.Background(), c, codeBuddyChinaAccount(nil), []byte(`{"model":"cbc/deepseek-v3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover)
		require.Empty(t, recorder.Body.String())
	})

	t.Run("client cancellation stops read", func(t *testing.T) {
		svc := NewCodeBuddyChinaGatewayService(codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(codeBuddyContextReader{ctx: req.Context()})}, nil
		}})
		ctx, cancel := context.WithCancel(context.Background())
		c, _ := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
		c.Request = c.Request.WithContext(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := svc.ForwardAsChatCompletions(ctx, c, codeBuddyChinaAccount(nil), []byte(`{"model":"cbc/deepseek-v3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			done <- err
		}()
		time.Sleep(10 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(time.Second):
			t.Fatal("CodeBuddy China upstream read did not stop after cancellation")
		}
	})
}

func TestCodeBuddyChinaFailureClassificationIsScopedAndSanitized(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		scope      GatewayFailureScope
		action     NextAccountAction
		reason     GatewayFailureReason
		clientCode int
	}{
		{name: "unsupported model", status: 400, body: `{"code":11102,"msg":"service info not found secret"}`, scope: GatewayFailureScopeRequest, action: NextAccountStop, reason: "model_not_supported", clientCode: 400},
		{name: "content safety 403", status: 403, body: `{"code":11140,"msg":"request illegal secret"}`, scope: GatewayFailureScopeRequest, action: NextAccountStop, reason: "content_safety_rejected", clientCode: 400},
		{name: "credential rejected", status: 401, body: `{"msg":"access-secret"}`, scope: GatewayFailureScopeAccount, action: NextAccountRetry, reason: "credential_rejected", clientCode: 401},
		{name: "quota", status: 429, body: `{"msg":"access-secret"}`, scope: GatewayFailureScopeAccount, action: NextAccountRetry, reason: "quota_or_rate_limit", clientCode: 429},
		{name: "transient", status: 503, body: `{"msg":"access-secret"}`, scope: GatewayFailureScopeAccount, action: NextAccountRetry, reason: "upstream_transient", clientCode: 502},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{"Retry-After": []string{"17"}, "Set-Cookie": []string{"session=secret"}, "X-Diagnostic": []string{"token-secret"}}
			failure := classifyCodeBuddyChinaHTTPFailure(tc.status, headers, []byte(tc.body))
			require.Equal(t, tc.scope, failure.Scope)
			require.Equal(t, tc.action, failure.NextAccountAction)
			require.Equal(t, tc.reason, failure.Reason)
			require.Equal(t, tc.clientCode, failure.ClientStatusCode)
			require.Equal(t, "17", failure.ResponseHeaders.Get("Retry-After"))
			require.Empty(t, failure.ResponseHeaders.Get("Set-Cookie"))
			require.Empty(t, failure.ResponseHeaders.Get("X-Diagnostic"))
			require.Empty(t, failure.ResponseBody)
			require.NotContains(t, failure.ClientMessage, "secret")
		})
	}
}

func TestCodeBuddyChinaNetworkFailureNeverLeaksCredential(t *testing.T) {
	var calls atomic.Int32
	svc := NewCodeBuddyChinaGatewayService(codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		require.Equal(t, "Bearer china-secret", req.Header.Get("Authorization"))
		return nil, errors.New("network failed with china-secret")
	}})
	c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, codeBuddyChinaAccount(nil), []byte(`{"model":"cbc/deepseek-v3","messages":[{"role":"user","content":"hi"}]}`))
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, int32(1), calls.Load())
	require.NotContains(t, err.Error(), "china-secret")
	require.NotContains(t, recorder.Body.String(), "china-secret")
}

func TestAccountTestServiceCodeBuddyChinaUsesCuratedDefaultModel(t *testing.T) {
	var requestedModel string
	upstream := codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		requestedModel, _ = payload["model"].(string)
		return codeBuddySSE(`{"choices":[{"delta":{"content":"healthy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil
	}}
	account := codeBuddyChinaAccount(nil)
	repo := &codeBuddyChinaAccountRepoStub{account: account}
	svc := NewAccountTestService(repo, nil, nil, nil, nil, nil, upstream, nil, nil)
	svc.SetCodeBuddyChinaGatewayService(NewCodeBuddyChinaGatewayService(upstream))
	c, recorder := newCodeBuddyGinContext(http.MethodPost, "/api/v1/admin/accounts/91/test")

	require.NoError(t, svc.TestAccountConnection(c, account.ID, "", "", AccountTestModeDefault))
	require.Equal(t, "deepseek-v3", requestedModel)
	require.Contains(t, recorder.Body.String(), `"model":"cbc/deepseek-v3"`)
	require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
	require.Contains(t, recorder.Body.String(), `"success":true`)
	require.NotContains(t, recorder.Body.String(), "china-secret")
}
