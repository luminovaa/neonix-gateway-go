package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/kiro"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type kiroUpstreamStub struct {
	responses []*http.Response
	requests  []*http.Request
	bodies    [][]byte
}

type kiroAccountRepoStub struct {
	AccountRepository
	account *Account
}

func (r *kiroAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account == nil || r.account.ID != id {
		return nil, ErrAccountNotFound
	}
	return r.account, nil
}

func (s *kiroUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	s.requests = append(s.requests, req)
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	s.bodies = append(s.bodies, body)
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

func (s *kiroUpstreamStub) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxy, id, concurrency)
}

func TestKiroGatewayNonStreamingContractAndCredentialShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := append(kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"hello"}`)), kiroServiceFrame("messageMetadataEvent", []byte(`{"tokenUsage":{"uncachedInputTokens":8,"outputTokens":3}}`))...)
	stub := &kiroUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Header: http.Header{"X-Amzn-Requestid": []string{"req-1"}}, Body: io.NopCloser(bytes.NewReader(stream))}}}
	svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret", "profileArn": "arn:test"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.Equal(t, "req-1", result.RequestID)
	require.Equal(t, 8, result.Usage.InputTokens)
	var response map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, "chat.completion", response["object"])
	require.Contains(t, recorder.Body.String(), "hello")
	require.Equal(t, "Bearer secret", stub.requests[0].Header.Get("Authorization"))
	require.NotContains(t, string(stub.bodies[0]), "secret")
	require.Contains(t, stub.requests[0].URL.String(), "q.us-east-1.amazonaws.com")
}

func TestKiroGatewayResponsesBufferedAndStreamingContracts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{ID: 42, Platform: PlatformKiro, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret", "profileArn": "arn:test"}}

	t.Run("buffered", func(t *testing.T) {
		stream := append(
			kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"buffered response"}`)),
			kiroServiceFrame("messageMetadataEvent", []byte(`{"tokenUsage":{"uncachedInputTokens":5,"outputTokens":2}}`))...,
		)
		stub := &kiroUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(stream))}}}
		svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		result, err := svc.ForwardAsResponses(context.Background(), c, account, []byte(`{"model":"kiro/MODEL_A","input":"hello"}`))
		require.NoError(t, err)
		require.False(t, result.Stream)
		require.Contains(t, recorder.Body.String(), `"object":"response"`)
		require.Contains(t, recorder.Body.String(), "buffered response")
		require.NotContains(t, recorder.Body.String(), "chat.completion")
	})

	t.Run("streaming", func(t *testing.T) {
		stream := append(
			kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"streamed response"}`)),
			kiroServiceFrame("messageMetadataEvent", []byte(`{"tokenUsage":{"uncachedInputTokens":4,"outputTokens":2}}`))...,
		)
		stub := &kiroUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(stream))}}}
		svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		result, err := svc.ForwardAsResponses(context.Background(), c, account, []byte(`{"model":"kiro/MODEL_A","stream":true,"input":"hello"}`))
		require.NoError(t, err)
		require.True(t, result.Stream)
		require.Contains(t, recorder.Body.String(), "event: response.created")
		require.Contains(t, recorder.Body.String(), "streamed response")
		require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.completed"))
		require.True(t, strings.HasSuffix(recorder.Body.String(), "data: [DONE]\n\n"))
	})
}

func TestKiroGatewayUsesMappedNativeModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"hello"}`))
	stub := &kiroUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(stream))}}}
	svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{
		"accessToken":   "secret",
		"model_mapping": map[string]any{"claude-public": "CLAUDE_NATIVE_MODEL"},
	}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"claude-public","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.Equal(t, "CLAUDE_NATIVE_MODEL", result.UpstreamModel)
	require.Contains(t, string(stub.bodies[0]), `"modelId":"CLAUDE_NATIVE_MODEL"`)
	require.NotContains(t, string(stub.bodies[0]), `"modelId":"claude-public"`)
}

func TestKiroGatewayDoesNotFallbackAfterStreamingOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := append(kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"first"}`)), []byte{0, 0, 0, 16}...)
	stub := &kiroUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(stream))}, {StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}}}
	svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	require.Error(t, err)
	require.Len(t, stub.requests, 1)
	require.Contains(t, recorder.Body.String(), "first")
}

func TestKiroGatewayFallsBackBeforeFirstByte(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"from-fallback"}`))
	stub := &kiroUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"30"}}, Body: io.NopCloser(strings.NewReader(`{"message":"quota"}`))},
		{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(stream))},
	}}
	svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.Len(t, stub.requests, 2)
	require.Contains(t, stub.requests[0].URL.String(), "q.us-east-1.amazonaws.com")
	require.Contains(t, stub.requests[1].URL.String(), "codewhisperer.us-east-1.amazonaws.com")
	require.Contains(t, string(stub.bodies[1]), `"modelId":"CLAUDE_SONNET_4_20250514_V1_0"`)
	require.Contains(t, recorder.Body.String(), "from-fallback")
}

func TestKiroGatewayFallsBackOnEmptyStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"after empty"}`))
	stub := &kiroUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil))},
		{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(stream))},
	}}
	svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.Len(t, stub.requests, 2)
	require.Contains(t, recorder.Body.String(), "after empty")
}

func TestKiroGatewayTriesCodeWhispererAfterRequestScopedAmazonQFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"fallback"}`))
	stub := &kiroUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"message":"model unsupported"}`))},
		{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(stream))},
	}}
	svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"friendly-model","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.Len(t, stub.requests, 2)
	require.Contains(t, string(stub.bodies[1]), `"modelId":"CLAUDE_SONNET_4_20250514_V1_0"`)
}

func TestKiroGatewayCredentialFailureUsesProviderSpecificSafeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &kiroUpstreamStub{}
	svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Concurrency: 1}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, KiroCredentialUnavailableReason, failover.Reason)
	require.Equal(t, KiroCredentialUnavailableClientMessage, failover.ClientMessage)
	require.NotContains(t, failover.ClientMessage, "token")
}

func TestKiroGatewayAnthropicMessagesContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"hello anthropic"}`))
	stub := &kiroUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(stream))}}}
	svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	result, err := svc.ForwardAsMessages(context.Background(), c, account, []byte(`{"model":"m","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.False(t, result.Stream)
	require.Contains(t, recorder.Body.String(), "hello anthropic")
	require.Contains(t, recorder.Body.String(), `"type":"message"`)
}

func TestKiroGatewayAnthropicThinkingFieldsAndUnsupportedFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"fallback works"}`))
	stub := &kiroUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"message":"additionalModelRequestFields not supported: request_body_invalid"}`))},
		{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(stream))},
	}}
	svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, err := svc.ForwardAsMessages(context.Background(), c, account, []byte(`{"model":"m","max_tokens":128,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.Len(t, stub.requests, 2)
	require.Contains(t, string(stub.bodies[0]), `"additionalModelRequestFields":{"thinking":{"budget_tokens":2048,"type":"enabled"}}`)
	require.NotContains(t, string(stub.bodies[1]), "additionalModelRequestFields")
	require.Contains(t, recorder.Body.String(), "fallback works")
}

func TestKiroGatewayAnthropicMessagesStreamingContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"hello stream"}`))
	stub := &kiroUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(stream))}}}
	svc := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	result, err := svc.ForwardAsMessages(context.Background(), c, account, []byte(`{"model":"m","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.True(t, result.Stream)
	require.Contains(t, recorder.Body.String(), "event: message_start")
	require.Contains(t, recorder.Body.String(), "hello stream")
	require.Contains(t, recorder.Body.String(), "event: message_stop")
}

func TestKiroGatewayAnthropicStreamingRefreshesBeforeOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"after refresh"}`))
	stub := &kiroUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"message":"expired"}`))},
		{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}`))},
		{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(stream))},
	}}
	provider := NewKiroTokenProvider(nil, stub)
	svc := NewKiroGatewayService(provider, stub)
	account := &Account{ID: 42, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "old-access", "refreshToken": "old-refresh", "authMethod": "social"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	result, err := svc.ForwardAsMessages(context.Background(), c, account, []byte(`{"model":"m","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.True(t, result.Stream)
	require.Len(t, stub.requests, 3)
	require.Equal(t, "Bearer old-access", stub.requests[0].Header.Get("Authorization"))
	require.Equal(t, kiroSocialRefreshURL, stub.requests[1].URL.String())
	require.Equal(t, "Bearer new-access", stub.requests[2].Header.Get("Authorization"))
	require.Contains(t, recorder.Body.String(), "after refresh")
}

func TestPrepareKiroCLIEndpointPayloadUpdatesEntireConversation(t *testing.T) {
	payload := &kiro.Payload{ConversationState: kiro.ConversationState{
		AgentContinuationID: "continuation", AgentTaskType: "vibe",
		CurrentMessage: kiro.HistoryMessage{UserInputMessage: &kiro.UserInputMessage{ModelID: "MODEL_NATIVE", Origin: "AI_EDITOR"}},
		History:        []kiro.HistoryMessage{{UserInputMessage: &kiro.UserInputMessage{ModelID: "MODEL_NATIVE", Origin: "AI_EDITOR"}}},
	}}
	prepared := prepareKiroEndpointPayload(payload, kiroEndpoint{name: "amazonq-cli", origin: "AmazonQ", cli: true})
	require.Empty(t, prepared.ConversationState.AgentContinuationID)
	require.Empty(t, prepared.ConversationState.AgentTaskType)
	require.Equal(t, "AmazonQ", prepared.ConversationState.CurrentMessage.UserInputMessage.Origin)
	require.Equal(t, "AmazonQ", prepared.ConversationState.History[0].UserInputMessage.Origin)
	require.Equal(t, "AI_EDITOR", payload.ConversationState.History[0].UserInputMessage.Origin, "source payload must remain unchanged for later endpoint attempts")
}

func TestKiroTokenProviderRefreshPreservesRotatedToken(t *testing.T) {
	stub := &kiroUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"accessToken":"new-access","expiresIn":3600}`))}}}
	account := &Account{ID: 9, Credentials: map[string]any{"refreshToken": "old-refresh", "authMethod": "social"}}
	provider := NewKiroTokenProvider(nil, stub)
	provider.now = func() time.Time { return time.Unix(1000, 0) }
	token, err := provider.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "new-access", token)
	require.Equal(t, "old-refresh", account.Credentials["refreshToken"])
	require.Equal(t, "new-access", account.Credentials["accessToken"])
	require.Equal(t, int64(4600000), account.Credentials["expiresAt"])
}

func TestKiroTokenProviderRefreshesOIDCCredentialsInConfiguredRegion(t *testing.T) {
	stub := &kiroUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":1800}`))}}}
	account := &Account{ID: 10, Credentials: map[string]any{
		"refresh_token": "old-refresh", "auth_method": "idc",
		"client_id": "client-id", "client_secret": "client-secret", "region": "eu-west-1",
	}}
	provider := NewKiroTokenProvider(nil, stub)
	provider.now = func() time.Time { return time.Unix(2000, 0) }
	token, err := provider.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "new-access", token)
	require.Equal(t, "https://oidc.eu-west-1.amazonaws.com/token", stub.requests[0].URL.String())
	require.JSONEq(t, `{"clientId":"client-id","clientSecret":"client-secret","refreshToken":"old-refresh","grantType":"refresh_token"}`, string(stub.bodies[0]))
	require.Equal(t, "new-access", account.Credentials["accessToken"])
	require.Equal(t, "new-refresh", account.Credentials["refresh_token"])
	require.Equal(t, int64(3800000), account.Credentials["expiresAt"])
}

func TestKiroTokenProviderSocialFallsBackToOIDC(t *testing.T) {
	stub := &kiroUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader(`{"error":"temporary"}`))},
		{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"accessToken":"oidc-access","expiresIn":3600}`))},
	}}
	account := &Account{ID: 11, Credentials: map[string]any{
		"refreshToken": "refresh", "authMethod": "social",
		"clientId": "client-id", "clientSecret": "client-secret", "region": "us-west-2",
	}}
	provider := NewKiroTokenProvider(nil, stub)
	token, err := provider.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "oidc-access", token)
	require.Len(t, stub.requests, 2)
	require.Equal(t, kiroSocialRefreshURL, stub.requests[0].URL.String())
	require.Equal(t, "https://oidc.us-west-2.amazonaws.com/token", stub.requests[1].URL.String())
}

func TestKiroModelDiscoveryPaginatesNativeCatalog(t *testing.T) {
	stub := &kiroUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"models":[{"modelId":"MODEL_A","modelName":"A"}],"nextToken":"next page"}`))},
		{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"models":[{"modelId":"MODEL_B","modelName":"B"}]}`))},
	}}
	kiroGateway := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	service := NewAccountTestService(nil, nil, nil, nil, nil, kiroGateway, stub, nil, nil)
	account := &Account{ID: 12, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	models, err := service.FetchUpstreamSupportedModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"MODEL_A", "MODEL_B"}, models)
	require.Len(t, stub.requests, 2)
	require.Equal(t, "next page", stub.requests[1].URL.Query().Get("nextToken"))
	require.Equal(t, "Bearer secret", stub.requests[1].Header.Get("Authorization"))
}

func TestKiroModelDiscoveryRejectsRepeatedPaginationToken(t *testing.T) {
	stub := &kiroUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"models":[{"modelId":"MODEL_A"}],"nextToken":"same"}`))},
		{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"models":[{"modelId":"MODEL_B"}],"nextToken":"same"}`))},
	}}
	kiroGateway := NewKiroGatewayService(NewKiroTokenProvider(nil, stub), stub)
	service := NewAccountTestService(nil, nil, nil, nil, nil, kiroGateway, stub, nil, nil)
	account := &Account{ID: 12, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	_, err := service.FetchUpstreamSupportedModels(context.Background(), account)
	require.ErrorContains(t, err, "repeated a pagination token")
	require.Len(t, stub.requests, 2)
}

func TestAccountTestServiceKiroConnectionSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := kiroServiceFrame("assistantResponseEvent", []byte(`{"content":"ok"}`))
	stub := &kiroUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(stream))}}}
	account := &Account{ID: 12, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	repo := &kiroAccountRepoStub{account: account}
	kiroGateway := NewKiroGatewayService(NewKiroTokenProvider(repo, stub), stub)
	svc := NewAccountTestService(repo, nil, nil, nil, nil, kiroGateway, stub, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/12/test", nil)

	require.NoError(t, svc.TestAccountConnection(c, account.ID, "MODEL_A", "hi", ""))
	require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
	require.Contains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestServiceKiroConnectionFailureIsSanitized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &kiroUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"accessToken=must-not-leak"}`))},
	}}
	account := &Account{ID: 12, Platform: PlatformKiro, Concurrency: 1, Credentials: map[string]any{"accessToken": "secret"}}
	repo := &kiroAccountRepoStub{account: account}
	kiroGateway := NewKiroGatewayService(NewKiroTokenProvider(repo, stub), stub)
	svc := NewAccountTestService(repo, nil, nil, nil, nil, kiroGateway, stub, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/12/test", nil)

	err := svc.TestAccountConnection(c, account.ID, "MODEL_A", "hi", "")
	require.Error(t, err)
	require.NotContains(t, recorder.Body.String(), "must-not-leak")
	require.NotContains(t, recorder.Body.String(), "accessToken")
}

func kiroServiceFrame(eventType string, payload []byte) []byte {
	name, value := []byte(":event-type"), []byte(eventType)
	headers := append([]byte{byte(len(name))}, name...)
	headers = append(headers, 7, byte(len(value)>>8), byte(len(value)))
	headers = append(headers, value...)
	total := 12 + len(headers) + len(payload) + 4
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payload)
	binary.BigEndian.PutUint32(frame[total-4:], crc32.ChecksumIEEE(frame[:total-4]))
	return frame
}
