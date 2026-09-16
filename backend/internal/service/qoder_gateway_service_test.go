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

type qoderUpstreamStub struct {
	do func(*http.Request) (*http.Response, error)
}

func (s qoderUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return s.do(req)
}

func (s qoderUpstreamStub) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxy, id, concurrency)
}

func qoderJSONResponse(status int, value any) *http.Response {
	raw, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}
}

func qoderJobTokenResponse() *http.Response {
	return qoderJSONResponse(http.StatusOK, map[string]any{"id": "user-1", "securityOauthToken": "security", "refreshToken": "refresh", "expireTime": int64(4102444800000), "userType": "personal_standard"})
}

func qoderSSE(events ...string) *http.Response {
	var stream strings.Builder
	for _, inner := range events {
		wrapper, _ := json.Marshal(map[string]any{"body": inner})
		stream.WriteString("data: " + string(wrapper) + "\n\n")
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream.String()))}
}

func qoderServiceWithChat(t *testing.T, chat func(*http.Request) (*http.Response, error)) *QoderGatewayService {
	t.Helper()
	upstream := qoderUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "center.qoder.sh" {
			return qoderJobTokenResponse(), nil
		}
		return chat(req)
	}}
	return NewQoderGatewayService(NewQoderTokenProvider(nil, upstream), upstream)
}

func qoderTestAccount() *Account {
	return &Account{ID: 77, Name: "qoder-test", Platform: PlatformQoder, Credentials: map[string]any{"token": "pat"}, Concurrency: 1}
}

func TestQoderChatCompletionExchangesTokenAndStreams(t *testing.T) {
	var sawJob, sawChat bool
	upstream := qoderUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "center.qoder.sh":
			sawJob = true
			require.Equal(t, "cosy", req.Header.Get("appcode"))
			return qoderJSONResponse(http.StatusOK, map[string]any{"id": "user-1", "securityOauthToken": "security", "refreshToken": "refresh", "expireTime": int64(4102444800000), "userType": "personal_standard"}), nil
		case "api3.qoder.sh":
			sawChat = true
			require.True(t, strings.HasPrefix(req.Header.Get("Authorization"), "Bearer COSY."))
			inner := "{\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}"
			wrapper, _ := json.Marshal(map[string]any{"body": inner})
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + string(wrapper) + "\n\n"))}, nil
		default:
			t.Fatalf("unexpected host %s", req.URL.Host)
			return nil, nil
		}
	}}
	svc := NewQoderGatewayService(NewQoderTokenProvider(nil, upstream), upstream)
	account := &Account{ID: 7, Platform: PlatformQoder, Credentials: map[string]any{"token": "pat"}, Concurrency: 1}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte("{\"model\":\"qr/Lite\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":false}")
	result, err := svc.ForwardAsChatCompletions(context.Background(), ctx, account, body)
	require.NoError(t, err)
	require.True(t, sawJob && sawChat)
	require.Equal(t, "lite", result.UpstreamModel)
	require.NotContains(t, rec.Body.String(), "security")
	require.Contains(t, rec.Body.String(), "hello")
}

func TestQoderQuotaFailureIsSanitized(t *testing.T) {
	upstream := qoderUpstreamStub{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "center.qoder.sh" {
			return qoderJSONResponse(200, map[string]any{"id": "u", "securityOauthToken": "secret"}), nil
		}
		return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("upstream secret"))}, nil
	}}
	svc := NewQoderGatewayService(NewQoderTokenProvider(nil, upstream), upstream)
	account := &Account{ID: 8, Platform: PlatformQoder, Credentials: map[string]any{"token": "pat"}, Concurrency: 1}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	_, err := svc.ForwardAsChatCompletions(context.Background(), ctx, account, []byte("{\"model\":\"qr/Lite\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"))
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, 429, failure.ClientStatusCode)
	require.NotContains(t, failure.ClientMessage, "secret")
	require.Empty(t, failure.ResponseBody)
}

func TestQoderHTTPFailureScopeAndRetryAfter(t *testing.T) {
	requestFailure := classifyQoderHTTPFailure(http.StatusNotFound, http.Header{})
	require.Equal(t, GatewayFailureScopeRequest, requestFailure.Scope)
	require.Equal(t, NextAccountStop, requestFailure.NextAccountAction)

	credentialFailure := classifyQoderHTTPFailure(http.StatusUnauthorized, http.Header{})
	require.True(t, credentialFailure.IsCredentialFailure())
	require.Equal(t, GatewayFailureScopeAccount, credentialFailure.Scope)
	require.Equal(t, NextAccountRetry, credentialFailure.NextAccountAction)

	before := time.Now().Add(16 * time.Second)
	quotaFailure := classifyQoderHTTPFailure(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"17"}})
	require.False(t, quotaFailure.IsCredentialFailure())
	require.True(t, quotaFailure.SameAccountRetryDeadline.After(before))
	require.True(t, quotaFailure.SameAccountRetryDeadline.Before(time.Now().Add(18*time.Second)))
}

func TestQoderFailureOnlyRetainsSafeResponseHeaders(t *testing.T) {
	failure := classifyQoderHTTPFailure(http.StatusTooManyRequests, http.Header{
		"Retry-After":  []string{"17"},
		"Set-Cookie":   []string{"session=secret"},
		"X-Diagnostic": []string{"token-secret"},
	})
	require.Equal(t, "17", failure.ResponseHeaders.Get("Retry-After"))
	require.Empty(t, failure.ResponseHeaders.Get("Set-Cookie"))
	require.Empty(t, failure.ResponseHeaders.Get("X-Diagnostic"))
}

func TestQoderStreamingChatEmitsOneTerminalChunkAndFallbackUsage(t *testing.T) {
	svc := qoderServiceWithChat(t, func(*http.Request) (*http.Response, error) {
		return qoderSSE(
			`{"choices":[{"delta":{"content":"hello"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		), nil
	})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, qoderTestAccount(), []byte(`{"model":"qr/Lite","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.True(t, result.Stream)
	body := recorder.Body.String()
	require.Equal(t, 1, strings.Count(body, `"finish_reason":"stop"`))
	require.Contains(t, body, `"prompt_tokens":`)
	require.Contains(t, body, "data: [DONE]")
	require.Greater(t, result.Usage.InputTokens, 0)
	require.Greater(t, result.Usage.OutputTokens, 0)
}

func TestQoderStreamingAnthropicProducesSingleTerminalEvent(t *testing.T) {
	svc := qoderServiceWithChat(t, func(*http.Request) (*http.Response, error) {
		return qoderSSE(
			`{"choices":[{"delta":{"content":"hello"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`,
		), nil
	})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, err := svc.ForwardAsMessages(context.Background(), c, qoderTestAccount(), []byte(`{"model":"qr/Lite","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	body := recorder.Body.String()
	require.Contains(t, body, "event: message_start")
	require.Contains(t, body, "event: content_block_delta")
	require.Equal(t, 1, strings.Count(body, "event: message_stop"))
}

func TestQoderResponsesStreamingAndBuffered(t *testing.T) {
	newService := func() *QoderGatewayService {
		return qoderServiceWithChat(t, func(*http.Request) (*http.Response, error) {
			return qoderSSE(
				`{"choices":[{"delta":{"content":"hello"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			), nil
		})
	}

	streamRecorder := httptest.NewRecorder()
	streamContext, _ := gin.CreateTestContext(streamRecorder)
	streamContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := newService().ForwardAsResponses(context.Background(), streamContext, qoderTestAccount(), []byte(`{"model":"qr/Lite","stream":true,"input":"hi"}`))
	require.NoError(t, err)
	require.Contains(t, streamRecorder.Body.String(), "event: response.created")
	require.Contains(t, streamRecorder.Body.String(), "event: response.output_text.delta")
	require.Equal(t, 1, strings.Count(streamRecorder.Body.String(), "event: response.completed"))
	require.Contains(t, streamRecorder.Body.String(), `"usage":{"input_tokens":`)

	bufferedRecorder := httptest.NewRecorder()
	bufferedContext, _ := gin.CreateTestContext(bufferedRecorder)
	bufferedContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err = newService().ForwardAsResponses(context.Background(), bufferedContext, qoderTestAccount(), []byte(`{"model":"qr/Lite","input":"hi"}`))
	require.NoError(t, err)
	require.Equal(t, "response", jsonField(t, bufferedRecorder.Body.Bytes(), "object"))
	require.Equal(t, "completed", jsonField(t, bufferedRecorder.Body.Bytes(), "status"))
}

func jsonField(t *testing.T, body []byte, key string) string {
	t.Helper()
	var value map[string]any
	require.NoError(t, json.Unmarshal(body, &value))
	text, _ := value[key].(string)
	return text
}

func TestQoderEmptyAndReasoningOnlyStreamsCanFailOver(t *testing.T) {
	tests := []struct {
		name   string
		events []string
	}{
		{name: "empty"},
		{name: "reasoning only", events: []string{`{"choices":[{"delta":{"reasoning_content":"thinking"},"finish_reason":"stop"}]}`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := qoderServiceWithChat(t, func(*http.Request) (*http.Response, error) { return qoderSSE(tt.events...), nil })
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			_, err := svc.ForwardAsChatCompletions(context.Background(), c, qoderTestAccount(), []byte(`{"model":"qr/Lite","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Empty(t, recorder.Body.String())
		})
	}
}

type qoderErrorAfterReader struct {
	reader *strings.Reader
	err    error
}

func (r *qoderErrorAfterReader) Read(p []byte) (int, error) {
	if r.reader.Len() > 0 {
		return r.reader.Read(p)
	}
	return 0, r.err
}

func TestQoderDoesNotFailOverAfterSemanticOutput(t *testing.T) {
	inner := `{"choices":[{"delta":{"content":"visible"}}]}`
	wrapper, _ := json.Marshal(map[string]any{"body": inner})
	readErr := errors.New("stream interrupted")
	svc := qoderServiceWithChat(t, func(*http.Request) (*http.Response, error) {
		reader := &qoderErrorAfterReader{reader: strings.NewReader("data: " + string(wrapper) + "\n\n"), err: readErr}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(reader)}, nil
	})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, qoderTestAccount(), []byte(`{"model":"qr/Lite","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	require.ErrorIs(t, err, readErr)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.Contains(t, recorder.Body.String(), "visible")
}

func TestQoderToolFragmentsUseStableGeneratedID(t *testing.T) {
	svc := qoderServiceWithChat(t, func(*http.Request) (*http.Response, error) {
		return qoderSSE(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"run","arguments":"{"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\":1}"}}]},"finish_reason":"tool_calls"}]}`,
		), nil
	})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, qoderTestAccount(), []byte(`{"model":"qr/Lite","stream":true,"messages":[{"role":"user","content":"run"}]}`))
	require.NoError(t, err)
	matches := regexp.MustCompile(`"id":"(call_[^"]+)"`).FindAllStringSubmatch(recorder.Body.String(), -1)
	require.NotEmpty(t, matches)
	for _, match := range matches {
		require.Equal(t, matches[0][1], match[1])
	}
}

func TestQoderResponsesToolFragmentsKeepStableCallID(t *testing.T) {
	svc := qoderServiceWithChat(t, func(*http.Request) (*http.Response, error) {
		return qoderSSE(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_stable","type":"function","function":{"name":"run","arguments":"{"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}\n"}}]},"finish_reason":"tool_calls"}]}`,
		), nil
	})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := svc.ForwardAsResponses(context.Background(), c, qoderTestAccount(), []byte(`{"model":"qr/Lite","stream":true,"input":"run","tools":[{"type":"function","name":"run","parameters":{"type":"object"}}]}`))
	require.NoError(t, err)
	body := recorder.Body.String()
	require.NotContains(t, body, "call_"+"stable2")
	require.GreaterOrEqual(t, strings.Count(body, `"call_id":"call_stable"`), 2)
	require.Equal(t, 1, strings.Count(body, "event: response.completed"))
}

type qoderContextReader struct{ ctx context.Context }

func (r qoderContextReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestQoderContextCancellationClosesUpstreamRead(t *testing.T) {
	svc := qoderServiceWithChat(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(qoderContextReader{ctx: req.Context()})}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := svc.ForwardAsChatCompletions(ctx, c, qoderTestAccount(), []byte(`{"model":"qr/Lite","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Qoder upstream read did not stop after cancellation")
	}
}

func TestQoderTokenCacheInvalidationAndRefreshFallback(t *testing.T) {
	var calls atomic.Int32
	upstream := qoderUpstreamStub{do: func(*http.Request) (*http.Response, error) {
		n := calls.Add(1)
		return qoderJSONResponse(200, map[string]any{"id": "u", "securityOauthToken": "access-" + string(rune('0'+n)), "expireTime": int64(4102444800000)}), nil
	}}
	provider := NewQoderTokenProvider(nil, upstream)
	account := qoderTestAccount()
	account.Credentials["refreshToken"] = "keep-refresh"
	first, err := provider.Tokens(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "keep-refresh", first.RefreshToken)
	require.Equal(t, "keep-refresh", account.GetCredential("refreshToken"))
	account.Credentials["token"] = "new-pat"
	second, err := provider.Tokens(context.Background(), account)
	require.NoError(t, err)
	require.NotEqual(t, first.SecurityOAuthToken, second.SecurityOAuthToken)
	require.Equal(t, int32(2), calls.Load())
}

func TestQoderConcurrentTokenExchangeIsSingleFlight(t *testing.T) {
	var calls atomic.Int32
	upstream := qoderUpstreamStub{do: func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return qoderJobTokenResponse(), nil
	}}
	provider := NewQoderTokenProvider(nil, upstream)
	account := qoderTestAccount()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := provider.Tokens(context.Background(), account)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), calls.Load())
}

func TestQoderJobTokenErrorsAreClassifiedWithoutSecrets(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := qoderUpstreamStub{do: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"17"}}, Body: io.NopCloser(strings.NewReader("pat-secret upstream-secret"))}, nil
			}}
			provider := NewQoderTokenProvider(nil, upstream)
			_, tokenErr := provider.Tokens(context.Background(), qoderTestAccount())
			require.Error(t, tokenErr)
			require.NotContains(t, tokenErr.Error(), "pat-secret")
			failure := qoderTokenFailure(tokenErr)
			var classified *UpstreamFailoverError
			require.ErrorAs(t, failure, &classified)
			require.Equal(t, status, classified.StatusCode)
			if status == http.StatusUnauthorized {
				require.True(t, classified.IsCredentialFailure())
			} else {
				require.False(t, classified.IsCredentialFailure())
			}
			require.NotContains(t, classified.ClientMessage, "secret")
		})
	}
}

func TestQoderRejectsIncompleteJobTokenResponse(t *testing.T) {
	upstream := qoderUpstreamStub{do: func(*http.Request) (*http.Response, error) {
		return qoderJSONResponse(200, map[string]any{"id": "u"}), nil
	}}
	provider := NewQoderTokenProvider(nil, upstream)
	_, err := provider.Tokens(context.Background(), qoderTestAccount())
	require.EqualError(t, err, "Qoder job-token response did not contain an access token")
}
