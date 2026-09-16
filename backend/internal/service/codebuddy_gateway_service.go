package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/apicompat"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddy"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/workbuddy"
)

// CodeBuddyGatewayService owns the native CodeBuddy .ai data plane. Both the
// CLI device credential and the historical browser credential shape are kept
// here so migrated accounts do not need to be rewritten before cutover.
type CodeBuddyGatewayService struct {
	tokens       *CodeBuddyTokenProvider
	httpUpstream HTTPUpstream
	accountRepo  AccountRepository
}

func NewCodeBuddyGatewayService(tokens *CodeBuddyTokenProvider, upstream HTTPUpstream, repos ...AccountRepository) *CodeBuddyGatewayService {
	service := &CodeBuddyGatewayService{tokens: tokens, httpUpstream: upstream}
	if len(repos) > 0 {
		service.accountRepo = repos[0]
	}
	return service
}

func (s *CodeBuddyGatewayService) ForwardAsChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	var request apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, writeCodeBuddyChatError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
	}
	return s.forward(ctx, c, account, &request, providerChatOutputChat, nil)
}

func (s *CodeBuddyGatewayService) ForwardAsMessages(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
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
		return nil, errors.New("CodeBuddy returned an invalid compatibility response")
	}
	c.JSON(http.StatusOK, apicompat.ChatCompletionsResponseToAnthropic(&response, anthropic.Model))
	return result, nil
}

func (s *CodeBuddyGatewayService) ForwardAsResponses(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	var request apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, errors.New("invalid CodeBuddy Responses request")
	}
	if strings.TrimSpace(request.Model) == "" {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, errors.New("model is required")
	}
	effectiveTools, err := apicompat.EffectiveResponsesTools(&request)
	if err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Invalid tools configuration")
		return nil, errors.New("invalid CodeBuddy Responses tools")
	}
	chatRequest, err := apicompat.ResponsesToChatCompletionsRequest(&request)
	if err != nil {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", "Failed to convert request")
		return nil, errors.New("invalid CodeBuddy Responses input")
	}
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
		return nil, errors.New("CodeBuddy returned an invalid compatibility response")
	}
	c.JSON(http.StatusOK, apicompat.ChatCompletionsResponseToResponses(
		&chatResponse, request.Model, bridge.customTools, bridge.functionTools, bridge.toolSearch, bridge.namespaceTools,
	))
	return result, nil
}

func (s *CodeBuddyGatewayService) forward(ctx context.Context, c *gin.Context, account *Account, request *apicompat.ChatCompletionsRequest, output providerChatOutputProtocol, responses *providerChatResponsesBridge) (*ForwardResult, error) {
	if account == nil || !isCodeBuddyOAuthPlatform(account.Platform) {
		return nil, errors.New("invalid CodeBuddy or WorkBuddy account")
	}
	if s == nil || s.tokens == nil || s.httpUpstream == nil {
		return nil, errors.New("CodeBuddy gateway service is not configured")
	}
	if strings.TrimSpace(request.Model) == "" {
		return nil, writeCodeBuddyRequestError(c, output, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	if codebuddy.IsChinaRealm(codeBuddyIdentity(account).Domain) {
		return nil, codeBuddyUnsupportedRealmFailure()
	}
	if account.Platform == PlatformWorkBuddy && !workbuddy.IsGlobalDomain(codeBuddyIdentity(account).Domain) {
		return nil, errors.New("WorkBuddy account has an invalid realm")
	}
	if account.Platform == PlatformCodeBuddy && workbuddy.IsGlobalDomain(codeBuddyIdentity(account).Domain) {
		return nil, errors.New("WorkBuddy credentials require the workbuddy provider")
	}
	mappedModel, mappingMatched := account.ResolveMappedModel(request.Model)
	// The legacy registry maps cb/ to an empty string, but its adapter used a
	// JavaScript `mapped || requested` fallback and therefore sent cb/ upstream.
	// Preserve that behavior for migrated account snapshots.
	if mappingMatched && strings.TrimSpace(mappedModel) == "" {
		mappedModel = request.Model
	}
	resolvedModel := codebuddy.ResolveModel(mappedModel)
	payload, err := codebuddy.BuildPayload(request, mappedModel)
	if err != nil {
		return nil, writeCodeBuddyRequestError(c, output, http.StatusBadRequest, "invalid_request_error", "Failed to convert request")
	}
	if account.Platform == PlatformWorkBuddy {
		workbuddy.NormalizePayload(payload)
	}
	resp, requestBody, err := s.call(ctx, account, payload)
	if err != nil {
		var tokenErr *CodeBuddyTokenError
		if errors.As(err, &tokenErr) {
			return nil, codeBuddyTokenFailure(tokenErr)
		}
		return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: "CodeBuddy upstream request failed"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
		failure := classifyCodeBuddyHTTPFailure(resp.StatusCode, resp.Header)
		if account.Platform == PlatformWorkBuddy {
			failure = classifyWorkBuddyHTTPFailure(resp.StatusCode, resp.Header, body)
			// Code 6004 is an upstream-model window, not an account-wide quota.
			// Persist it at the same boundary that knows both account and requested
			// model so scheduler selection can continue using this account for other
			// WorkBuddy models.
			if failure.ModelCooldown && s.accountRepo != nil && !failure.SameAccountRetryDeadline.IsZero() {
				modelKey := strings.TrimSpace(resolvedModel.Upstream)
				if modelKey == "" {
					modelKey = strings.TrimSpace(mappedModel)
				}
				if modelKey != "" {
					stateCtx, cancel := openAIAccountStateContext(ctx)
					_ = s.accountRepo.SetModelRateLimit(stateCtx, account.ID, modelKey, failure.SameAccountRetryDeadline, "workbuddy_model_rate_limit")
					cancel()
				}
			}
		}
		resp.Body.Close()
		return nil, failure
	}
	providerName := "CodeBuddy"
	if account.Platform == PlatformWorkBuddy {
		providerName = "WorkBuddy"
	}
	return s.consume(c, resp, request, resolvedModel.Upstream, requestBody, output, responses, providerName)
}

var workBuddyResetTime = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2})`)

// classifyWorkBuddyHTTPFailure retains the generic public error contract while
// preserving the provider's explicit cooldown semantics for the scheduler.
func classifyWorkBuddyHTTPFailure(status int, headers http.Header, body []byte) *UpstreamFailoverError {
	failure := classifyCodeBuddyHTTPFailure(status, headers)
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(body, &envelope)
	switch envelope.Code {
	case 12153:
		failure.Stage = GatewayFailureStageAccountAuth
		failure.Reason = GatewayFailureReason("credential_rejected")
		failure.ClientStatusCode = http.StatusUnauthorized
		failure.ClientMessage = "WorkBuddy credential was rejected"
	case 14018:
		failure.Scope = GatewayFailureScopeAccount
		failure.NextAccountAction = NextAccountRetry
		failure.ClientStatusCode = http.StatusTooManyRequests
		failure.ClientMessage = "WorkBuddy quota is exhausted"
		location, _ := time.LoadLocation("Asia/Jakarta")
		now := time.Now().In(location)
		failure.SameAccountRetryDeadline = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, location)
	case 6004:
		failure.Scope = GatewayFailureScopeModel
		failure.NextAccountAction = NextAccountRetry
		failure.ModelCooldown = true
		failure.ClientStatusCode = http.StatusTooManyRequests
		failure.ClientMessage = "WorkBuddy model rate limit reached"
		if match := workBuddyResetTime.FindStringSubmatch(envelope.Msg); len(match) == 2 {
			utc8 := time.FixedZone("UTC+8", 8*60*60)
			if resetAt, err := time.ParseInLocation("2006-01-02 15:04:05", strings.Replace(match[1], "T", " ", 1), utc8); err == nil && resetAt.After(time.Now()) {
				failure.SameAccountRetryDeadline = resetAt
			}
		}
	case 11128, 11148, 11151:
		failure.Scope = GatewayFailureScopeRequest
		failure.NextAccountAction = NextAccountStop
		failure.ClientStatusCode = http.StatusBadRequest
		failure.ClientMessage = "WorkBuddy rejected the request payload"
	}
	return failure
}

func (s *CodeBuddyGatewayService) call(ctx context.Context, account *Account, payload any) (*http.Response, []byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	identity := codeBuddyIdentity(account)
	hosts := codebuddy.ResolveHosts(identity.Domain)
	var headers http.Header
	if identity.IsCLI() {
		accessToken, err := s.tokens.AccessToken(ctx, account)
		if err != nil {
			return nil, nil, err
		}
		identity.AccessToken = accessToken
		headers = codebuddy.CLIHeaders(identity)
	} else {
		headers = codeBuddyLegacyHeaders(account, hosts)
		if strings.TrimSpace(headers.Get("Authorization")) == "" && strings.TrimSpace(headers.Get("Cookie")) == "" {
			return nil, nil, &CodeBuddyTokenError{Status: http.StatusUnauthorized, Revoked: true}
		}
	}
	conversationID := uuid.NewString()
	headers.Set("X-Conversation-ID", conversationID)
	headers.Set("X-Conversation-Request-ID", strings.ReplaceAll(uuid.NewString(), "-", ""))
	headers.Set("X-Conversation-Message-ID", strings.ReplaceAll(uuid.NewString(), "-", ""))
	headers.Set("X-Request-ID", strings.ReplaceAll(uuid.NewString(), "-", ""))
	if account.Platform == PlatformWorkBuddy {
		// WorkBuddy Global validates the CLI channel identity more strictly than
		// CodeBuddy. Keep this profile adapter-local so a header update cannot
		// alter the CodeBuddy .ai wire contract.
		headers.Set("User-Agent", "CLI/2.139.0 CodeBuddy/2.139.0")
		headers.Set("X-Session-ID", strings.ReplaceAll(uuid.NewString(), "-", ""))
		headers.Set("X-Agent-Type", "main")
		headers.Set("X-Agent-Intent", "default")
		headers.Set("X-IDE-Type", "CLI")
		headers.Set("X-IDE-Name", "CLI")
		headers.Set("X-IDE-Version", "2.139.0")
		headers.Set("X-Product-Version", "2.139.0")
		headers.Set("X-Private-Data", "false")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codebuddy.ChatURL(hosts), bytes.NewReader(encoded))
	if err != nil {
		return nil, nil, err
	}
	req.Header = headers
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	return resp, encoded, err
}

func codeBuddyLegacyHeaders(account *Account, hosts codebuddy.Hosts) http.Header {
	headers := codebuddy.CommonHeaders(hosts)
	headers.Set("Accept", "text/event-stream, application/json, */*")
	headers.Set("X-Domain", strings.TrimPrefix(hosts.Chat, "https://"))
	headers.Set("X-Product", "SaaS")
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	if token := codeBuddyLegacyToken(account); token != "" {
		headers.Set("Authorization", "Bearer "+token)
		headers.Set("X-Api-Key", token)
	}
	if cookies := codeBuddyCookies(account); cookies != "" {
		headers.Set("Cookie", cookies)
	}
	if userAgent := codeBuddyCredential(account, "userAgent", "user_agent"); userAgent != "" {
		headers.Set("User-Agent", userAgent)
	}
	return headers
}

func codeBuddyCookies(account *Account) string {
	if value := codeBuddyCredential(account, "rawCookies", "raw_cookies", "cookies", "cookie"); value != "" {
		return value
	}
	if account == nil {
		return ""
	}
	for _, key := range []string{"rawCookies", "raw_cookies", "cookies"} {
		object, ok := account.Credentials[key].(map[string]any)
		if !ok || len(object) == 0 {
			continue
		}
		keys := make([]string, 0, len(object))
		for name := range object {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, name := range keys {
			parts = append(parts, name+"="+fmt.Sprint(object[name]))
		}
		return strings.Join(parts, "; ")
	}
	return ""
}

type codeBuddyCollected struct {
	content, reasoning string
	tools              []apicompat.ChatToolCall
	usage              providerChatUsage
	firstTokenMs       *int
	finish             string
	semantic           bool
}

func (s *CodeBuddyGatewayService) consume(c *gin.Context, resp *http.Response, request *apicompat.ChatCompletionsRequest, upstreamModel string, requestBody []byte, output providerChatOutputProtocol, responses *providerChatResponsesBridge, providerName string) (*ForwardResult, error) {
	return consumeCodeBuddyCompatibleStream(c, resp, request, upstreamModel, requestBody, output, responses, providerName)
}

func consumeCodeBuddyCompatibleStream(c *gin.Context, resp *http.Response, request *apicompat.ChatCompletionsRequest, upstreamModel string, requestBody []byte, output providerChatOutputProtocol, responses *providerChatResponsesBridge, providerName string) (*ForwardResult, error) {
	defer resp.Body.Close()
	start := time.Now()
	collected := &codeBuddyCollected{}
	id := "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	created := time.Now().Unix()
	pending := make([]providerChatEvent, 0, 8)
	committed := false
	anthropicState := apicompat.NewChatCompletionsToAnthropicStreamState(request.Model)
	responsesState := apicompat.NewChatCompletionsToResponsesStreamState(request.Model)
	if responses != nil {
		responsesState.CustomTools = responses.customTools
		responsesState.FunctionTools = responses.functionTools
		responsesState.ToolSearchDeclared = responses.toolSearch
		responsesState.NamespaceTools = responses.namespaceTools
	}
	emit := func(chunk apicompat.ChatCompletionsChunk) error {
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Usage != nil {
			collected.usage = providerChatUsage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		event := providerChatEvent{}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			event.Delta = choice.Delta
			if event.Delta.ReasoningContent == nil {
				event.Delta.ReasoningContent = event.Delta.Reasoning
			}
			event.Delta.Reasoning = nil
			event.Delta.ToolCalls = normalizeProviderToolCalls(event.Delta.ToolCalls, &collected.tools)
			if choice.FinishReason != nil {
				event.FinishReason = *choice.FinishReason
			}
		}
		event.Usage = collected.usage
		if event.Delta.Content != nil && *event.Delta.Content != "" {
			collected.content += *event.Delta.Content
			collected.semantic = true
		}
		if event.Delta.ReasoningContent != nil && *event.Delta.ReasoningContent != "" {
			collected.reasoning += *event.Delta.ReasoningContent
			collected.semantic = true
		}
		if len(event.Delta.ToolCalls) > 0 {
			collected.semantic = true
		}
		if event.FinishReason != "" {
			collected.finish = event.FinishReason
		}
		if collected.firstTokenMs == nil && (event.Delta.Content != nil || event.Delta.ReasoningContent != nil || len(event.Delta.ToolCalls) > 0) {
			value := int(time.Since(start).Milliseconds())
			collected.firstTokenMs = &value
		}
		// The WorkBuddy plugin occasionally sends heartbeat-like empty deltas
		// between semantic chunks. They are not OpenAI content and must not
		// enter the pending buffer before the stream is committed either.
		if providerName == "WorkBuddy" && workBuddyEmptyDelta(event) {
			return nil
		}
		if !request.Stream {
			return nil
		}
		// Emit one terminal event after decoding so usage and finish reason are
		// stable across chat, Messages, and Responses output protocols.
		event.FinishReason = ""
		pending = append(pending, event)
		if !committed && !collected.semantic {
			return nil
		}
		if !committed {
			committed = true
			for _, item := range pending {
				if err := writeProviderChatEvent(c, id, created, request.Model, item, output, anthropicState, responsesState); err != nil {
					return err
				}
			}
			pending = nil
			return nil
		}
		return writeProviderChatEvent(c, id, created, request.Model, event, output, anthropicState, responsesState)
	}
	if err := codebuddy.DecodeStream(resp.Body, emit); err != nil {
		if c.Writer.Written() {
			return nil, err
		}
		return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: providerName + " upstream stream failed"}
	}
	if !collected.semantic {
		return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: providerName + " upstream returned an empty stream"}
	}
	if collected.finish == "" {
		collected.finish = "stop"
	}
	if len(collected.tools) > 0 {
		collected.finish = "tool_calls"
	}
	usage := codeBuddyUsage(collected.usage, requestBody, codeBuddyCollectedOutputLength(collected))
	if request.Stream {
		if output == providerChatOutputResponses && responsesState != nil {
			responsesState.Usage = apicompat.ChatUsageToResponsesUsage(usage)
		}
		final := providerChatEvent{FinishReason: collected.finish, Usage: providerChatUsage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens}}
		if err := writeProviderChatEvent(c, id, created, request.Model, final, output, anthropicState, responsesState); err != nil {
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
				return nil, fmt.Errorf("invalid %s tool call arguments: %w", providerName, err)
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
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
	} else {
		message := apicompat.ChatMessage{Role: "assistant", Content: json.RawMessage(providerJSONString(collected.content)), ReasoningContent: collected.reasoning, ToolCalls: collected.tools}
		c.JSON(http.StatusOK, apicompat.ChatCompletionsResponse{ID: id, Object: "chat.completion", Created: created, Model: request.Model, Choices: []apicompat.ChatChoice{{Index: 0, Message: message, FinishReason: collected.finish}}, Usage: usage})
	}
	return &ForwardResult{RequestID: resp.Header.Get("x-request-id"), UpstreamHeaders: codeBuddySafeResponseHeaders(resp.Header), Usage: ClaudeUsage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens}, Model: request.Model, UpstreamModel: upstreamModel, Stream: request.Stream, Duration: time.Since(start), FirstTokenMs: collected.firstTokenMs}, nil
}

func workBuddyEmptyDelta(event providerChatEvent) bool {
	return event.FinishReason == "" &&
		(event.Delta.Role == "" || event.Delta.Role == "assistant") &&
		(event.Delta.Content == nil || *event.Delta.Content == "") &&
		(event.Delta.ReasoningContent == nil || *event.Delta.ReasoningContent == "") &&
		len(event.Delta.ToolCalls) == 0
}

func codeBuddyUsage(usage providerChatUsage, request []byte, outputLength int) *apicompat.ChatUsage {
	if usage.InputTokens <= 0 {
		usage.InputTokens = max(1, len(request)/4)
	}
	if usage.OutputTokens <= 0 {
		usage.OutputTokens = max(1, outputLength/4)
	}
	return &apicompat.ChatUsage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.InputTokens + usage.OutputTokens}
}

func codeBuddyCollectedOutputLength(collected *codeBuddyCollected) int {
	if collected == nil {
		return 0
	}
	total := len(collected.content) + len(collected.reasoning)
	for _, call := range collected.tools {
		total += len(call.Function.Name) + len(call.Function.Arguments)
	}
	return total
}

func classifyCodeBuddyHTTPFailure(status int, headers http.Header) *UpstreamFailoverError {
	failure := &UpstreamFailoverError{StatusCode: status, ResponseHeaders: codeBuddySafeResponseHeaders(headers), Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: "CodeBuddy upstream request failed"}
	switch status {
	case http.StatusUnauthorized:
		failure.Stage = GatewayFailureStageAccountAuth
		failure.Reason = GatewayFailureReason("credential_rejected")
		failure.ClientStatusCode = http.StatusUnauthorized
		failure.ClientMessage = "CodeBuddy credential was rejected"
	case http.StatusTooManyRequests:
		failure.ClientStatusCode = http.StatusTooManyRequests
		failure.ClientMessage = "CodeBuddy quota or rate limit reached"
		if resetAt := parseRetryAfterResetTime(headers, time.Now()); resetAt != nil {
			failure.SameAccountRetryDeadline = *resetAt
		}
	case http.StatusBadRequest, http.StatusNotFound:
		failure.Scope = GatewayFailureScopeRequest
		failure.NextAccountAction = NextAccountStop
		failure.ClientStatusCode = http.StatusBadRequest
		failure.ClientMessage = "CodeBuddy does not support the requested model or payload"
	}
	return failure
}

func codeBuddySafeResponseHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	safe := make(http.Header)
	if retryAfter := strings.TrimSpace(headers.Get("Retry-After")); retryAfter != "" {
		safe.Set("Retry-After", retryAfter)
	}
	return safe
}

func codeBuddyTokenFailure(tokenErr *CodeBuddyTokenError) error {
	if tokenErr != nil && tokenErr.Revoked {
		return classifyCodeBuddyHTTPFailure(http.StatusUnauthorized, nil)
	}
	return &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeAccount, NextAccountAction: NextAccountRetry, Reason: GatewayFailureReason("codebuddy_token_unavailable"), ClientStatusCode: http.StatusServiceUnavailable, ClientMessage: "No healthy CodeBuddy account is currently available"}
}

func codeBuddyUnsupportedRealmFailure() error {
	return &UpstreamFailoverError{
		StatusCode:        http.StatusServiceUnavailable,
		Scope:             GatewayFailureScopeAccount,
		NextAccountAction: NextAccountRetry,
		Reason:            GatewayFailureReason("codebuddy_china_requires_separate_provider"),
		ClientStatusCode:  http.StatusServiceUnavailable,
		ClientMessage:     "CodeBuddy China accounts must use the codebuddy-china provider",
	}
}

func writeCodeBuddyChatError(c *gin.Context, status int, typ, message string) error {
	c.JSON(status, gin.H{"error": gin.H{"type": typ, "message": message}})
	return errors.New(message)
}

func writeCodeBuddyAnthropicError(c *gin.Context, status int, typ, message string) error {
	c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": typ, "message": message}})
	return errors.New(message)
}

func writeCodeBuddyRequestError(c *gin.Context, output providerChatOutputProtocol, status int, typ, message string) error {
	switch output {
	case providerChatOutputAnthropic:
		return writeCodeBuddyAnthropicError(c, status, typ, message)
	case providerChatOutputResponses:
		writeResponsesError(c, status, typ, message)
		return errors.New(message)
	default:
		return writeCodeBuddyChatError(c, status, typ, message)
	}
}
