package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/handler/dto"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	"github.com/luminovaa/neonix-gateway-go/internal/provider"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

type neonixAccountWrite struct {
	Provider    string         `json:"provider"`
	Platform    string         `json:"platform"`
	Type        string         `json:"type"`
	Email       string         `json:"email"`
	Nickname    string         `json:"nickname"`
	Name        string         `json:"name"`
	Credentials map[string]any `json:"credentials"`
	Extra       map[string]any `json:"extra"`
	Enabled     *bool          `json:"enabled"`
}

func (h *AccountHandler) GetByIDCompat(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, http.StatusBadRequest, "invalid account ID")
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, neonixAccountView(account))
}

func (h *AccountHandler) RevealLinkedIdentitySecretCompat(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, http.StatusBadRequest, "invalid account ID")
		return
	}
	password, err := h.adminService.RevealLinkedGitHubIdentityPassword(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"password": password})
}

// CreateCompat accepts the stable Neonix account contract and translates it
// into the normalized Go account model. Canonical platform/type requests are
// accepted too, which keeps this endpoint useful during the cutover.
func (h *AccountHandler) CreateCompat(c *gin.Context) {
	var req neonixAccountWrite
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid account payload")
		return
	}
	input, err := neonixCreateInput(req)
	if err != nil {
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	account, err := h.adminService.CreateAccount(c.Request.Context(), input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if req.Enabled != nil && !*req.Enabled {
		account, err = h.adminService.SetAccountSchedulable(c.Request.Context(), account.ID, false)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
	}
	response.Success(c, neonixAccountView(account))
}

func (h *AccountHandler) BatchCreateCompat(c *gin.Context) {
	var req []neonixAccountWrite
	if err := c.ShouldBindJSON(&req); err != nil || len(req) == 0 || len(req) > 500 {
		response.Error(c, http.StatusBadRequest, "accounts must contain between 1 and 500 items")
		return
	}
	success, failed := 0, 0
	errorsOut := make([]gin.H, 0)
	for index, item := range req {
		input, err := neonixCreateInput(item)
		if err == nil {
			_, err = h.adminService.CreateAccount(c.Request.Context(), input)
		}
		if err != nil {
			failed++
			errorsOut = append(errorsOut, gin.H{"id": strconv.Itoa(index), "error": "account could not be imported"})
			continue
		}
		success++
	}
	response.Success(c, gin.H{"success": success, "failed": failed, "errors": errorsOut})
}

// UpdateCompat preserves stored secrets when the browser sends an empty
// credential field. The browser never needs a decrypted API key to edit the
// label, URLs, or model list.
func (h *AccountHandler) UpdateCompat(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, http.StatusBadRequest, "invalid account ID")
		return
	}
	var req neonixAccountWrite
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid account payload")
		return
	}
	existing, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	credentials := cloneMap(existing.Credentials)
	for key, value := range normalizeNeonixCredentials(req.Provider, req.Credentials) {
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		credentials[key] = value
	}
	extra := cloneMap(existing.Extra)
	if req.Provider != "" {
		extra["source_provider"] = req.Provider
	}
	for key, value := range req.Extra {
		extra[key] = value
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSpace(req.Nickname)
	}
	account, err := h.adminService.UpdateAccount(c.Request.Context(), id, &service.UpdateAccountInput{
		Name: name, Type: req.Type, Credentials: credentials, Extra: extra,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if req.Enabled != nil {
		account, err = h.adminService.SetAccountSchedulable(c.Request.Context(), id, *req.Enabled)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
	}
	response.Success(c, neonixAccountView(account))
}

func neonixCreateInput(req neonixAccountWrite) (*service.CreateAccountInput, error) {
	source := strings.TrimSpace(req.Provider)
	platform := strings.TrimSpace(req.Platform)
	accountType := strings.TrimSpace(req.Type)
	if source != "" {
		if strings.HasPrefix(source, "byok-") || source == "byok" {
			platform, accountType = byokTargetPlatform(source), service.AccountTypeUpstream
		} else {
			definition, ok := provider.Lookup(source)
			if !ok || definition.Deprecated {
				return nil, fmt.Errorf("unsupported provider")
			}
			platform, accountType = definition.TargetPlatform, definition.AccountType
		}
	}
	if platform == "" || accountType == "" {
		return nil, fmt.Errorf("provider is required")
	}
	credentials := normalizeNeonixCredentials(source, req.Credentials)
	if strings.HasPrefix(source, "byok-") || source == "byok" {
		if strings.TrimSpace(stringValue(credentials["api_key"])) == "" {
			return nil, fmt.Errorf("API key is required")
		}
		if strings.TrimSpace(stringValue(credentials["base_url"])) == "" {
			return nil, fmt.Errorf("an [OI] base URL is required")
		}
	}
	extra := cloneMap(req.Extra)
	if source != "" {
		extra["source_provider"] = source
	}
	name := neonixFirstNonEmpty(req.Name, req.Nickname, req.Email, source)
	return &service.CreateAccountInput{
		Name: name, Platform: platform, Type: accountType, Credentials: credentials, Extra: extra,
		Concurrency: 1, LoadFactor: neonixIntPtr(1), SkipDefaultGroupBind: false,
		AutoPauseOnExpired:    neonixBoolPtr(true),
		ProbeEnabled:          neonixBoolPtr(false),
		SkipMixedChannelCheck: false,
	}, nil
}

func normalizeNeonixCredentials(source string, input map[string]any) map[string]any {
	out := make(map[string]any, len(input)+4)
	aliases := map[string]string{
		"apiKey": "api_key", "baseUrl": "base_url", "accessToken": "access_token",
		"refreshToken": "refresh_token", "idToken": "id_token", "clientId": "client_id",
		"clientSecret": "client_secret", "projectId": "project_id", "userAgent": "user_agent",
	}
	for key, value := range input {
		if alias := aliases[key]; alias != "" {
			out[alias] = value
		} else {
			out[key] = value
		}
	}
	if raw, ok := input["byokConfig"].(map[string]any); ok {
		out["byok_config"] = raw
		base := neonixFirstNonEmpty(stringValue(raw["openaiBaseUrl"]), stringValue(input["baseUrl"]))
		if base == "" {
			if preset, found := byokPresetByID(source); found {
				base = preset.OpenAIBaseURL
			}
		}
		if base != "" {
			out["base_url"] = strings.TrimRight(base, "/")
		}
		anthropicBase := strings.TrimRight(stringValue(raw["anthropicBaseUrl"]), "/")
		if anthropicBase == "" {
			if preset, found := byokPresetByID(source); found {
				anthropicBase = preset.AnthropicBaseURL
			}
		}
		if service.IsCNProvider(byokTargetPlatform(source)) && base != "" && anthropicBase != "" {
			out["api_protocol"] = service.APIProtocolAdaptive
			out["api_base_urls"] = map[string]any{
				service.APIProtocolChatCompletions: strings.TrimRight(base, "/"),
				service.APIProtocolAnthropic:       anthropicBase,
			}
		}
		prefix := neonixFirstNonEmpty(stringValue(raw["customPrefix"]), stringValue(raw["prefix"]))
		if prefix == "" {
			if preset, found := byokPresetByID(source); found {
				prefix = preset.Prefix
			}
		}
		models := stringSlice(raw["models"])
		if prefix != "" && len(models) > 0 {
			mapping := make(map[string]any, len(models))
			for _, model := range models {
				mapping[prefix+"/"+model] = model
			}
			out["model_mapping"] = mapping
		}
	}
	return out
}

func byokTargetPlatform(source string) string {
	switch source {
	case "byok-zai":
		return service.PlatformZhipu
	case "byok-deepseek":
		return service.PlatformDeepseek
	case "byok-moonshot":
		return service.PlatformKimi
	case "byok-minimax":
		return service.PlatformMiniMax
	default:
		return provider.TargetPlatform("codex")
	}
}

func neonixAccountView(account *service.Account) gin.H {
	if account == nil {
		return gin.H{}
	}
	providerID := account.Platform
	if source := strings.TrimSpace(stringValue(account.Extra["source_provider"])); source != "" {
		providerID = source
	}
	credentials, status := dto.RedactCredentials(account.Credentials)
	credentials = neonixCredentialAliases(credentials)
	email := neonixFirstNonEmpty(stringValue(account.Credentials["email"]), stringValue(account.Extra["neonix_legacy_email"]))
	createdAt := account.CreatedAt.UnixMilli()
	lastUsedAt := int64(0)
	if account.LastUsedAt != nil {
		lastUsedAt = account.LastUsedAt.UnixMilli()
	}
	return gin.H{
		"id": strconv.FormatInt(account.ID, 10), "provider": providerID, "email": email,
		"nickname": account.Name, "idp": "Manual", "credentials": credentials,
		"credentialsStatus": status, "subscription": gin.H{"type": "Unknown"},
		"usage": gin.H{"current": 0, "limit": 0, "percentUsed": 0, "lastUpdated": time.Now().UnixMilli()},
		"tags":  []string{}, "status": account.Status, "lastError": account.ErrorMessage,
		"enabled": account.Schedulable, "isActive": false, "createdAt": createdAt, "lastUsedAt": lastUsedAt,
	}
}

func neonixCredentialAliases(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	out := cloneMap(input)
	aliases := map[string]string{
		"byok_config": "byokConfig", "base_url": "baseUrl", "access_token": "accessToken",
		"refresh_token": "refreshToken", "client_id": "clientId", "client_secret": "clientSecret",
		"project_id": "projectId", "user_agent": "userAgent",
	}
	for key, alias := range aliases {
		if value, ok := out[key]; ok {
			out[alias] = value
			delete(out, key)
		}
	}
	// Per-format header overrides may contain secondary credentials. Keep them
	// encrypted at rest and out of every browser response.
	if cfg, ok := out["byokConfig"].(map[string]any); ok {
		safe := cloneMap(cfg)
		delete(safe, "openaiHeaders")
		delete(safe, "anthropicHeaders")
		out["byokConfig"] = safe
	}
	return out
}

type byokPreset struct {
	ID, Name, Prefix, OpenAIBaseURL, AnthropicBaseURL, AuthStyle, DocsURL, Surfaces string
	DefaultModels                                                                   []string
}

var byokPresets = []byokPreset{
	{"byok-zai", "Z.AI (GLM)", "zai", "https://api.z.ai/api/paas/v4", "https://api.z.ai/api/anthropic", "auto", "https://docs.z.ai/guides/overview/quick-start", "openai+anthropic", []string{"glm-4.6", "glm-4.5", "glm-4.5-air", "glm-4-flash"}},
	{"byok-deepseek", "DeepSeek", "deepseek", "https://api.deepseek.com", "https://api.deepseek.com/anthropic", "auto", "https://api-docs.deepseek.com/", "openai+anthropic", []string{"deepseek-chat", "deepseek-reasoner"}},
	{"byok-moonshot", "Moonshot (Kimi)", "moonshot", "https://api.moonshot.ai/v1", "https://api.moonshot.ai/anthropic", "auto", "https://platform.moonshot.ai/docs/intro", "openai+anthropic", []string{"kimi-k2-0905-preview", "moonshot-v1-128k", "moonshot-v1-32k", "moonshot-v1-8k"}},
	{"byok-minimax", "MiniMax", "minimax", "https://api.minimaxi.com/v1", "https://api.minimaxi.com/anthropic", "auto", "https://platform.MiniMax.io/docs", "openai+anthropic", []string{"MiniMax-M1", "MiniMax-Text-01", "MiniMax-VL-01"}},
	{"byok-mimo", "Xiaomi MiMo", "mimo", "https://api.xiaomimimo.com/v1", "", "bearer", "https://docs.xiaomimimo.com/", "openai-only", []string{"mimo-v2-flash", "mimo-v2"}},
	{"byok-custom", "Custom (manual URLs)", "custom", "", "", "auto", "", "openai+anthropic", []string{}},
}

func byokPresetByID(id string) (byokPreset, bool) {
	for _, preset := range byokPresets {
		if preset.ID == id {
			return preset, true
		}
	}
	return byokPreset{}, false
}

func (h *AccountHandler) ListBYOKPresetsCompat(c *gin.Context) {
	items := make([]gin.H, 0, len(byokPresets))
	for _, p := range byokPresets {
		item := gin.H{"id": p.ID, "name": p.Name, "prefix": p.Prefix, "authStyle": p.AuthStyle, "defaultModels": p.DefaultModels, "surfaces": p.Surfaces}
		if p.OpenAIBaseURL != "" {
			item["openaiBaseUrl"] = p.OpenAIBaseURL
		}
		if p.AnthropicBaseURL != "" {
			item["anthropicBaseUrl"] = p.AnthropicBaseURL
		}
		if p.DocsURL != "" {
			item["docsUrl"] = p.DocsURL
		}
		items = append(items, item)
	}
	response.Success(c, gin.H{"presets": items})
}

// ValidateProviderCompat performs bounded validation before the browser saves
// a manual provider account. OpenCode can be verified against its public model
// catalog without persistence; provider-specific credentials are checked for
// the minimum material their adapter needs and receive a full warmup after save.
func (h *AccountHandler) ValidateProviderCompat(c *gin.Context) {
	providerID := strings.TrimSpace(c.Param("id"))
	definition, ok := provider.Lookup(providerID)
	if !ok || definition.Deprecated || definition.Category == provider.CategoryIdentity || definition.Category == provider.CategoryBYOK {
		response.Error(c, http.StatusBadRequest, "unsupported provider")
		return
	}
	var req struct {
		Credentials map[string]any `json:"credentials"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid credential payload")
		return
	}
	credentials := normalizeNeonixCredentials(providerID, req.Credentials)
	if missing := missingProviderCredential(providerID, credentials); missing != "" {
		response.Success(c, gin.H{"valid": false, "error": missing})
		return
	}
	if providerID == "oc" && h.accountTestService != nil {
		account := &service.Account{Platform: definition.TargetPlatform, Type: definition.AccountType, Credentials: credentials}
		account.Credentials["base_url"] = "https://opencode.ai/zen/v1"
		ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
		defer cancel()
		if _, err := h.accountTestService.FetchUpstreamSupportedModels(ctx, account); err != nil {
			response.Success(c, gin.H{"valid": false, "error": "OpenCode rejected the credentials"})
			return
		}
	}
	result := gin.H{"valid": true}
	if email := strings.TrimSpace(stringValue(credentials["email"])); email != "" {
		result["userInfo"] = gin.H{"email": email}
	}
	response.Success(c, result)
}

func missingProviderCredential(providerID string, credentials map[string]any) string {
	present := func(keys ...string) bool {
		for _, key := range keys {
			if strings.TrimSpace(stringValue(credentials[key])) != "" {
				return true
			}
		}
		return false
	}
	switch providerID {
	case "kiro":
		if !present("access_token") {
			return "access token is required"
		}
	case "qoder":
		if !present("token", "access_token", "qoder_session_cookie", "rawCookies") {
			return "Qoder PAT or session is required"
		}
	case "oc", "codebuddy-china":
		if !present("api_key", "token") {
			return "API key is required"
		}
	}
	return ""
}

type byokProbeRequest struct {
	APIKey, Provider, PresetID, OpenAIBaseURL, AnthropicBaseURL, BaseURL string
}

func (r *byokProbeRequest) UnmarshalJSON(data []byte) error {
	type wire struct {
		APIKey           string `json:"apiKey"`
		Provider         string `json:"provider"`
		PresetID         string `json:"presetId"`
		OpenAIBaseURL    string `json:"openaiBaseUrl"`
		AnthropicBaseURL string `json:"anthropicBaseUrl"`
		BaseURL          string `json:"baseUrl"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*r = byokProbeRequest(w)
	return nil
}

func (h *AccountHandler) ProbeBYOKModelsCompat(c *gin.Context) {
	var req byokProbeRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.APIKey) == "" {
		response.Error(c, http.StatusBadRequest, "API key is required")
		return
	}
	providerID := neonixFirstNonEmpty(req.PresetID, req.Provider)
	baseURL := neonixFirstNonEmpty(req.OpenAIBaseURL, req.BaseURL)
	if baseURL == "" {
		if p, ok := byokPresetByID(providerID); ok {
			baseURL = p.OpenAIBaseURL
		}
	}
	h.fetchBYOKModels(c, &service.Account{Platform: provider.TargetPlatform("codex"), Type: service.AccountTypeUpstream, Credentials: map[string]any{"api_key": strings.TrimSpace(req.APIKey), "base_url": strings.TrimRight(baseURL, "/")}})
}

func (h *AccountHandler) FetchBYOKModelsCompat(c *gin.Context) {
	account, ok := h.byokAccountFromParam(c)
	if !ok {
		return
	}
	h.fetchBYOKModels(c, account)
}

func (h *AccountHandler) fetchBYOKModels(c *gin.Context, account *service.Account) {
	if h.accountTestService == nil {
		response.Error(c, http.StatusServiceUnavailable, "model discovery is unavailable")
		return
	}
	if err := validateBYOKBaseURL(account.GetCredential("base_url")); err != nil {
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	models, err := h.accountTestService.FetchUpstreamSupportedModels(ctx, account)
	if err != nil {
		response.Error(c, http.StatusBadGateway, "unable to fetch models from upstream")
		return
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if id := strings.TrimSpace(model); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	response.Success(c, gin.H{"models": ids, "source": "openai"})
}

func (h *AccountHandler) TestBYOKModelCompat(c *gin.Context) {
	account, ok := h.byokAccountFromParam(c)
	if !ok {
		return
	}
	var req struct {
		Model  string `json:"model"`
		Format string `json:"format"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Model) == "" {
		response.Error(c, http.StatusBadRequest, "model is required")
		return
	}
	baseURL := account.GetCredential("base_url")
	if req.Format == "anthropic" {
		if cfg, ok := account.Credentials["byok_config"].(map[string]any); ok {
			baseURL = neonixFirstNonEmpty(stringValue(cfg["anthropicBaseUrl"]), baseURL)
		}
	}
	if err := validateBYOKBaseURL(baseURL); err != nil {
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	started := time.Now()
	status, err := probeBYOKModel(c.Request.Context(), baseURL, account.GetCredential("api_key"), req.Model, req.Format)
	if err != nil {
		response.Success(c, gin.H{"success": false, "status": status, "error": safeBYOKProbeError(status, err)})
		return
	}
	response.Success(c, gin.H{"success": true, "status": status, "latencyMs": time.Since(started).Milliseconds()})
}

func (h *AccountHandler) byokAccountFromParam(c *gin.Context) (*service.Account, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, http.StatusBadRequest, "invalid account ID")
		return nil, false
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return nil, false
	}
	source := stringValue(account.Extra["source_provider"])
	if source != "byok" && !strings.HasPrefix(source, "byok-") {
		response.Error(c, http.StatusBadRequest, "account is not a BYOK provider")
		return nil, false
	}
	return account, true
}

func validateBYOKBaseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || u.User != nil {
		return errors.New("invalid upstream URL")
	}
	if u.Scheme != "https" {
		return errors.New("upstream URL must use HTTPS")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return errors.New("local upstream URLs are not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && unsafeBYOKIP(ip) {
		return errors.New("private upstream URLs are not allowed")
	}
	return nil
}

func unsafeBYOKIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func newBYOKHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid upstream address")
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve upstream host: %w", err)
		}
		for _, resolved := range addresses {
			if unsafeBYOKIP(resolved.IP) {
				return nil, errors.New("private upstream address is not allowed")
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
	}
	return &http.Client{
		Timeout:   12 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many upstream redirects")
			}
			return validateBYOKBaseURL(req.URL.String())
		},
	}
}

func probeBYOKModel(parent context.Context, baseURL, apiKey, model, format string) (int, error) {
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	baseURL = strings.TrimRight(baseURL, "/")
	versioned := strings.HasSuffix(baseURL, "/v1") || strings.HasSuffix(baseURL, "/v4")
	path := "/chat/completions"
	body := map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}, "max_tokens": 5, "stream": false}
	if format == "anthropic" {
		path = "/messages"
		body = map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}, "max_tokens": 5}
	}
	if !versioned {
		path = "/v1" + path
	}
	encoded, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, strings.NewReader(string(encoded)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if format == "anthropic" {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := newBYOKHTTPClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("upstream rejected model probe")
	}
	return resp.StatusCode, nil
}

func safeBYOKProbeError(status int, err error) string {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "upstream rejected the API key"
	}
	if status == http.StatusTooManyRequests {
		return "upstream rate limit reached"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "upstream request timed out"
	}
	return "model probe failed"
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}
func stringValue(value any) string { text, _ := value.(string); return text }
func neonixFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
func stringSlice(value any) []string {
	raw, ok := value.([]any)
	if ok {
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if text := strings.TrimSpace(stringValue(item)); text != "" {
				out = append(out, text)
			}
		}
		return out
	}
	if direct, ok := value.([]string); ok {
		return direct
	}
	return nil
}
func neonixIntPtr(value int) *int    { return &value }
func neonixBoolPtr(value bool) *bool { return &value }
