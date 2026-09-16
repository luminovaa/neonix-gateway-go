package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/kiro"
)

const (
	kiroIDEVersion                  = "0.12.155"
	kiroAWSSDKVersion               = "1.0.34"
	kiroStreamingAPIVer             = "1.0.34"
	kiroDefaultCodeWhispererModelID = KiroDefaultModelID
)

type KiroGatewayService struct {
	tokenProvider *KiroTokenProvider
	httpUpstream  HTTPUpstream
}

func NewKiroGatewayService(tokenProvider *KiroTokenProvider, httpUpstream HTTPUpstream) *KiroGatewayService {
	return &KiroGatewayService{tokenProvider: tokenProvider, httpUpstream: httpUpstream}
}

type kiroEndpoint struct {
	name, url, origin string
	cli               bool
}

// ForwardAsMessages converts Anthropic Messages input through the shared
// compatibility graph and returns Anthropic wire output. Kiro remains the only
// upstream call; this method does not proxy through another Neonix service.
func (s *KiroGatewayService) ForwardAsMessages(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	var anthropicRequest apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropicRequest); err != nil {
		return nil, writeKiroAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	chatRequest, err := apicompat.AnthropicToChatCompletionsRequest(&anthropicRequest)
	if err != nil {
		return nil, writeKiroAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	if anthropicRequest.Stream {
		return s.forwardAnthropicStreaming(ctx, c, account, chatRequest, kiroThinkingFields(anthropicRequest.Thinking))
	}
	chatRequest.Stream = false
	recorder := httptest.NewRecorder()
	compat, _ := gin.CreateTestContext(recorder)
	compat.Request = c.Request.Clone(ctx)
	result, err := s.forwardChatRequest(ctx, compat, account, chatRequest, kiroThinkingFields(anthropicRequest.Thinking))
	if err != nil {
		return nil, err
	}
	var chatResponse apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &chatResponse); err != nil {
		return nil, errors.New("Kiro returned an invalid compatibility response")
	}
	anthropicResponse := apicompat.ChatCompletionsResponseToAnthropic(&chatResponse, anthropicRequest.Model)
	c.JSON(http.StatusOK, anthropicResponse)
	result.Stream = false
	return result, nil
}

// ForwardAsResponses bridges the [OI] Responses API to Kiro's native event
// stream. Conversion stays inside the Go data plane and preserves the
// failover-before-first-output boundary used by the other native providers.
func (s *KiroGatewayService) ForwardAsResponses(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	var request apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, errors.New("invalid Kiro Responses request")
	}
	if strings.TrimSpace(request.Model) == "" {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, errors.New("model is required")
	}
	effectiveTools, err := apicompat.EffectiveResponsesTools(&request)
	if err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Invalid tools configuration")
		return nil, errors.New("invalid Kiro Responses tools")
	}
	chatRequest, err := apicompat.ResponsesToChatCompletionsRequest(&request)
	if err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to convert request")
		return nil, errors.New("invalid Kiro Responses input")
	}
	bridge := &providerChatResponsesBridge{
		customTools:    apicompat.CustomToolNames(effectiveTools),
		functionTools:  apicompat.FunctionToolNames(effectiveTools),
		toolSearch:     apicompat.HasToolSearchTool(effectiveTools),
		namespaceTools: apicompat.NamespaceToolNames(effectiveTools),
	}
	if request.Stream {
		chatRequest.Stream = true
		return s.forwardChatRequestWithOutput(ctx, c, account, chatRequest, nil, providerChatOutputResponses, bridge)
	}

	chatRequest.Stream = false
	recorder := httptest.NewRecorder()
	compat, _ := gin.CreateTestContext(recorder)
	compat.Request = c.Request.Clone(ctx)
	result, err := s.forwardChatRequest(ctx, compat, account, chatRequest, nil)
	if err != nil {
		return nil, err
	}
	var chatResponse apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &chatResponse); err != nil {
		return nil, errors.New("Kiro returned an invalid compatibility response")
	}
	c.JSON(http.StatusOK, apicompat.ChatCompletionsResponseToResponses(
		&chatResponse, request.Model, bridge.customTools, bridge.functionTools, bridge.toolSearch, bridge.namespaceTools,
	))
	return result, nil
}

func writeKiroAnthropicError(c *gin.Context, status int, typ, message string) error {
	c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": typ, "message": message}})
	return errors.New(message)
}

func (s *KiroGatewayService) forwardAnthropicStreaming(ctx context.Context, c *gin.Context, account *Account, request *apicompat.ChatCompletionsRequest, additionalFields map[string]any) (*ForwardResult, error) {
	mappedModel := account.GetMappedModel(request.Model)
	payload, err := kiro.BuildChatPayload(request, mappedModel, kiroProfileARN(account), "AI_EDITOR")
	if err != nil {
		return nil, writeKiroAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	payload.AdditionalModelRequestFields = additionalFields
	token, err := s.tokenProvider.Token(ctx, account)
	if err != nil {
		return nil, kiroCredentialFailover(http.StatusUnauthorized)
	}
	start := time.Now()
	result, err := s.forwardAnthropicEndpoints(ctx, c, account, request, payload, token, start)
	if err == nil {
		return result, nil
	}
	var failover *UpstreamFailoverError
	if errors.As(err, &failover) && (failover.StatusCode == http.StatusUnauthorized || failover.StatusCode == http.StatusForbidden) && kiroCredential(account, "refreshToken", "refresh_token") != "" && !c.Writer.Written() {
		refreshed, refreshErr := s.tokenProvider.Refresh(ctx, account)
		if refreshErr == nil {
			return s.forwardAnthropicEndpoints(ctx, c, account, request, payload, refreshed, start)
		}
	}
	return nil, err
}

func (s *KiroGatewayService) forwardAnthropicEndpoints(ctx context.Context, c *gin.Context, account *Account, request *apicompat.ChatCompletionsRequest, payload *kiro.Payload, token string, start time.Time) (*ForwardResult, error) {
	var lastErr error
	for _, endpoint := range kiroEndpoints(account) {
		copyPayload := prepareKiroEndpointPayload(payload, endpoint)
		resp, requestBytes, callErr := s.callEndpointWithThinkingFallback(ctx, account, endpoint, copyPayload, token)
		if callErr != nil {
			lastErr = &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, ClientMessage: "Kiro upstream request failed"}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			failure := classifyKiroHTTPFailure(resp)
			failure.ResponseBody, _ = io.ReadAll(io.LimitReader(resp.Body, 512<<10))
			_ = resp.Body.Close()
			lastErr = failure
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return nil, failure
			}
			continue
		}
		result, streamErr := s.consumeAnthropicStream(ctx, c, resp, request.Model, requestBytes, start)
		if streamErr == nil || c.Writer.Written() {
			return result, streamErr
		}
		lastErr = streamErr
	}
	if lastErr == nil {
		lastErr = errors.New("no Kiro endpoint is configured")
	}
	return nil, lastErr
}

func (s *KiroGatewayService) consumeAnthropicStream(ctx context.Context, c *gin.Context, resp *http.Response, model string, requestBytes []byte, start time.Time) (*ForwardResult, error) {
	defer resp.Body.Close()
	decoder := kiro.NewEventStreamDecoder(resp.Body)
	parser := &kiro.StreamParser{}
	state := apicompat.NewChatCompletionsToAnthropicStreamState(model)
	id := "chatcmpl-" + strings.ReplaceAll(newKiroRequestID(), "-", "")
	created := time.Now().Unix()
	toolIndex := 0
	var firstTokenMs *int
	sawSemantic := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		typ, payload, err := decoder.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, ClientMessage: "Kiro upstream stream failed"}
		}
		events, err := parser.Parse(typ, payload)
		if err != nil {
			return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Kiro upstream stream failed"}
		}
		for _, event := range events {
			if event.Type == "usage" {
				continue
			}
			if !kiroSemanticEvent(event) {
				continue
			}
			sawSemantic = true
			if firstTokenMs == nil {
				value := int(time.Since(start).Milliseconds())
				firstTokenMs = &value
			}
			if event.Type == "content" {
				parser.ObserveText(event.Content)
			}
			chunk := kiroChatChunk(id, created, model, event, &toolIndex)
			if chunk == nil {
				continue
			}
			if err := writeKiroAnthropicEvents(c, apicompat.ChatCompletionsChunkToAnthropicEvents(chunk, state)); err != nil {
				return nil, err
			}
		}
	}
	for _, event := range parser.Finish() {
		if !kiroSemanticEvent(event) {
			continue
		}
		sawSemantic = true
		if chunk := kiroChatChunk(id, created, model, event, &toolIndex); chunk != nil {
			if err := writeKiroAnthropicEvents(c, apicompat.ChatCompletionsChunkToAnthropicEvents(chunk, state)); err != nil {
				return nil, err
			}
		}
	}
	if !sawSemantic {
		return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Kiro upstream returned an empty stream"}
	}
	usage := parser.Usage()
	if usage.InputTokens == 0 {
		usage.InputTokens = kiroMaxInt(1, len(requestBytes)/3)
	}
	finish := "stop"
	if toolIndex > 0 {
		finish = "tool_calls"
	}
	final := &apicompat.ChatCompletionsChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: apicompat.ChatDelta{}, FinishReason: &finish}}, Usage: kiroChatUsage(usage)}
	events := apicompat.ChatCompletionsChunkToAnthropicEvents(final, state)
	events = append(events, apicompat.FinalizeChatCompletionsAnthropicStream(state)...)
	if err := writeKiroAnthropicEvents(c, events); err != nil {
		return nil, err
	}
	return &ForwardResult{RequestID: resp.Header.Get("x-amzn-requestid"), UpstreamHeaders: resp.Header.Clone(), Usage: ClaudeUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CacheReadInputTokens: usage.CacheReadTokens, CacheCreationInputTokens: usage.CacheWriteTokens}, Model: model, UpstreamModel: payloadModel(requestBytes), Stream: true, Duration: time.Since(start), FirstTokenMs: firstTokenMs}, nil
}

func writeKiroAnthropicEvents(c *gin.Context, events []apicompat.AnthropicStreamEvent) error {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	for _, event := range events {
		body, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event.Type, body); err != nil {
			return err
		}
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return nil
}

func (s *KiroGatewayService) ForwardAsChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	if account == nil || account.Platform != PlatformKiro {
		return nil, errors.New("invalid Kiro account")
	}
	if s == nil || s.httpUpstream == nil || s.tokenProvider == nil {
		return nil, errors.New("Kiro gateway service is not configured")
	}
	var request apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, writeKiroChatError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	if strings.TrimSpace(request.Model) == "" {
		return nil, writeKiroChatError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	return s.forwardChatRequest(ctx, c, account, &request, nil)
}

func (s *KiroGatewayService) forwardChatRequest(ctx context.Context, c *gin.Context, account *Account, request *apicompat.ChatCompletionsRequest, additionalFields map[string]any) (*ForwardResult, error) {
	return s.forwardChatRequestWithOutput(ctx, c, account, request, additionalFields, providerChatOutputChat, nil)
}

func (s *KiroGatewayService) forwardChatRequestWithOutput(ctx context.Context, c *gin.Context, account *Account, request *apicompat.ChatCompletionsRequest, additionalFields map[string]any, output providerChatOutputProtocol, responses *providerChatResponsesBridge) (*ForwardResult, error) {
	if account == nil || account.Platform != PlatformKiro {
		return nil, errors.New("invalid Kiro account")
	}
	if s == nil || s.httpUpstream == nil || s.tokenProvider == nil {
		return nil, errors.New("Kiro gateway service is not configured")
	}
	if request == nil || strings.TrimSpace(request.Model) == "" {
		return nil, writeKiroChatError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	mappedModel := account.GetMappedModel(request.Model)
	profileARN := kiroProfileARN(account)
	payload, err := kiro.BuildChatPayload(request, mappedModel, profileARN, "AI_EDITOR")
	if err != nil {
		return nil, writeKiroChatError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	payload.AdditionalModelRequestFields = additionalFields
	token, err := s.tokenProvider.Token(ctx, account)
	if err != nil {
		return nil, kiroCredentialFailover(http.StatusUnauthorized)
	}
	start := time.Now()
	result, err := s.forwardEndpoints(ctx, c, account, request, payload, token, start, output, responses)
	if err == nil {
		return result, nil
	}
	var failover *UpstreamFailoverError
	if errors.As(err, &failover) && (failover.StatusCode == http.StatusUnauthorized || failover.StatusCode == http.StatusForbidden) && kiroCredential(account, "refreshToken", "refresh_token") != "" && !c.Writer.Written() {
		refreshed, refreshErr := s.tokenProvider.Refresh(ctx, account)
		if refreshErr == nil {
			return s.forwardEndpoints(ctx, c, account, request, payload, refreshed, start, output, responses)
		}
	}
	return nil, err
}

func (s *KiroGatewayService) forwardEndpoints(ctx context.Context, c *gin.Context, account *Account, request *apicompat.ChatCompletionsRequest, payload *kiro.Payload, token string, start time.Time, output providerChatOutputProtocol, responses *providerChatResponsesBridge) (*ForwardResult, error) {
	endpoints := kiroEndpoints(account)
	var lastErr error
	for _, endpoint := range endpoints {
		copyPayload := prepareKiroEndpointPayload(payload, endpoint)
		resp, requestBytes, err := s.callEndpointWithThinkingFallback(ctx, account, endpoint, copyPayload, token)
		if err != nil {
			lastErr = &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Kiro upstream request failed"}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			failure := classifyKiroHTTPFailure(resp)
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
			failure.ResponseBody = body
			_ = resp.Body.Close()
			lastErr = failure
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return nil, failure
			}
			continue
		}
		result, streamErr := s.consumeResponse(ctx, c, resp, request, requestBytes, start, output, responses)
		if streamErr == nil || c.Writer.Written() {
			return result, streamErr
		}
		lastErr = streamErr
	}
	if lastErr == nil {
		lastErr = errors.New("no Kiro endpoint is configured")
	}
	return nil, lastErr
}

func cloneKiroUserMessage(message *kiro.UserInputMessage) *kiro.UserInputMessage {
	if message == nil {
		return &kiro.UserInputMessage{}
	}
	copyMessage := *message
	return &copyMessage
}

func prepareKiroEndpointPayload(payload *kiro.Payload, endpoint kiroEndpoint) *kiro.Payload {
	copyPayload := *payload
	copyPayload.ConversationState = payload.ConversationState
	copyPayload.ConversationState.CurrentMessage = cloneKiroHistoryMessage(payload.ConversationState.CurrentMessage)
	copyPayload.ConversationState.History = make([]kiro.HistoryMessage, len(payload.ConversationState.History))
	for i, message := range payload.ConversationState.History {
		copyPayload.ConversationState.History[i] = cloneKiroHistoryMessage(message)
	}
	modelID := ""
	if current := copyPayload.ConversationState.CurrentMessage.UserInputMessage; current != nil {
		modelID = kiroEndpointModelID(current.ModelID, endpoint)
	}
	applyKiroEndpointFields(&copyPayload.ConversationState.CurrentMessage, endpoint.origin, modelID)
	for i := range copyPayload.ConversationState.History {
		applyKiroEndpointFields(&copyPayload.ConversationState.History[i], endpoint.origin, modelID)
	}
	if endpoint.cli {
		copyPayload.ConversationState.AgentContinuationID = ""
		copyPayload.ConversationState.AgentTaskType = ""
	}
	return &copyPayload
}

func cloneKiroHistoryMessage(message kiro.HistoryMessage) kiro.HistoryMessage {
	copyMessage := message
	copyMessage.UserInputMessage = cloneKiroUserMessage(message.UserInputMessage)
	return copyMessage
}

func applyKiroEndpointFields(message *kiro.HistoryMessage, origin, modelID string) {
	if message == nil || message.UserInputMessage == nil {
		return
	}
	message.UserInputMessage.Origin = origin
	if modelID != "" {
		message.UserInputMessage.ModelID = modelID
	}
}

func kiroEndpointModelID(modelID string, endpoint kiroEndpoint) string {
	modelID = strings.TrimSpace(strings.TrimPrefix(modelID, "ko/"))
	if endpoint.name != "codewhisperer" || isKiroNativeModelID(modelID) {
		return modelID
	}
	return kiroDefaultCodeWhispererModelID
}

func isKiroNativeModelID(modelID string) bool {
	if modelID == "" || !strings.Contains(modelID, "_") {
		return false
	}
	for _, char := range modelID {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func (s *KiroGatewayService) callEndpoint(ctx context.Context, account *Account, endpoint kiroEndpoint, payload *kiro.Payload, token string) (*http.Response, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	machineID := stableKiroMachineID(account)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	req.Header.Set("X-Amzn-Kiro-Agent-Mode", kiroAgentMode(account))
	req.Header.Set("X-Amz-User-Agent", kiroAmzUserAgent(machineID))
	req.Header.Set("User-Agent", kiroUserAgent(machineID))
	req.Header.Set("Amz-Sdk-Invocation-Id", newKiroRequestID())
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	return resp, body, err
}

func (s *KiroGatewayService) callEndpointWithThinkingFallback(ctx context.Context, account *Account, endpoint kiroEndpoint, payload *kiro.Payload, token string) (*http.Response, []byte, error) {
	resp, requestBytes, err := s.callEndpoint(ctx, account, endpoint, payload, token)
	if err != nil || resp == nil || resp.StatusCode != http.StatusBadRequest || len(payload.AdditionalModelRequestFields) == 0 {
		return resp, requestBytes, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, requestBytes, readErr
	}
	if !kiroUnsupportedAdditionalFields(body) {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, requestBytes, nil
	}
	withoutThinking := *payload
	withoutThinking.AdditionalModelRequestFields = nil
	return s.callEndpoint(ctx, account, endpoint, &withoutThinking, token)
}

func kiroUnsupportedAdditionalFields(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "additionalmodelrequestfields") &&
		(strings.Contains(lower, "not supported") || strings.Contains(lower, "request_body_invalid"))
}

func kiroThinkingFields(thinking *apicompat.AnthropicThinking) map[string]any {
	if thinking == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "enabled":
		if thinking.BudgetTokens <= 0 {
			return nil
		}
		return map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": thinking.BudgetTokens}}
	case "adaptive":
		return map[string]any{"thinking": map[string]any{"type": "adaptive"}}
	default:
		return nil
	}
}

type kiroCollected struct {
	content, reasoning, signature string
	tools                         []kiro.ToolUse
	usage                         kiro.Usage
	firstTokenMs                  *int
}

func (s *KiroGatewayService) consumeResponse(ctx context.Context, c *gin.Context, resp *http.Response, request *apicompat.ChatCompletionsRequest, requestBytes []byte, start time.Time, output providerChatOutputProtocol, responses *providerChatResponsesBridge) (*ForwardResult, error) {
	defer resp.Body.Close()
	decoder := kiro.NewEventStreamDecoder(resp.Body)
	parser := &kiro.StreamParser{}
	collected := &kiroCollected{}
	id := "chatcmpl-" + strings.ReplaceAll(newKiroRequestID(), "-", "")
	created := time.Now().Unix()
	toolIndex := 0
	started := false
	sawSemantic := false
	var pendingEvents []kiro.Event
	var responsesState *apicompat.ChatCompletionsToResponsesStreamState
	if output == providerChatOutputResponses {
		responsesState = apicompat.NewChatCompletionsToResponsesStreamState(request.Model)
		if responses != nil {
			responsesState.CustomTools = responses.customTools
			responsesState.FunctionTools = responses.functionTools
			responsesState.ToolSearchDeclared = responses.toolSearch
			responsesState.NamespaceTools = responses.namespaceTools
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		eventType, payload, err := decoder.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if c.Writer.Written() {
				return nil, fmt.Errorf("Kiro stream failed after output started: %w", err)
			}
			return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, ClientMessage: "Kiro upstream stream failed"}
		}
		events, err := parser.Parse(eventType, payload)
		if err != nil {
			if c.Writer.Written() {
				return nil, err
			}
			return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, ClientMessage: "Kiro upstream stream failed"}
		}
		for _, event := range events {
			if event.Type == "usage" {
				if event.Usage != nil {
					collected.usage = *event.Usage
				}
				continue
			}
			if !kiroSemanticEvent(event) {
				continue
			}
			sawSemantic = true
			if collected.firstTokenMs == nil {
				value := int(time.Since(start).Milliseconds())
				collected.firstTokenMs = &value
			}
			if !started {
				pendingEvents = append(pendingEvents, event)
				continue
			}
			applyKiroCollectedEvent(collected, parser, event)
			if request.Stream {
				if err := writeKiroOutputEvent(c, id, created, request.Model, event, &toolIndex, output, responsesState); err != nil {
					return nil, err
				}
			}
		}
		if !started && len(pendingEvents) > 0 && request.Stream {
			started = true
			if err := writeKiroOutputRole(c, id, created, request.Model, output, responsesState); err != nil {
				return nil, err
			}
			for _, event := range pendingEvents {
				applyKiroCollectedEvent(collected, parser, event)
				if request.Stream {
					if err := writeKiroOutputEvent(c, id, created, request.Model, event, &toolIndex, output, responsesState); err != nil {
						return nil, err
					}
				}
			}
			pendingEvents = nil
		}
	}
	if !started && len(pendingEvents) > 0 {
		for _, event := range pendingEvents {
			applyKiroCollectedEvent(collected, parser, event)
		}
	}
	for _, event := range parser.Finish() {
		if !kiroSemanticEvent(event) {
			continue
		}
		sawSemantic = true
		if event.ToolUse != nil {
			collected.tools = append(collected.tools, *event.ToolUse)
		}
		if request.Stream {
			if err := writeKiroOutputEvent(c, id, created, request.Model, event, &toolIndex, output, responsesState); err != nil {
				return nil, err
			}
		}
	}
	if !sawSemantic {
		return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Kiro upstream returned an empty stream"}
	}
	collected.usage = parser.Usage()
	if collected.usage.InputTokens == 0 {
		collected.usage.InputTokens = kiroMaxInt(1, len(requestBytes)/3)
	}
	if request.Stream {
		finish := "stop"
		if len(collected.tools) > 0 {
			finish = "tool_calls"
		}
		if err := writeKiroOutputFinal(c, id, created, request.Model, finish, collected.usage, request.StreamOptions != nil && request.StreamOptions.IncludeUsage, output, responsesState); err != nil {
			return nil, err
		}
	} else {
		if err := writeKiroNonStream(c, id, created, request.Model, collected); err != nil {
			return nil, err
		}
	}
	return &ForwardResult{RequestID: resp.Header.Get("x-amzn-requestid"), UpstreamHeaders: resp.Header.Clone(), Usage: ClaudeUsage{InputTokens: collected.usage.InputTokens, OutputTokens: collected.usage.OutputTokens, CacheReadInputTokens: collected.usage.CacheReadTokens, CacheCreationInputTokens: collected.usage.CacheWriteTokens}, Model: request.Model, UpstreamModel: payloadModel(requestBytes), Stream: request.Stream, Duration: time.Since(start), FirstTokenMs: collected.firstTokenMs}, nil
}

func applyKiroCollectedEvent(collected *kiroCollected, parser *kiro.StreamParser, event kiro.Event) {
	switch event.Type {
	case "content":
		collected.content += event.Content
		parser.ObserveText(event.Content)
	case "reasoning":
		collected.reasoning += event.Reasoning
		if event.Signature != "" {
			collected.signature = event.Signature
		}
	case "tool":
		if event.ToolUse != nil {
			collected.tools = append(collected.tools, *event.ToolUse)
		}
	}
}

func kiroSemanticEvent(event kiro.Event) bool {
	switch event.Type {
	case "content":
		return event.Content != ""
	case "reasoning":
		return event.Reasoning != "" || event.Signature != ""
	case "tool":
		return event.ToolUse != nil
	default:
		return false
	}
}

func writeKiroStreamEvent(c *gin.Context, id string, created int64, model string, event kiro.Event, toolIndex *int) error {
	chunk := kiroChatChunk(id, created, model, event, toolIndex)
	if chunk == nil {
		return nil
	}
	return writeKiroSSE(c, chunk)
}

func writeKiroOutputRole(c *gin.Context, id string, created int64, model string, output providerChatOutputProtocol, responsesState *apicompat.ChatCompletionsToResponsesStreamState) error {
	if output != providerChatOutputResponses {
		return writeKiroStreamRole(c, id, created, model)
	}
	return writeProviderChatEvent(c, id, created, model, providerChatEvent{
		Delta: apicompat.ChatDelta{Role: "assistant"},
	}, output, nil, responsesState)
}

func writeKiroOutputEvent(c *gin.Context, id string, created int64, model string, event kiro.Event, toolIndex *int, output providerChatOutputProtocol, responsesState *apicompat.ChatCompletionsToResponsesStreamState) error {
	if output != providerChatOutputResponses {
		return writeKiroStreamEvent(c, id, created, model, event, toolIndex)
	}
	chunk := kiroChatChunk(id, created, model, event, toolIndex)
	if chunk == nil || len(chunk.Choices) == 0 {
		return nil
	}
	return writeProviderChatEvent(c, id, created, model, providerChatEvent{
		Delta: chunk.Choices[0].Delta,
	}, output, nil, responsesState)
}

func writeKiroOutputFinal(c *gin.Context, id string, created int64, model, finish string, usage kiro.Usage, includeUsage bool, output providerChatOutputProtocol, responsesState *apicompat.ChatCompletionsToResponsesStreamState) error {
	if output != providerChatOutputResponses {
		return writeKiroFinalStream(c, id, created, model, finish, usage, includeUsage)
	}
	chatUsage := kiroChatUsage(usage)
	if responsesState == nil {
		return errors.New("Kiro Responses stream state is unavailable")
	}
	responsesState.Usage = apicompat.ChatUsageToResponsesUsage(chatUsage)
	if err := writeProviderChatEvent(c, id, created, model, providerChatEvent{
		FinishReason: finish,
		Usage: providerChatUsage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
		},
	}, output, nil, responsesState); err != nil {
		return err
	}
	if err := responsesState.ValidateToolCallArguments(); err != nil {
		return fmt.Errorf("invalid Kiro tool call arguments: %w", err)
	}
	for _, event := range apicompat.FinalizeChatCompletionsResponsesStream(responsesState) {
		if err := writeProviderChatResponsesWire(c, event); err != nil {
			return err
		}
	}
	_, err := c.Writer.Write([]byte("data: [DONE]\n\n"))
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return err
}

func kiroChatChunk(id string, created int64, model string, event kiro.Event, toolIndex *int) *apicompat.ChatCompletionsChunk {
	chunk := apicompat.ChatCompletionsChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: apicompat.ChatDelta{}}}}
	switch event.Type {
	case "content":
		value := event.Content
		chunk.Choices[0].Delta.Content = &value
	case "reasoning":
		value := event.Reasoning
		chunk.Choices[0].Delta.ReasoningContent = &value
	case "tool":
		if event.ToolUse == nil {
			return nil
		}
		arguments, _ := json.Marshal(event.ToolUse.Input)
		index := *toolIndex
		*toolIndex = *toolIndex + 1
		chunk.Choices[0].Delta.ToolCalls = []apicompat.ChatToolCall{{Index: &index, ID: event.ToolUse.ToolUseID, Type: "function", Function: apicompat.ChatFunctionCall{Name: event.ToolUse.Name, Arguments: string(arguments)}}}
	default:
		return nil
	}
	return &chunk
}

func writeKiroStreamRole(c *gin.Context, id string, created int64, model string) error {
	return writeKiroSSE(c, apicompat.ChatCompletionsChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: apicompat.ChatDelta{Role: "assistant"}}},
	})
}

func writeKiroFinalStream(c *gin.Context, id string, created int64, model, finish string, usage kiro.Usage, includeUsage bool) error {
	chunk := apicompat.ChatCompletionsChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: apicompat.ChatDelta{}, FinishReason: &finish}}}
	if err := writeKiroSSE(c, chunk); err != nil {
		return err
	}
	if includeUsage {
		usageChunk := apicompat.ChatCompletionsChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []apicompat.ChatChunkChoice{}, Usage: kiroChatUsage(usage)}
		if err := writeKiroSSE(c, usageChunk); err != nil {
			return err
		}
	}
	_, err := c.Writer.Write([]byte("data: [DONE]\n\n"))
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return err
}

func writeKiroSSE(c *gin.Context, value any) error {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
		return err
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func writeKiroNonStream(c *gin.Context, id string, created int64, model string, collected *kiroCollected) error {
	content, _ := json.Marshal(collected.content)
	if strings.TrimSpace(collected.content) == "" {
		content = json.RawMessage("null")
	}
	message := apicompat.ChatMessage{Role: "assistant", Content: content, ReasoningContent: collected.reasoning}
	for _, tool := range collected.tools {
		args, _ := json.Marshal(tool.Input)
		message.ToolCalls = append(message.ToolCalls, apicompat.ChatToolCall{ID: tool.ToolUseID, Type: "function", Function: apicompat.ChatFunctionCall{Name: tool.Name, Arguments: string(args)}})
	}
	finish := "stop"
	if len(message.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	c.JSON(http.StatusOK, apicompat.ChatCompletionsResponse{ID: id, Object: "chat.completion", Created: created, Model: model, Choices: []apicompat.ChatChoice{{Index: 0, Message: message, FinishReason: finish}}, Usage: kiroChatUsage(collected.usage)})
	return nil
}

func kiroChatUsage(usage kiro.Usage) *apicompat.ChatUsage {
	out := &apicompat.ChatUsage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.InputTokens + usage.OutputTokens}
	if usage.CacheReadTokens > 0 || usage.CacheWriteTokens > 0 {
		out.PromptTokensDetails = &apicompat.ChatTokenDetails{CachedTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens}
	}
	if usage.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &apicompat.ChatTokenDetails{ReasoningTokens: usage.ReasoningTokens}
	}
	return out
}

func classifyKiroHTTPFailure(resp *http.Response) *UpstreamFailoverError {
	failure := &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseHeaders: resp.Header.Clone(), Scope: GatewayFailureScopeAccount, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Kiro upstream rejected the request"}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		failure.Stage = GatewayFailureStageAccountAuth
		failure.Reason = GatewayFailureReason("credential_rejected")
		failure.ClientStatusCode = http.StatusUnauthorized
	case http.StatusBadRequest, http.StatusNotFound:
		failure.Scope = GatewayFailureScopeRequest
		failure.NextAccountAction = NextAccountStop
		failure.ClientStatusCode = http.StatusBadRequest
		failure.ClientMessage = "Kiro does not support the requested model or payload"
	case http.StatusTooManyRequests:
		failure.ClientStatusCode = http.StatusTooManyRequests
		failure.ClientMessage = "Kiro quota or rate limit reached"
	}
	return failure
}

const (
	KiroCredentialUnavailableReason        GatewayFailureReason = "kiro_credential_unavailable"
	KiroCredentialUnavailableClientMessage                      = "No healthy Kiro account is currently available"
)

func kiroCredentialFailover(status int) error {
	return &UpstreamFailoverError{StatusCode: status, Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeAccount, Reason: KiroCredentialUnavailableReason, ClientStatusCode: http.StatusServiceUnavailable, ClientMessage: KiroCredentialUnavailableClientMessage}
}
func writeKiroChatError(c *gin.Context, status int, typ, message string) error {
	c.JSON(status, gin.H{"error": gin.H{"type": typ, "message": message}})
	return errors.New(message)
}

func kiroEndpoints(account *Account) []kiroEndpoint {
	region := strings.ToLower(kiroCredential(account, "region"))
	qHost := "q.us-east-1.amazonaws.com"
	if strings.HasPrefix(region, "eu-") {
		qHost = "q.eu-central-1.amazonaws.com"
	}
	q := kiroEndpoint{name: "amazonq", url: "https://" + qHost + "/generateAssistantResponse", origin: "AI_EDITOR"}
	cw := kiroEndpoint{name: "codewhisperer", url: "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse", origin: "AI_EDITOR"}
	cli := kiroEndpoint{name: "amazonq-cli", url: "https://" + qHost + "/SendMessageStreaming", origin: "AmazonQ", cli: true}
	switch strings.ToLower(kiroCredential(account, "preferredEndpoint", "preferred_endpoint")) {
	case "codewhisperer":
		return []kiroEndpoint{cw, q}
	case "amazonq-cli":
		return []kiroEndpoint{cli}
	default:
		return []kiroEndpoint{q, cw}
	}
}

func kiroProfileARN(account *Account) string {
	if value := kiroCredential(account, "profileArn", "profile_arn"); value != "" {
		return value
	}
	provider := strings.ToLower(kiroCredential(account, "provider", "identityProvider", "identity_provider"))
	if provider == "github" || provider == "google" {
		return kiro.SocialProfileARN
	}
	return kiro.BuilderIDProfileARN
}
func stableKiroMachineID(account *Account) string {
	if value := kiroCredential(account, "machineId", "machine_id"); value != "" {
		return value
	}
	id := int64(0)
	if account != nil {
		id = account.ID
	}
	sum := sha256.Sum256([]byte("kiro-device-" + strconv.FormatInt(id, 10)))
	return hex.EncodeToString(sum[:])
}
func kiroAgentMode(account *Account) string {
	if strings.EqualFold(kiroCredential(account, "authMethod", "auth_method"), "idc") {
		return "vibe"
	}
	return "spec"
}
func kiroUserAgent(machineID string) string {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	return fmt.Sprintf("aws-sdk-js/%s ua/2.1 os/%s lang/js api/codewhispererstreaming#%s m/E KiroIDE-%s-%s", kiroAWSSDKVersion, osName, kiroStreamingAPIVer, kiroIDEVersion, machineID)
}
func kiroAmzUserAgent(machineID string) string {
	return fmt.Sprintf("aws-sdk-js/%s KiroIDE %s %s", kiroAWSSDKVersion, kiroIDEVersion, machineID)
}
func newKiroRequestID() string {
	return uuid.NewString()
}
func payloadModel(body []byte) string {
	var payload kiro.Payload
	if json.Unmarshal(body, &payload) == nil && payload.ConversationState.CurrentMessage.UserInputMessage != nil {
		return payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
	}
	return ""
}
func kiroMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
