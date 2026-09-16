package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type codeBuddyUpstreamStub struct {
	do func(*http.Request) (*http.Response, error)
}

func (s codeBuddyUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return s.do(req)
}

func (s codeBuddyUpstreamStub) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxy, accountID, concurrency)
}

func codeBuddyResponse(status int, headers http.Header, body string) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

func codeBuddySSE(chunks ...string) *http.Response {
	var stream strings.Builder
	for _, chunk := range chunks {
		stream.WriteString("data: " + chunk + "\n\n")
	}
	stream.WriteString("data: [DONE]\n\n")
	return codeBuddyResponse(http.StatusOK, make(http.Header), stream.String())
}

func codeBuddyCLIAccount() *Account {
	return &Account{
		ID: 81, Name: "codebuddy-cli", Platform: PlatformCodeBuddy, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{
			"accessToken": "access-secret", "refreshToken": "refresh-secret",
			"expiresAt": time.Now().Add(time.Hour).UnixMilli(), "userId": "user-1",
			"enterpriseId": "enterprise-1", "domain": "www.codebuddy.ai", "codebuddyAuth": "cli",
		},
	}
}

func codeBuddyServiceWithChat(t *testing.T, chat func(*http.Request) (*http.Response, error)) *CodeBuddyGatewayService {
	t.Helper()
	upstream := codeBuddyUpstreamStub{do: chat}
	return NewCodeBuddyGatewayService(NewCodeBuddyTokenProvider(nil, upstream), upstream)
}

func newCodeBuddyGinContext(method, path string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, path, nil)
	return c, recorder
}

func TestCodeBuddyCLIChatUsesFixedEndpointAndNeverSendsRefreshToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodPost, req.Method)
		require.Equal(t, "https://www.codebuddy.ai/v2/chat/completions", req.URL.String())
		require.Equal(t, "Bearer access-secret", req.Header.Get("Authorization"))
		require.Equal(t, "user-1", req.Header.Get("X-User-Id"))
		require.Equal(t, "enterprise-1", req.Header.Get("X-Enterprise-Id"))
		require.Empty(t, req.Header.Get("X-Refresh-Token"))
		for _, values := range req.Header {
			require.NotContains(t, strings.Join(values, " "), "refresh-secret")
		}
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		require.Equal(t, "gemini-3.1-pro", payload["model"])
		require.Equal(t, true, payload["stream"])
		return codeBuddySSE(
			`{"id":"cb-1","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`{"id":"cb-1","choices":[{"delta":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		), nil
	}}
	svc := NewCodeBuddyGatewayService(NewCodeBuddyTokenProvider(nil, upstream), upstream)
	c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, codeBuddyCLIAccount(), []byte(`{"model":"cb/gemini-3.1-pro","messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.Equal(t, "gemini-3.1-pro", result.UpstreamModel)
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Contains(t, recorder.Body.String(), "hello")
	require.Contains(t, recorder.Body.String(), "thinking")
	require.NotContains(t, recorder.Body.String(), "access-secret")
	require.NotContains(t, recorder.Body.String(), "refresh-secret")
}

func TestWorkBuddyUsesGlobalEndpointAndCLIChannelHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "https://www.workbuddy.ai/v2/chat/completions", req.URL.String())
		require.Equal(t, "CLI/2.139.0 CodeBuddy/2.139.0", req.Header.Get("User-Agent"))
		require.NotEmpty(t, req.Header.Get("X-Session-Id"))
		require.Equal(t, "CLI", req.Header.Get("X-Ide-Type"))
		require.Empty(t, req.Header.Get("X-Refresh-Token"))
		return codeBuddySSE(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	}}
	account := codeBuddyCLIAccount()
	account.Platform = PlatformWorkBuddy
	account.Credentials["domain"] = "www.workbuddy.ai"
	svc := NewCodeBuddyGatewayService(NewCodeBuddyTokenProvider(nil, upstream), upstream)
	c, _ := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"cb/gpt-5.4","messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
}

func TestCodeBuddyDefaultModelPreservesLegacyCBSlash(t *testing.T) {
	svc := codeBuddyServiceWithChat(t, func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		require.Equal(t, "cb/", payload["model"])
		return codeBuddySSE(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	})
	c, _ := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, codeBuddyCLIAccount(), []byte(`{"model":"cb/","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.Equal(t, "cb/", result.UpstreamModel)
}

func TestCodeBuddyAIRouterRejectsExplicitChinaRealm(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := codeBuddyUpstreamStub{do: func(*http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return nil, errors.New("must not call China upstream")
	}}
	svc := NewCodeBuddyGatewayService(NewCodeBuddyTokenProvider(nil, upstream), upstream)
	account := codeBuddyCLIAccount()
	account.Credentials["domain"] = "copilot.tencent.com"
	c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"cb/gpt-5.4","messages":[{"role":"user","content":"hi"}]}`))
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, GatewayFailureReason("codebuddy_china_requires_separate_provider"), failure.Reason)
	require.Equal(t, GatewayFailureScopeAccount, failure.Scope)
	require.Equal(t, int32(0), upstreamCalls.Load())
	require.Empty(t, recorder.Body.String())
}

func TestCodeBuddyLegacyTokenAndCookieCredentials(t *testing.T) {
	account := &Account{ID: 82, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{
		"apiKey": "legacy-token", "rawCookies": map[string]any{"session": "cookie-secret", "tenant": "one"}, "userAgent": "legacy-agent",
	}}
	svc := codeBuddyServiceWithChat(t, func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "Bearer legacy-token", req.Header.Get("Authorization"))
		require.Equal(t, "legacy-token", req.Header.Get("X-Api-Key"))
		require.Equal(t, "session=cookie-secret; tenant=one", req.Header.Get("Cookie"))
		require.Equal(t, "legacy-agent", req.Header.Get("User-Agent"))
		return codeBuddySSE(`{"choices":[{"delta":{"content":"legacy ok"},"finish_reason":"stop"}]}`), nil
	})
	c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"cb/gpt-5.4","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.NotContains(t, recorder.Body.String(), "legacy-token")
	require.NotContains(t, recorder.Body.String(), "cookie-secret")
}

func TestCodeBuddyStreamingChatMessagesAndResponses(t *testing.T) {
	newService := func() *CodeBuddyGatewayService {
		return codeBuddyServiceWithChat(t, func(*http.Request) (*http.Response, error) {
			return codeBuddySSE(
				`{"id":"cb-stream","choices":[{"delta":{"content":"hello"}}]}`,
				`{"id":"cb-stream","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`,
			), nil
		})
	}

	t.Run("chat", func(t *testing.T) {
		c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
		result, err := newService().ForwardAsChatCompletions(context.Background(), c, codeBuddyCLIAccount(), []byte(`{"model":"cb/gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		require.True(t, result.Stream)
		require.Contains(t, recorder.Body.String(), "hello")
		require.Equal(t, 1, strings.Count(recorder.Body.String(), `"finish_reason":"stop"`))
		require.Contains(t, recorder.Body.String(), "data: [DONE]")
	})

	t.Run("messages", func(t *testing.T) {
		c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/messages")
		_, err := newService().ForwardAsMessages(context.Background(), c, codeBuddyCLIAccount(), []byte(`{"model":"cb/gpt-5.4","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		require.Contains(t, recorder.Body.String(), "event: message_start")
		require.Contains(t, recorder.Body.String(), "event: content_block_delta")
		require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: message_stop"))
	})

	t.Run("responses stream", func(t *testing.T) {
		c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/responses")
		_, err := newService().ForwardAsResponses(context.Background(), c, codeBuddyCLIAccount(), []byte(`{"model":"cb/gpt-5.4","stream":true,"input":"hi"}`))
		require.NoError(t, err)
		require.Contains(t, recorder.Body.String(), "event: response.created")
		require.Contains(t, recorder.Body.String(), "event: response.output_text.delta")
		require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.completed"))
	})

	t.Run("responses buffered", func(t *testing.T) {
		c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/responses")
		_, err := newService().ForwardAsResponses(context.Background(), c, codeBuddyCLIAccount(), []byte(`{"model":"cb/gpt-5.4","input":"hi"}`))
		require.NoError(t, err)
		var response map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		require.Equal(t, "response", response["object"])
		require.Equal(t, "completed", response["status"])
	})
}

func TestCodeBuddyToolFragmentsKeepStableGeneratedID(t *testing.T) {
	svc := codeBuddyServiceWithChat(t, func(*http.Request) (*http.Response, error) {
		return codeBuddySSE(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"run","arguments":"{"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\":1}"}}]},"finish_reason":"tool_calls"}]}`,
		), nil
	})
	c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, codeBuddyCLIAccount(), []byte(`{"model":"cb/gpt-5.4","stream":true,"messages":[{"role":"user","content":"run"}]}`))
	require.NoError(t, err)
	matches := regexp.MustCompile(`"id":"(call_[^"]+)"`).FindAllStringSubmatch(recorder.Body.String(), -1)
	require.NotEmpty(t, matches)
	for _, match := range matches {
		require.Equal(t, matches[0][1], match[1])
	}
}

func TestCodeBuddyEmptyOrFinishOnlyStreamCanFailOverBeforeOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
	}{
		{name: "empty"},
		{name: "malformed", chunks: []string{`not-json`}},
		{name: "finish only", chunks: []string{`{"choices":[{"delta":{},"finish_reason":"stop"}]}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := codeBuddyServiceWithChat(t, func(*http.Request) (*http.Response, error) { return codeBuddySSE(tc.chunks...), nil })
			c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
			_, err := svc.ForwardAsChatCompletions(context.Background(), c, codeBuddyCLIAccount(), []byte(`{"model":"cb/gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Empty(t, recorder.Body.String())
		})
	}
}

type codeBuddyErrorAfterReader struct {
	reader *strings.Reader
	err    error
}

func (r *codeBuddyErrorAfterReader) Read(p []byte) (int, error) {
	if r.reader.Len() > 0 {
		return r.reader.Read(p)
	}
	return 0, r.err
}

func TestCodeBuddyDoesNotFailOverAfterOutputCommit(t *testing.T) {
	readErr := errors.New("stream interrupted")
	svc := codeBuddyServiceWithChat(t, func(*http.Request) (*http.Response, error) {
		reader := &codeBuddyErrorAfterReader{reader: strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"visible\"}}]}\n\n"), err: readErr}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(reader)}, nil
	})
	c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, codeBuddyCLIAccount(), []byte(`{"model":"cb/gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	require.ErrorIs(t, err, readErr)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.Contains(t, recorder.Body.String(), "visible")
}

type codeBuddyContextReader struct{ ctx context.Context }

func (r codeBuddyContextReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestCodeBuddyClientCancellationStopsUpstreamRead(t *testing.T) {
	svc := codeBuddyServiceWithChat(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(codeBuddyContextReader{ctx: req.Context()})}, nil
	})
	requestContext, cancel := context.WithCancel(context.Background())
	c, _ := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	c.Request = c.Request.WithContext(requestContext)
	done := make(chan error, 1)
	go func() {
		_, err := svc.ForwardAsChatCompletions(requestContext, c, codeBuddyCLIAccount(), []byte(`{"model":"cb/gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("CodeBuddy upstream read did not stop after cancellation")
	}
}

func TestCodeBuddyFailureClassificationSanitizesBodyAndHeaders(t *testing.T) {
	upstream := codeBuddyUpstreamStub{do: func(*http.Request) (*http.Response, error) {
		return codeBuddyResponse(http.StatusTooManyRequests, http.Header{
			"Retry-After": []string{"17"}, "Set-Cookie": []string{"session=secret"}, "X-Diagnostic": []string{"token-secret"},
		}, "upstream access-secret refresh-secret"), nil
	}}
	svc := NewCodeBuddyGatewayService(NewCodeBuddyTokenProvider(nil, upstream), upstream)
	c, recorder := newCodeBuddyGinContext(http.MethodPost, "/v1/chat/completions")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, codeBuddyCLIAccount(), []byte(`{"model":"cb/gpt-5.4","messages":[{"role":"user","content":"hi"}]}`))
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, http.StatusTooManyRequests, failure.ClientStatusCode)
	require.Equal(t, "17", failure.ResponseHeaders.Get("Retry-After"))
	require.Empty(t, failure.ResponseHeaders.Get("Set-Cookie"))
	require.Empty(t, failure.ResponseHeaders.Get("X-Diagnostic"))
	require.Empty(t, failure.ResponseBody)
	require.NotContains(t, failure.ClientMessage, "secret")
	require.Empty(t, recorder.Body.String())
	require.True(t, failure.SameAccountRetryDeadline.After(time.Now().Add(16*time.Second)))
}

func TestCodeBuddyConcurrentRefreshIsSingleFlightAndManualRotationInvalidatesCache(t *testing.T) {
	var refreshCalls atomic.Int32
	release := make(chan struct{})
	upstream := codeBuddyUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "/v2/plugin/auth/token/refresh", req.URL.Path)
		call := refreshCalls.Add(1)
		if call == 1 {
			<-release
		}
		return codeBuddyResponse(http.StatusOK, nil, `{"code":0,"data":{"accessToken":"refreshed-`+string(rune('0'+call))+`","expiresIn":3600}}`), nil
	}}
	provider := NewCodeBuddyTokenProvider(nil, upstream)
	account := codeBuddyCLIAccount()
	account.Credentials["expiresAt"] = time.Now().Add(-time.Minute).UnixMilli()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := provider.AccessToken(context.Background(), account)
			errs <- err
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), refreshCalls.Load())
	require.Equal(t, "refresh-secret", account.GetCredential("refreshToken"), "missing rotated refresh token must preserve the old token")

	account.Credentials["accessToken"] = "manually-rotated"
	account.Credentials["refreshToken"] = "manually-rotated-refresh"
	account.Credentials["expiresAt"] = time.Now().Add(-time.Minute).UnixMilli()
	access, err := provider.AccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "refreshed-2", access)
	require.Equal(t, int32(2), refreshCalls.Load())
}
