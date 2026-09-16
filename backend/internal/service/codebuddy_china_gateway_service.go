package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddychina"
)

const codeBuddyChinaFailureBodyMax = 512 << 10

// CodeBuddyChinaGatewayService owns the regional codebuddy.cn data plane.
// Registration and token rotation remain Python-worker responsibilities; the
// request path accepts only the persisted bearer credential.
type CodeBuddyChinaGatewayService struct {
	httpUpstream HTTPUpstream
}

func NewCodeBuddyChinaGatewayService(upstream HTTPUpstream) *CodeBuddyChinaGatewayService {
	return &CodeBuddyChinaGatewayService{httpUpstream: upstream}
}

func (s *CodeBuddyChinaGatewayService) ForwardAsChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	var request apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, writeCodeBuddyChatError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	return s.forward(ctx, c, account, &request, providerChatOutputChat, nil)
}

func (s *CodeBuddyChinaGatewayService) ForwardAsMessages(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	var anthropic apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropic); err != nil {
		return nil, writeCodeBuddyAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	request, err := apicompat.AnthropicToChatCompletionsRequest(&anthropic)
	if err != nil {
		return nil, writeCodeBuddyAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	if anthropic.Stream {
		return s.forward(ctx, c, account, request, providerChatOutputAnthropic, nil)
	}
	request.Stream = false
	recorder := httptest.NewRecorder()
	compat, _ := gin.CreateTestContext(recorder)
	compat.Request = c.Request.Clone(ctx)
	result, err := s.forward(ctx, compat, account, request, providerChatOutputChat, nil)
	if err != nil {
		return nil, err
	}
	var response apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		return nil, errors.New("CodeBuddy China returned an invalid compatibility response")
	}
	c.JSON(http.StatusOK, apicompat.ChatCompletionsResponseToAnthropic(&response, anthropic.Model))
	return result, nil
}

func (s *CodeBuddyChinaGatewayService) ForwardAsResponses(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	var request apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, errors.New("invalid CodeBuddy China Responses request")
	}
	if strings.TrimSpace(request.Model) == "" {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, errors.New("model is required")
	}
	effectiveTools, err := apicompat.EffectiveResponsesTools(&request)
	if err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Invalid tools configuration")
		return nil, errors.New("invalid CodeBuddy China Responses tools")
	}
	chatRequest, err := apicompat.ResponsesToChatCompletionsRequest(&request)
	if err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to convert request")
		return nil, errors.New("invalid CodeBuddy China Responses input")
	}
	bridge := &providerChatResponsesBridge{
		customTools: apicompat.CustomToolNames(effectiveTools), functionTools: apicompat.FunctionToolNames(effectiveTools),
		toolSearch: apicompat.HasToolSearchTool(effectiveTools), namespaceTools: apicompat.NamespaceToolNames(effectiveTools),
	}
	if request.Stream {
		chatRequest.Stream = true
		return s.forward(ctx, c, account, chatRequest, providerChatOutputResponses, bridge)
	}
	chatRequest.Stream = false
	recorder := httptest.NewRecorder()
	compat, _ := gin.CreateTestContext(recorder)
	compat.Request = c.Request.Clone(ctx)
	result, err := s.forward(ctx, compat, account, chatRequest, providerChatOutputChat, nil)
	if err != nil {
		return nil, err
	}
	var chatResponse apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &chatResponse); err != nil {
		return nil, errors.New("CodeBuddy China returned an invalid compatibility response")
	}
	c.JSON(http.StatusOK, apicompat.ChatCompletionsResponseToResponses(
		&chatResponse, request.Model, bridge.customTools, bridge.functionTools, bridge.toolSearch, bridge.namespaceTools,
	))
	return result, nil
}

func (s *CodeBuddyChinaGatewayService) forward(ctx context.Context, c *gin.Context, account *Account, request *apicompat.ChatCompletionsRequest, output providerChatOutputProtocol, responses *providerChatResponsesBridge) (*ForwardResult, error) {
	if account == nil || account.Platform != PlatformCodeBuddyChina {
		return nil, errors.New("invalid CodeBuddy China account")
	}
	if s == nil || s.httpUpstream == nil {
		return nil, errors.New("CodeBuddy China gateway service is not configured")
	}
	if strings.TrimSpace(request.Model) == "" {
		return nil, writeCodeBuddyRequestError(c, output, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	mappedModel, _ := account.ResolveMappedModel(request.Model)
	payload, err := codebuddychina.BuildPayload(request, mappedModel)
	if err != nil {
		return nil, writeCodeBuddyRequestError(c, output, http.StatusBadRequest, "invalid_request_error", "Failed to convert request")
	}
	upstreamModel := codebuddychina.ResolveModel(mappedModel).Upstream
	resp, requestBody, err := s.call(ctx, account, payload)
	if err != nil {
		var tokenErr *CodeBuddyTokenError
		if errors.As(err, &tokenErr) {
			return nil, classifyCodeBuddyChinaHTTPFailure(http.StatusUnauthorized, nil, nil)
		}
		return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: "CodeBuddy China upstream request failed"}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, codeBuddyChinaFailureBodyMax))
		_ = resp.Body.Close()
		return nil, classifyCodeBuddyChinaHTTPFailure(resp.StatusCode, resp.Header, body)
	}
	return consumeCodeBuddyCompatibleStream(c, resp, request, upstreamModel, requestBody, output, responses, "CodeBuddy China")
}

func (s *CodeBuddyChinaGatewayService) call(ctx context.Context, account *Account, payload any) (*http.Response, []byte, error) {
	token := codeBuddyCredential(account, "apiKey", "api_key", "token", "accessToken", "access_token", "sessionToken", "session_token")
	if token == "" {
		return nil, nil, &CodeBuddyTokenError{Status: http.StatusUnauthorized, Revoked: true}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codebuddychina.ChatURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "text/event-stream, application/json, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Conversation-ID", uuid.NewString())
	req.Header.Set("X-Request-ID", strings.ReplaceAll(uuid.NewString(), "-", ""))
	req.Header.Set("X-Domain", "www.codebuddy.cn")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	return resp, body, err
}

type codeBuddyChinaFailureEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func classifyCodeBuddyChinaHTTPFailure(status int, headers http.Header, body []byte) *UpstreamFailoverError {
	var envelope codeBuddyChinaFailureEnvelope
	_ = json.Unmarshal(body, &envelope)
	message := strings.ToLower(strings.TrimSpace(envelope.Msg))
	failure := &UpstreamFailoverError{
		StatusCode: status, ResponseHeaders: codeBuddySafeResponseHeaders(headers), Scope: GatewayFailureScopeAccount,
		NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: "CodeBuddy China upstream request failed",
	}
	switch {
	case status == http.StatusBadRequest && (envelope.Code == 11102 || strings.Contains(message, "service info not found")):
		failure.Scope = GatewayFailureScopeRequest
		failure.NextAccountAction = NextAccountStop
		failure.ClientStatusCode = http.StatusBadRequest
		failure.ClientMessage = "CodeBuddy China does not support the requested model"
		failure.Reason = GatewayFailureReason("model_not_supported")
	case (status == http.StatusBadRequest || status == http.StatusForbidden) && envelope.Code == 11140:
		failure.Scope = GatewayFailureScopeRequest
		failure.NextAccountAction = NextAccountStop
		failure.ClientStatusCode = http.StatusBadRequest
		failure.ClientMessage = "CodeBuddy China rejected the request content"
		failure.Reason = GatewayFailureReason("content_safety_rejected")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		failure.Stage = GatewayFailureStageAccountAuth
		failure.ClientStatusCode = http.StatusUnauthorized
		failure.ClientMessage = "CodeBuddy China credential was rejected"
		failure.Reason = GatewayFailureReason("credential_rejected")
	case status == http.StatusTooManyRequests:
		failure.ClientStatusCode = http.StatusTooManyRequests
		failure.ClientMessage = "CodeBuddy China quota or rate limit reached"
		failure.Reason = GatewayFailureReason("quota_or_rate_limit")
		if resetAt := parseRetryAfterResetTime(headers, time.Now()); resetAt != nil {
			failure.SameAccountRetryDeadline = *resetAt
		}
	case status >= http.StatusInternalServerError:
		failure.Reason = GatewayFailureReason("upstream_transient")
	default:
		failure.Scope = GatewayFailureScopeRequest
		failure.NextAccountAction = NextAccountStop
		failure.ClientStatusCode = http.StatusBadRequest
		failure.ClientMessage = "CodeBuddy China rejected the request"
		failure.Reason = GatewayFailureReason("request_rejected")
	}
	return failure
}
