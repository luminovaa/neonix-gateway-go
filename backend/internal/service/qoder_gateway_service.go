package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/qoder"
)

const qoderChatURL = "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

type QoderGatewayService struct {
	tokens       *QoderTokenProvider
	httpUpstream HTTPUpstream
}

func NewQoderGatewayService(tokens *QoderTokenProvider, upstream HTTPUpstream) *QoderGatewayService {
	return &QoderGatewayService{tokens: tokens, httpUpstream: upstream}
}

func qoderProviderChatEvent(event qoder.Event) providerChatEvent {
	return providerChatEvent{
		Delta: event.Delta, FinishReason: event.FinishReason,
		Usage: providerChatUsage{InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens},
	}
}

func (s *QoderGatewayService) ForwardAsChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	var request apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, writeQoderChatError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	return s.forward(ctx, c, account, &request, providerChatOutputChat, nil)
}
func (s *QoderGatewayService) ForwardAsMessages(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	var anthropic apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropic); err != nil {
		return nil, writeQoderAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	request, err := apicompat.AnthropicToChatCompletionsRequest(&anthropic)
	if err != nil {
		return nil, writeQoderAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	if anthropic.Stream {
		return s.forward(ctx, c, account, request, providerChatOutputAnthropic, nil)
	}
	request.Stream = false
	rec := httptest.NewRecorder()
	compat, _ := gin.CreateTestContext(rec)
	compat.Request = c.Request.Clone(ctx)
	result, err := s.forward(ctx, compat, account, request, providerChatOutputChat, nil)
	if err != nil {
		return nil, err
	}
	var response apicompat.ChatCompletionsResponse
	if json.Unmarshal(rec.Body.Bytes(), &response) != nil {
		return nil, errors.New("Qoder returned an invalid compatibility response")
	}
	c.JSON(http.StatusOK, apicompat.ChatCompletionsResponseToAnthropic(&response, anthropic.Model))
	return result, nil
}

// ForwardAsResponses bridges the [OI] Responses API to Qoder's native chat
// stream without a request-time dependency on the retired Node backend.
func (s *QoderGatewayService) ForwardAsResponses(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	var request apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, errors.New("invalid Qoder Responses request")
	}
	if strings.TrimSpace(request.Model) == "" {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, errors.New("model is required")
	}
	effectiveTools, err := apicompat.EffectiveResponsesTools(&request)
	if err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Invalid tools configuration")
		return nil, errors.New("invalid Qoder Responses tools")
	}
	chatRequest, err := apicompat.ResponsesToChatCompletionsRequest(&request)
	if err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to convert request")
		return nil, errors.New("invalid Qoder Responses input")
	}
	// Qoder accepts tools but not the [OI] tool_choice wire shape. BuildPayload
	// also intentionally omits it; clear it here to keep that invariant explicit.
	chatRequest.ToolChoice = nil
	bridge := &providerChatResponsesBridge{
		customTools:    apicompat.CustomToolNames(effectiveTools),
		functionTools:  apicompat.FunctionToolNames(effectiveTools),
		toolSearch:     apicompat.HasToolSearchTool(effectiveTools),
		namespaceTools: apicompat.NamespaceToolNames(effectiveTools),
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
		return nil, errors.New("Qoder returned an invalid compatibility response")
	}
	response := apicompat.ChatCompletionsResponseToResponses(
		&chatResponse, request.Model, bridge.customTools, bridge.functionTools, bridge.toolSearch, bridge.namespaceTools,
	)
	c.JSON(http.StatusOK, response)
	return result, nil
}

func (s *QoderGatewayService) forward(ctx context.Context, c *gin.Context, account *Account, request *apicompat.ChatCompletionsRequest, output providerChatOutputProtocol, responses *providerChatResponsesBridge) (*ForwardResult, error) {
	if account == nil || account.Platform != PlatformQoder {
		return nil, errors.New("invalid Qoder account")
	}
	if s == nil || s.tokens == nil || s.httpUpstream == nil {
		return nil, errors.New("Qoder gateway service is not configured")
	}
	if strings.TrimSpace(request.Model) == "" {
		return nil, writeQoderRequestError(c, output, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	actual := qoder.ActualModel(account.GetMappedModel(request.Model))
	tokens, err := s.tokens.Tokens(ctx, account)
	if err != nil {
		return nil, qoderTokenFailure(err)
	}
	payload, err := qoder.BuildPayload(request, actual, tokens.UserType)
	if err != nil {
		return nil, writeQoderRequestError(c, output, http.StatusBadRequest, "invalid_request_error", "Failed to convert request")
	}
	resp, requestBody, err := s.call(ctx, account, tokens, payload)
	if err != nil {
		return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Qoder upstream request failed"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := classifyQoderHTTPFailure(resp.StatusCode, resp.Header)
		// Qoder errors are classified from status and Retry-After only. Drain a
		// bounded amount for connection reuse, but never retain an upstream body:
		// it may contain credential or session diagnostics and generic handlers can
		// otherwise expose it through passthrough rules or operational logs.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512<<10))
		resp.Body.Close()
		return nil, failure
	}
	return s.consume(ctx, c, resp, request, actual, requestBody, output, responses)
}

func (s *QoderGatewayService) call(ctx context.Context, account *Account, tokens QoderTokens, payload any) (*http.Response, []byte, error) {
	encoded, err := qoder.EncodePayload(payload)
	if err != nil {
		return nil, nil, err
	}
	identity := map[string]any{"name": tokens.UserName, "aid": tokens.UserID, "uid": tokens.UserID, "yx_uid": "", "organization_id": "", "organization_name": "", "user_type": tokens.UserType, "security_oauth_token": tokens.SecurityOAuthToken, "refresh_token": tokens.RefreshToken}
	cosyKey, info, err := qoder.EncryptSession(identity)
	if err != nil {
		return nil, nil, err
	}
	envelope, _ := json.Marshal(map[string]any{"cosyVersion": qoderCosyVersion, "ideVersion": "", "info": info, "requestId": uuid.NewString(), "version": "v1"})
	payloadB64 := base64.StdEncoding.EncodeToString(envelope)
	date := fmt.Sprint(time.Now().Unix())
	parsed, _ := url.Parse(qoderChatURL)
	path := strings.TrimPrefix(parsed.Path, "/algo")
	signature := qoder.MD5Hex(payloadB64 + "\n" + cosyKey + "\n" + date + "\n" + encoded + "\n" + path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qoderChatURL, bytes.NewBufferString(encoded))
	if err != nil {
		return nil, nil, err
	}
	headers := map[string]string{"cosy-data-policy": "AGREE", "content-type": "application/json", "cosy-machinetype": tokens.MachineType, "cosy-clienttype": "5", "cosy-date": date, "cosy-user": tokens.UserID, "cosy-key": cosyKey, "cache-control": "no-cache", "accept": "text/event-stream", "cosy-clientip": "169.254.198.161", "authorization": "Bearer COSY." + payloadB64 + "." + signature, "accept-encoding": "identity", "cosy-version": qoderCosyVersion, "cosy-machineid": tokens.MachineID, "cosy-machinetoken": tokens.MachineToken, "login-version": "v2", "user-agent": "Go-http-client/2.0"}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	return resp, []byte(encoded), err
}

type qoderCollected struct {
	content, reasoning string
	tools              []apicompat.ChatToolCall
	usage              qoder.Usage
	firstTokenMs       *int
	finish             string
	visible            bool
}

func (s *QoderGatewayService) consume(ctx context.Context, c *gin.Context, resp *http.Response, request *apicompat.ChatCompletionsRequest, actual string, requestBody []byte, output providerChatOutputProtocol, responses *providerChatResponsesBridge) (*ForwardResult, error) {
	defer resp.Body.Close()
	start := time.Now()
	collected := &qoderCollected{}
	id := "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	created := time.Now().Unix()
	pending := make([]qoder.Event, 0, 8)
	committed := false
	anthropicState := apicompat.NewChatCompletionsToAnthropicStreamState(request.Model)
	responsesState := apicompat.NewChatCompletionsToResponsesStreamState(request.Model)
	if responses != nil {
		responsesState.CustomTools = responses.customTools
		responsesState.FunctionTools = responses.functionTools
		responsesState.ToolSearchDeclared = responses.toolSearch
		responsesState.NamespaceTools = responses.namespaceTools
	}
	emit := func(event qoder.Event) error {
		if event.Usage.InputTokens > 0 || event.Usage.OutputTokens > 0 {
			collected.usage = event.Usage
		}
		event.Delta.ToolCalls = normalizeProviderToolCalls(event.Delta.ToolCalls, &collected.tools)
		if event.Delta.Content != nil && *event.Delta.Content != "" {
			collected.content += *event.Delta.Content
			collected.visible = true
		}
		if event.Delta.ReasoningContent != nil {
			collected.reasoning += *event.Delta.ReasoningContent
		}
		if len(event.Delta.ToolCalls) > 0 {
			collected.visible = true
		}
		if event.FinishReason != "" {
			collected.finish = event.FinishReason
			if event.FinishReason == "length" {
				collected.visible = true
			}
		}
		if collected.firstTokenMs == nil && (event.Delta.Content != nil || event.Delta.ReasoningContent != nil || len(event.Delta.ToolCalls) > 0) {
			v := int(time.Since(start).Milliseconds())
			collected.firstTokenMs = &v
		}
		if !request.Stream {
			return nil
		}
		// A single terminal chunk is emitted after the decoder completes so usage
		// is attached once and clients never observe duplicate finish reasons.
		event.FinishReason = ""
		pending = append(pending, event)
		if !committed && !collected.visible {
			return nil
		}
		if !committed {
			committed = true
			for _, item := range pending {
				if err := writeProviderChatEvent(c, id, created, request.Model, qoderProviderChatEvent(item), output, anthropicState, responsesState); err != nil {
					return err
				}
			}
			pending = nil
			return nil
		}
		return writeProviderChatEvent(c, id, created, request.Model, qoderProviderChatEvent(event), output, anthropicState, responsesState)
	}
	err := qoder.DecodeStream(resp.Body, emit)
	if err != nil {
		if c.Writer.Written() {
			return nil, err
		}
		var streamErr *qoder.StreamError
		if errors.As(err, &streamErr) {
			return nil, classifyQoderHTTPFailure(streamErr.Status, http.Header{})
		}
		return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Qoder upstream stream failed"}
	}
	if !collected.visible {
		return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Qoder upstream returned an empty stream"}
	}
	if collected.finish == "" {
		collected.finish = "stop"
	}
	if len(collected.tools) > 0 {
		collected.finish = "tool_calls"
	}
	usage := qoderUsage(collected.usage, requestBody, qoderCollectedOutputLength(collected))
	if request.Stream {
		finalUsage := qoder.Usage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens}
		if output == providerChatOutputResponses && responsesState != nil {
			responsesState.Usage = apicompat.ChatUsageToResponsesUsage(usage)
		}
		final := qoder.Event{FinishReason: collected.finish, Usage: finalUsage}
		if err := writeProviderChatEvent(c, id, created, request.Model, qoderProviderChatEvent(final), output, anthropicState, responsesState); err != nil {
			return nil, err
		}
		switch output {
		case providerChatOutputAnthropic:
			for _, event := range apicompat.FinalizeChatCompletionsAnthropicStream(anthropicState) {
				if err := writeProviderChatAnthropicWire(c, event); err != nil {
					return nil, err
				}
			}
		case providerChatOutputResponses:
			if err := responsesState.ValidateToolCallArguments(); err != nil {
				return nil, fmt.Errorf("invalid Qoder tool call arguments: %w", err)
			}
			for _, event := range apicompat.FinalizeChatCompletionsResponsesStream(responsesState) {
				if err := writeProviderChatResponsesWire(c, event); err != nil {
					return nil, err
				}
			}
			fmt.Fprint(c.Writer, "data: [DONE]\n\n")
		default:
			fmt.Fprint(c.Writer, "data: [DONE]\n\n")
		}
		if f, ok := c.Writer.(http.Flusher); ok {
			f.Flush()
		}
	} else {
		message := apicompat.ChatMessage{Role: "assistant", Content: json.RawMessage(providerJSONString(collected.content)), ReasoningContent: collected.reasoning, ToolCalls: collected.tools}
		c.JSON(http.StatusOK, apicompat.ChatCompletionsResponse{ID: id, Object: "chat.completion", Created: created, Model: request.Model, Choices: []apicompat.ChatChoice{{Index: 0, Message: message, FinishReason: collected.finish}}, Usage: usage})
	}
	return &ForwardResult{RequestID: resp.Header.Get("x-request-id"), UpstreamHeaders: resp.Header.Clone(), Usage: ClaudeUsage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens}, Model: request.Model, UpstreamModel: actual, Stream: request.Stream, Duration: time.Since(start), FirstTokenMs: collected.firstTokenMs}, nil
}
func qoderUsage(usage qoder.Usage, request []byte, outputLen int) *apicompat.ChatUsage {
	if usage.InputTokens <= 0 {
		usage.InputTokens = max(1, len(request)/4)
	}
	if usage.OutputTokens <= 0 {
		usage.OutputTokens = max(1, outputLen/4)
	}
	return &apicompat.ChatUsage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.InputTokens + usage.OutputTokens}
}
func qoderCollectedOutputLength(collected *qoderCollected) int {
	if collected == nil {
		return 0
	}
	total := len(collected.content) + len(collected.reasoning)
	for _, call := range collected.tools {
		total += len(call.Function.Name) + len(call.Function.Arguments)
	}
	return total
}
func classifyQoderHTTPFailure(status int, headers http.Header) *UpstreamFailoverError {
	failure := &UpstreamFailoverError{StatusCode: status, ResponseHeaders: qoderSafeResponseHeaders(headers), Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Qoder upstream request failed"}
	switch status {
	case http.StatusUnauthorized:
		failure.Stage = GatewayFailureStageAccountAuth
		failure.Reason = GatewayFailureReason("credential_rejected")
		failure.ClientStatusCode = http.StatusUnauthorized
		failure.ClientMessage = "Qoder credential was rejected"
	case http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		failure.ClientStatusCode = http.StatusTooManyRequests
		failure.ClientMessage = "Qoder quota or rate limit reached"
		if resetAt := parseRetryAfterResetTime(headers, time.Now()); resetAt != nil {
			failure.SameAccountRetryDeadline = *resetAt
		}
	case http.StatusBadRequest, http.StatusNotFound:
		failure.Scope = GatewayFailureScopeRequest
		failure.NextAccountAction = NextAccountStop
		failure.ClientStatusCode = http.StatusBadRequest
		failure.ClientMessage = "Qoder does not support the requested model or payload"
	}
	return failure
}

func qoderSafeResponseHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	safe := make(http.Header)
	if retryAfter := strings.TrimSpace(headers.Get("Retry-After")); retryAfter != "" {
		safe.Set("Retry-After", retryAfter)
	}
	return safe
}

const QoderCredentialUnavailableReason GatewayFailureReason = "qoder_credential_unavailable"
const QoderCredentialUnavailableClientMessage = "No healthy Qoder account is currently available"

func qoderCredentialFailover(status int) error {
	return &UpstreamFailoverError{StatusCode: status, Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, Reason: QoderCredentialUnavailableReason, ClientStatusCode: http.StatusServiceUnavailable, ClientMessage: QoderCredentialUnavailableClientMessage}
}
func qoderTokenFailure(err error) error {
	var exchange *qoderTokenExchangeError
	if errors.As(err, &exchange) {
		return classifyQoderHTTPFailure(exchange.status, exchange.header)
	}
	return qoderCredentialFailover(http.StatusUnauthorized)
}
func writeQoderChatError(c *gin.Context, status int, typ, message string) error {
	c.JSON(status, gin.H{"error": gin.H{"type": typ, "message": message}})
	return errors.New(message)
}
func writeQoderAnthropicError(c *gin.Context, status int, typ, message string) error {
	c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": typ, "message": message}})
	return errors.New(message)
}
func writeQoderRequestError(c *gin.Context, output providerChatOutputProtocol, status int, typ, message string) error {
	switch output {
	case providerChatOutputAnthropic:
		return writeQoderAnthropicError(c, status, typ, message)
	case providerChatOutputResponses:
		writeResponsesError(c, status, typ, message)
		return errors.New(message)
	default:
		return writeQoderChatError(c, status, typ, message)
	}
}
