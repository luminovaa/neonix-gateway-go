package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/antigravity"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/claude"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddy"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddychina"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/geminicli"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/qoder"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/workbuddy"
)

const (
	upstreamModelsBodyLimit             int64 = 8 << 20
	modelsDevRegistryURL                      = "https://models.dev/api.json"
	modelsDevRegistryTTL                      = 6 * time.Hour
	UpstreamModelMetadataExtraKey             = "upstream_model_metadata"
	UpstreamModelMetadataIncompleteCode       = "upstream_model_metadata_incomplete"
	UpstreamModelMetadataPartialCode          = "upstream_model_metadata_partial"
)

type UpstreamModelMetadata struct {
	ID                       string                     `json:"id"`
	DisplayName              string                     `json:"display_name,omitempty"`
	Description              string                     `json:"description,omitempty"`
	Reasoning                *bool                      `json:"reasoning,omitempty"`
	DefaultReasoningLevel    string                     `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels []string                   `json:"supported_reasoning_levels,omitempty"`
	InputModalities          []string                   `json:"input_modalities,omitempty"`
	ContextWindow            int64                      `json:"context_window,omitempty"`
	MaxOutputTokens          int64                      `json:"max_output_tokens,omitempty"`
	CodexToolCapabilities    map[string]json.RawMessage `json:"codex_tool_capabilities,omitempty"`
}

type UpstreamModelMetadataSnapshot struct {
	Source   string                           `json:"source"`
	SyncedAt string                           `json:"synced_at"`
	Models   map[string]UpstreamModelMetadata `json:"models"`
}

type UpstreamModelCatalog struct {
	Models   []string                         `json:"models"`
	Metadata map[string]UpstreamModelMetadata `json:"metadata,omitempty"`
	Warnings []UpstreamModelSyncWarning       `json:"warnings,omitempty"`
}

type UpstreamModelSyncWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	API    string                    `json:"api"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID               string                     `json:"id"`
	Name             string                     `json:"name"`
	Description      string                     `json:"description"`
	Reasoning        *bool                      `json:"reasoning"`
	ReasoningOptions []modelsDevReasoningOption `json:"reasoning_options"`
	Modalities       modelsDevModalities        `json:"modalities"`
	Limit            modelsDevLimit             `json:"limit"`
}

type modelsDevReasoningOption struct {
	Type   string `json:"type"`
	Values []any  `json:"values"`
}

type modelsDevModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type modelsDevLimit struct {
	Context int64 `json:"context"`
	Output  int64 `json:"output"`
}

func (a *Account) SetUpstreamModelMetadataSnapshot(snapshot UpstreamModelMetadataSnapshot) {
	if a == nil {
		return
	}
	if a.Extra == nil {
		a.Extra = make(map[string]any)
	}
	a.Extra[UpstreamModelMetadataExtraKey] = snapshot
}

func (a *Account) GetUpstreamModelMetadataSnapshot() *UpstreamModelMetadataSnapshot {
	if a == nil || a.Extra == nil {
		return nil
	}
	raw, ok := a.Extra[UpstreamModelMetadataExtraKey]
	if !ok || raw == nil {
		return nil
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var snapshot UpstreamModelMetadataSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil || len(snapshot.Models) == 0 {
		return nil
	}
	return &snapshot
}

func (a *Account) GetUpstreamModelMetadata(modelID string) (UpstreamModelMetadata, bool) {
	snapshot := a.GetUpstreamModelMetadataSnapshot()
	if snapshot == nil {
		return UpstreamModelMetadata{}, false
	}
	metadata, ok := snapshot.Models[strings.TrimSpace(modelID)]
	return metadata, ok
}

// UpstreamModelSyncErrorKind classifies model sync failures for safe HTTP mapping.
type UpstreamModelSyncErrorKind string

const (
	// UpstreamModelSyncErrorConfiguration means the account or server configuration cannot perform the sync.
	UpstreamModelSyncErrorConfiguration UpstreamModelSyncErrorKind = "configuration"
	// UpstreamModelSyncErrorUnsupported means the account format is intentionally unsupported for live model sync.
	UpstreamModelSyncErrorUnsupported UpstreamModelSyncErrorKind = "unsupported"
	// UpstreamModelSyncErrorUpstream means the configured upstream failed or returned an unusable response.
	UpstreamModelSyncErrorUpstream UpstreamModelSyncErrorKind = "upstream"
	// UpstreamModelSyncErrorInternal means local persistence or service state failed after a valid upstream response.
	UpstreamModelSyncErrorInternal UpstreamModelSyncErrorKind = "internal"
)

// UpstreamModelSyncError keeps internal failure details wrapped while exposing a safe client message.
type UpstreamModelSyncError struct {
	Kind       UpstreamModelSyncErrorKind
	Message    string
	StatusCode int
	Err        error
}

func (e *UpstreamModelSyncError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return e.Message
	}
	return e.Message + ": " + e.Err.Error()
}

func (e *UpstreamModelSyncError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// SafeMessage returns the sanitized message that can be sent to API clients.
func (e *UpstreamModelSyncError) SafeMessage() string {
	if e == nil || strings.TrimSpace(e.Message) == "" {
		return "Failed to sync upstream models"
	}
	return e.Message
}

func newUpstreamModelSyncConfigError(message string, err error) error {
	return &UpstreamModelSyncError{Kind: UpstreamModelSyncErrorConfiguration, Message: message, Err: err}
}

func newUpstreamModelSyncUnsupportedError(message string, err error) error {
	return &UpstreamModelSyncError{Kind: UpstreamModelSyncErrorUnsupported, Message: message, Err: err}
}

func newUpstreamModelSyncUpstreamError(message string, err error) error {
	return &UpstreamModelSyncError{Kind: UpstreamModelSyncErrorUpstream, Message: message, Err: err}
}

func newUpstreamModelSyncInternalError(message string, err error) error {
	return &UpstreamModelSyncError{Kind: UpstreamModelSyncErrorInternal, Message: message, Err: err}
}

// FetchUpstreamSupportedModels fetches only live model IDs. The admin sync path
// uses SyncUpstreamModelCatalog so capability metadata can also be persisted.
func (s *AccountTestService) FetchUpstreamSupportedModels(ctx context.Context, account *Account) ([]string, error) {
	models, _, err := s.fetchUpstreamModelList(ctx, account)
	return models, err
}

// SyncUpstreamModelCatalog fetches the account's live model list, enriches
// missing capability fields from the provider registry used by the upstream,
// and persists a normalized account snapshot when complete metadata is available.
//
// Persistence is per-model: models with complete capability fields are saved even
// when other IDs in the same sync remain incomplete. An incomplete warning is
// still returned so admins can tell ID sync succeeded without a full capability
// snapshot. When no model is complete, the existing account snapshot is left
// untouched.
func (s *AccountTestService) SyncUpstreamModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error) {
	models, body, err := s.fetchUpstreamModelList(ctx, account)
	liveListAvailable := err == nil
	if err != nil {
		configuredModels := configuredUpstreamModelsForCapabilitySync(account)
		if !upstreamModelListEndpointUnsupported(err) || len(configuredModels) == 0 {
			return nil, err
		}
		models = configuredModels
		body = nil
		slog.Info("upstream model list endpoint unavailable; using configured models for capability sync",
			"account_id", upstreamModelSyncAccountID(account),
			"platform", upstreamModelSyncPlatform(account),
			"status_code", upstreamModelSyncStatusCode(err),
			"model_count", len(models),
		)
	}
	catalog := &UpstreamModelCatalog{Models: models, Metadata: make(map[string]UpstreamModelMetadata)}
	if len(body) > 0 {
		_, directMetadata, parseErr := extractUpstreamModelCatalog(body, account != nil && account.IsGrok())
		if parseErr == nil {
			catalog.Metadata = directMetadata
		}
	}

	// Capability enrichment also covers concrete model_mapping targets. Admins may
	// whitelist models that the live /models list omitted; those still need registry
	// metadata so Codex catalogs can advertise reasoning and modalities.
	enrichIDs := dedupeAndSortModelIDs(append(append([]string{}, models...), configuredUpstreamModelsForCapabilitySync(account)...))
	// Dedicated image/video generators are not Codex agent catalog entries and often
	// omit context windows in public registries. Keep them out of completeness checks
	// so they do not mask successful agent-model capability sync.
	capabilityIDs := capabilitySyncModelIDs(enrichIDs)

	source := "upstream"
	if upstreamCatalogNeedsRegistry(capabilityIDs, catalog.Metadata) {
		if registryMetadata, registryErr := s.fetchModelsDevMetadata(ctx, account, enrichIDs); registryErr == nil {
			for modelID, fallback := range registryMetadata {
				current := catalog.Metadata[modelID]
				merged, changed := mergeUpstreamModelMetadata(current, fallback)
				catalog.Metadata[modelID] = merged
				if changed {
					source = "models.dev"
				}
			}
		} else {
			slog.Warn("upstream model capability metadata enrichment failed",
				"account_id", upstreamModelSyncAccountID(account),
				"platform", upstreamModelSyncPlatform(account),
				"error", registryErr,
			)
		}
	}

	completeMetadata := completeUpstreamModelMetadataSubset(capabilityIDs, catalog.Metadata)
	persistedCapabilities := false
	if len(completeMetadata) > 0 && account != nil && account.ID > 0 && s.accountRepo != nil {
		// Retain known metadata only for models still listed or explicitly mapped.
		if previous := account.GetUpstreamModelMetadataSnapshot(); previous != nil {
			retainedModels := capabilityIDs
			if !liveListAvailable {
				retainedModels = append([]string(nil), capabilityIDs...)
				for modelID := range previous.Models {
					retainedModels = append(retainedModels, modelID)
				}
			}
			for _, modelID := range retainedModels {
				old, exists := previous.Models[modelID]
				if !exists {
					continue
				}
				if entry, ok := completeMetadata[modelID]; ok {
					if entry.CodexToolCapabilities == nil {
						entry.CodexToolCapabilities = make(map[string]json.RawMessage)
					}
					applyCodexToolCapabilities(entry.CodexToolCapabilities, old.CodexToolCapabilities, false)
					completeMetadata[modelID] = entry
				} else {
					completeMetadata[modelID] = old
				}
			}
		}
		snapshot := UpstreamModelMetadataSnapshot{
			Source:   source,
			SyncedAt: time.Now().UTC().Format(time.RFC3339),
			Models:   completeMetadata,
		}
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{UpstreamModelMetadataExtraKey: snapshot}); err != nil {
			return nil, newUpstreamModelSyncInternalError("Failed to save upstream model metadata", err)
		}
		account.SetUpstreamModelMetadataSnapshot(snapshot)
		persistedCapabilities = true
	}

	if upstreamCatalogNeedsRegistry(capabilityIDs, catalog.Metadata) {
		if persistedCapabilities {
			catalog.Warnings = append(catalog.Warnings, UpstreamModelSyncWarning{
				Code:    UpstreamModelMetadataPartialCode,
				Message: "Some model capabilities were saved; remaining models are still incomplete.",
			})
		} else {
			catalog.Warnings = append(catalog.Warnings, UpstreamModelSyncWarning{
				Code:    UpstreamModelMetadataIncompleteCode,
				Message: "Model IDs were synced, but capability metadata is incomplete.",
			})
		}
	}
	return catalog, nil
}

func upstreamModelSyncStatusCode(err error) int {
	var syncErr *UpstreamModelSyncError
	if errors.As(err, &syncErr) {
		return syncErr.StatusCode
	}
	return 0
}

func upstreamModelListEndpointUnsupported(err error) bool {
	statusCode := upstreamModelSyncStatusCode(err)
	return statusCode == http.StatusNotFound || statusCode == http.StatusMethodNotAllowed
}

func configuredUpstreamModelsForCapabilitySync(account *Account) []string {
	if account == nil {
		return nil
	}
	models := make([]string, 0)
	for _, mappedModel := range account.GetModelMapping() {
		mappedModel = strings.TrimSpace(mappedModel)
		if mappedModel == "" || strings.Contains(mappedModel, "*") {
			continue
		}
		models = append(models, mappedModel)
	}
	return dedupeAndSortModelIDs(models)
}

func capabilitySyncModelIDs(modelIDs []string) []string {
	filtered := make([]string, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" || isCodexDedicatedMediaModel(modelID) {
			continue
		}
		filtered = append(filtered, modelID)
	}
	return filtered
}

func upstreamModelSyncAccountID(account *Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID
}

func upstreamModelSyncPlatform(account *Account) string {
	if account == nil {
		return ""
	}
	return account.Platform
}

func upstreamCatalogNeedsRegistry(models []string, metadata map[string]UpstreamModelMetadata) bool {
	for _, modelID := range models {
		modelID = strings.TrimSpace(modelID)
		model, ok := metadata[modelID]
		if !ok || !upstreamModelMetadataIsComplete(model) {
			return true
		}
	}
	return false
}

func upstreamModelMetadataIsUseful(metadata UpstreamModelMetadata) bool {
	return strings.TrimSpace(metadata.DisplayName) != "" ||
		strings.TrimSpace(metadata.Description) != "" ||
		metadata.Reasoning != nil ||
		len(metadata.SupportedReasoningLevels) > 0 ||
		len(metadata.InputModalities) > 0 ||
		len(metadata.CodexToolCapabilities) > 0 ||
		metadata.ContextWindow > 0 ||
		metadata.MaxOutputTokens > 0
}

// upstreamModelMetadataIsComplete reports whether a snapshot entry is safe to
// persist and later prefer over local Codex name-based fallbacks.
func upstreamModelMetadataIsComplete(metadata UpstreamModelMetadata) bool {
	if metadata.Reasoning == nil {
		return false
	}
	if len(normalizeCodexInputModalities(metadata.InputModalities)) == 0 {
		return false
	}
	if metadata.ContextWindow <= 0 {
		return false
	}
	if *metadata.Reasoning && len(normalizeReasoningLevels(metadata.SupportedReasoningLevels)) == 0 {
		return false
	}
	return true
}

func completeUpstreamModelMetadataSubset(
	modelIDs []string,
	metadata map[string]UpstreamModelMetadata,
) map[string]UpstreamModelMetadata {
	if len(metadata) == 0 {
		return nil
	}
	complete := make(map[string]UpstreamModelMetadata)
	for _, modelID := range modelIDs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		entry, ok := metadata[modelID]
		if !ok || !upstreamModelMetadataIsComplete(entry) {
			continue
		}
		if strings.TrimSpace(entry.ID) == "" {
			entry.ID = modelID
		}
		complete[modelID] = entry
	}
	if len(complete) == 0 {
		return nil
	}
	return complete
}

func mergeUpstreamModelMetadata(primary, fallback UpstreamModelMetadata) (UpstreamModelMetadata, bool) {
	merged := primary
	changed := false
	if strings.TrimSpace(merged.ID) == "" && strings.TrimSpace(fallback.ID) != "" {
		merged.ID = strings.TrimSpace(fallback.ID)
		changed = true
	}
	if strings.TrimSpace(merged.DisplayName) == "" && strings.TrimSpace(fallback.DisplayName) != "" {
		merged.DisplayName = strings.TrimSpace(fallback.DisplayName)
		changed = true
	}
	if strings.TrimSpace(merged.Description) == "" && strings.TrimSpace(fallback.Description) != "" {
		merged.Description = strings.TrimSpace(fallback.Description)
		changed = true
	}
	if merged.Reasoning == nil && fallback.Reasoning != nil {
		reasoning := *fallback.Reasoning
		merged.Reasoning = &reasoning
		changed = true
	}
	if strings.TrimSpace(merged.DefaultReasoningLevel) == "" && strings.TrimSpace(fallback.DefaultReasoningLevel) != "" {
		merged.DefaultReasoningLevel = strings.TrimSpace(fallback.DefaultReasoningLevel)
		changed = true
	}
	if len(merged.SupportedReasoningLevels) == 0 && len(fallback.SupportedReasoningLevels) > 0 {
		merged.SupportedReasoningLevels = append([]string(nil), fallback.SupportedReasoningLevels...)
		changed = true
	}
	if len(merged.InputModalities) == 0 && len(fallback.InputModalities) > 0 {
		merged.InputModalities = append([]string(nil), fallback.InputModalities...)
		changed = true
	}
	if merged.ContextWindow <= 0 && fallback.ContextWindow > 0 {
		merged.ContextWindow = fallback.ContextWindow
		changed = true
	}
	if merged.MaxOutputTokens <= 0 && fallback.MaxOutputTokens > 0 {
		merged.MaxOutputTokens = fallback.MaxOutputTokens
		changed = true
	}
	return merged, changed
}

func (s *AccountTestService) fetchModelsDevMetadata(
	ctx context.Context,
	account *Account,
	modelIDs []string,
) (map[string]UpstreamModelMetadata, error) {
	if s == nil || s.httpUpstream == nil || account == nil {
		return nil, fmt.Errorf("model metadata registry is not configured")
	}
	registry, err := s.fetchModelsDevRegistry(ctx, account)
	if err != nil {
		return nil, err
	}
	provider, ok := matchModelsDevProvider(registry, upstreamModelRegistryBaseURL(account))
	if !ok {
		return nil, fmt.Errorf("no model metadata provider matches account base URL")
	}

	metadata := make(map[string]UpstreamModelMetadata)
	for _, modelID := range modelIDs {
		modelID = strings.TrimSpace(modelID)
		model, found := provider.Models[modelID]
		if !found {
			for candidateID, candidate := range provider.Models {
				if strings.EqualFold(strings.TrimSpace(candidateID), modelID) || strings.EqualFold(strings.TrimSpace(candidate.ID), modelID) {
					model = candidate
					found = true
					break
				}
			}
		}
		if !found {
			continue
		}
		entry := upstreamMetadataFromModelsDevModel(modelID, model)
		if upstreamModelMetadataIsUseful(entry) {
			metadata[modelID] = entry
		}
	}
	return metadata, nil
}

func (s *AccountTestService) fetchModelsDevRegistry(ctx context.Context, account *Account) (map[string]modelsDevProvider, error) {
	now := time.Now()
	s.modelMetadataRegistryMu.Lock()
	if len(s.modelMetadataRegistry) > 0 && now.Sub(s.modelMetadataRegistryAt) < modelsDevRegistryTTL {
		cached := s.modelMetadataRegistry
		s.modelMetadataRegistryMu.Unlock()
		return cached, nil
	}
	s.modelMetadataRegistryMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevRegistryURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.doUpstreamModelsRequest(req, upstreamModelsProxyURL(account), account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("model metadata registry returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, upstreamModelsBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > upstreamModelsBodyLimit {
		return nil, fmt.Errorf("model metadata registry response exceeds %d bytes", upstreamModelsBodyLimit)
	}
	var registry map[string]modelsDevProvider
	if err := json.Unmarshal(body, &registry); err != nil {
		return nil, fmt.Errorf("parse model metadata registry: %w", err)
	}
	if len(registry) == 0 {
		return nil, fmt.Errorf("model metadata registry is empty")
	}

	s.modelMetadataRegistryMu.Lock()
	s.modelMetadataRegistry = registry
	s.modelMetadataRegistryAt = now
	s.modelMetadataRegistryMu.Unlock()
	return registry, nil
}

func upstreamMetadataFromModelsDevModel(modelID string, model modelsDevModel) UpstreamModelMetadata {
	levels := reasoningLevelsFromModelsDevOptions(model.ReasoningOptions)
	reasoning := model.Reasoning
	if reasoning == nil && len(levels) > 0 {
		inferred := true
		reasoning = &inferred
	}
	metadata := UpstreamModelMetadata{
		ID:                       strings.TrimSpace(modelID),
		DisplayName:              strings.TrimSpace(model.Name),
		Description:              strings.TrimSpace(model.Description),
		Reasoning:                reasoning,
		SupportedReasoningLevels: levels,
		InputModalities:          normalizeCodexInputModalities(model.Modalities.Input),
		ContextWindow:            model.Limit.Context,
		MaxOutputTokens:          model.Limit.Output,
	}
	if len(levels) > 0 {
		metadata.DefaultReasoningLevel = levels[0]
	}
	if strings.TrimSpace(model.ID) != "" {
		metadata.ID = strings.TrimSpace(model.ID)
	}
	return metadata
}

func reasoningLevelsFromModelsDevOptions(options []modelsDevReasoningOption) []string {
	levels := make([]string, 0)
	for _, option := range options {
		if !strings.EqualFold(strings.TrimSpace(option.Type), "effort") {
			continue
		}
		for _, value := range option.Values {
			if value == nil {
				levels = append(levels, "none")
				continue
			}
			if effort, ok := value.(string); ok {
				levels = append(levels, effort)
			}
		}
	}
	return normalizeReasoningLevels(levels)
}

func upstreamModelRegistryBaseURL(account *Account) string {
	if account == nil {
		return ""
	}
	switch {
	case account.IsOpenAI() || account.IsCNProvider():
		return account.GetOpenAIFormatBaseURL()
	case account.IsGrok():
		return account.GetGrokBaseURL()
	case account.IsGemini():
		return account.GetGeminiBaseURL(geminicli.AIStudioBaseURL)
	case account.IsAnthropic():
		return account.GetBaseURL()
	case account.Platform == PlatformAntigravity:
		return account.GetGeminiBaseURL(geminicli.AIStudioBaseURL)
	default:
		return strings.TrimSpace(account.GetCredential("base_url"))
	}
}

func matchModelsDevProvider(registry map[string]modelsDevProvider, accountBaseURL string) (modelsDevProvider, bool) {
	if provider, ok := matchModelsDevProviderByAPIURL(registry, accountBaseURL); ok {
		return provider, true
	}
	return matchModelsDevProviderByKnownHost(registry, accountBaseURL)
}

func matchModelsDevProviderByAPIURL(registry map[string]modelsDevProvider, accountBaseURL string) (modelsDevProvider, bool) {
	accountBaseURL = normalizeModelRegistryBaseURL(accountBaseURL)
	if accountBaseURL == "" {
		return modelsDevProvider{}, false
	}
	var best modelsDevProvider
	bestScore := -1
	for _, provider := range registry {
		providerBaseURL := normalizeModelRegistryBaseURL(provider.API)
		if providerBaseURL == "" {
			continue
		}
		if accountBaseURL != providerBaseURL &&
			!strings.HasPrefix(accountBaseURL, providerBaseURL+"/") &&
			!strings.HasPrefix(providerBaseURL, accountBaseURL+"/") {
			continue
		}
		if len(providerBaseURL) > bestScore {
			best = provider
			bestScore = len(providerBaseURL)
		}
	}
	return best, bestScore >= 0
}

// matchModelsDevProviderByKnownHost covers first-party hosts whose models.dev
// entries omit the `api` field (notably the official OpenAI provider). Custom
// compatible gateways must still match by API URL so same-named models are not
// cross-attributed across vendors.
func matchModelsDevProviderByKnownHost(registry map[string]modelsDevProvider, accountBaseURL string) (modelsDevProvider, bool) {
	host := modelRegistryHostname(accountBaseURL)
	if host == "" {
		return modelsDevProvider{}, false
	}
	providerID := ""
	switch host {
	case "api.openai.com", "chatgpt.com":
		providerID = "openai"
	default:
		return modelsDevProvider{}, false
	}
	provider, ok := registry[providerID]
	if !ok || len(provider.Models) == 0 {
		return modelsDevProvider{}, false
	}
	if strings.TrimSpace(provider.ID) == "" {
		provider.ID = providerID
	}
	return provider, true
}

func modelRegistryHostname(raw string) string {
	normalized := normalizeModelRegistryBaseURL(raw)
	if normalized == "" {
		return ""
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(parsed.Hostname()))
}

func normalizeModelRegistryBaseURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/models") {
		path = strings.TrimRight(path[:len(path)-len("/models")], "/")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host) + path
}

func (s *AccountTestService) fetchUpstreamModelList(ctx context.Context, account *Account) ([]string, []byte, error) {
	if s == nil {
		return nil, nil, newUpstreamModelSyncConfigError("Account test service is not configured", nil)
	}
	if account == nil {
		return nil, nil, newUpstreamModelSyncConfigError("Account is required", nil)
	}

	if account.Platform == PlatformAntigravity && account.Type != AccountTypeAPIKey {
		models, err := s.fetchAntigravityOAuthUpstreamModels(ctx, account)
		return models, nil, err
	}
	if account.Platform == PlatformKiro {
		return s.fetchKiroUpstreamModels(ctx, account)
	}
	if account.Platform == PlatformQoder {
		return s.fetchQoderUpstreamModels(ctx, account)
	}
	if account.Platform == PlatformCodeBuddy || account.Platform == PlatformWorkBuddy {
		return s.fetchCodeBuddyUpstreamModels(ctx, account)
	}
	if account.Platform == PlatformCodeBuddyChina {
		return fallbackCodeBuddyChinaUpstreamModels()
	}

	if s.httpUpstream == nil {
		return nil, nil, newUpstreamModelSyncConfigError("Upstream HTTP client is not configured", nil)
	}

	req, err := s.buildUpstreamModelsRequest(ctx, account)
	if err != nil {
		return nil, nil, err
	}

	proxyURL := upstreamModelsProxyURL(account)
	resp, err := s.doUpstreamModelsRequest(req, proxyURL, account)
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Failed to request upstream model list", err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyLimit := resolveModelsListReadLimit(s.cfg)
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Failed to read upstream model list", err)
	}
	if int64(len(body)) > bodyLimit {
		return nil, nil, newUpstreamModelSyncUpstreamError("Upstream model list response is too large", fmt.Errorf("response exceeds %d bytes", bodyLimit))
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, &UpstreamModelSyncError{
			Kind:       UpstreamModelSyncErrorUpstream,
			Message:    fmt.Sprintf("Upstream model list request failed with HTTP %d", resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("upstream model list returned HTTP %d", resp.StatusCode),
		}
	}

	extractModels := extractUpstreamModelIDs
	if account.IsGrok() {
		extractModels = extractGrokUpstreamModelIDs
	}
	models, err := extractModels(body)
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Upstream model list response was not valid JSON", err)
	}
	if len(models) == 0 {
		return nil, nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}

	return models, body, nil
}

type qoderAvailableModel struct {
	Value       string   `json:"value"`
	DisplayName string   `json:"displayName"`
	Description string   `json:"description"`
	PriceFactor *float64 `json:"priceFactor"`
	Source      string   `json:"source"`
	IsDefault   bool     `json:"isDefault"`
	SortOrder   *int     `json:"sortOrder"`
}

func (s *AccountTestService) fetchQoderUpstreamModels(ctx context.Context, account *Account) ([]string, []byte, error) {
	token := qoderCredential(account, "token", "accessToken", "access_token", "apiKey", "api_key")
	if token == "" || s.httpUpstream == nil {
		return fallbackQoderUpstreamModels()
	}
	endpoints := []string{
		"https://qoder.com/api/v1/remote/environments?limit=100",
		"https://api.qoder.com/api/v1/remote/environments?limit=100",
		"https://qoder.com/api/v1/cloud/environments",
		"https://api.qoder.com/api/v1/cloud/environments",
	}
	limit := resolveModelsListReadLimit(s.cfg)
	for _, endpoint := range endpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		resp, err := s.httpUpstream.Do(req, upstreamModelsProxyURL(account), account.ID, account.Concurrency)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		_ = resp.Body.Close()
		if readErr != nil || int64(len(body)) > limit || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}
		var payload struct {
			Data []struct {
				Metadata struct {
					AvailableModels []qoderAvailableModel `json:"available_models"`
				} `json:"metadata"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &payload) != nil {
			continue
		}
		seen := make(map[string]struct{})
		available := make([]qoderAvailableModel, 0)
		for _, environment := range payload.Data {
			for _, model := range environment.Metadata.AvailableModels {
				model.Value = strings.TrimSpace(model.Value)
				if model.Value == "" {
					continue
				}
				if _, ok := seen[model.Value]; ok {
					continue
				}
				seen[model.Value] = struct{}{}
				available = append(available, model)
			}
		}
		if len(available) > 0 {
			return buildQoderUpstreamModels(available, "qoder_live")
		}
	}
	return fallbackQoderUpstreamModels()
}

func fallbackQoderUpstreamModels() ([]string, []byte, error) {
	available := make([]qoderAvailableModel, 0, len(qoder.Models))
	for _, model := range qoder.Models {
		priceFactor := model.PriceFactor
		available = append(available, qoderAvailableModel{Value: model.Upstream, DisplayName: model.DisplayName, PriceFactor: &priceFactor})
	}
	return buildQoderUpstreamModels(available, "qoder_fallback")
}

func buildQoderUpstreamModels(available []qoderAvailableModel, catalogSource string) ([]string, []byte, error) {
	models := make([]string, 0, len(available))
	items := make([]map[string]any, 0, len(available))
	seen := make(map[string]struct{}, len(available))
	for index, availableModel := range available {
		model := qoder.Resolve(availableModel.Value)
		normalizedID := strings.ToLower(model.ID)
		if _, duplicate := seen[normalizedID]; duplicate {
			continue
		}
		seen[normalizedID] = struct{}{}
		models = append(models, model.ID)
		modalities := []string{"text"}
		if model.Vision {
			modalities = append(modalities, "image")
		}
		description := strings.TrimSpace(availableModel.Description)
		if description == "" {
			description = "Qoder model via the configured account."
		}
		displayName := strings.TrimSpace(availableModel.DisplayName)
		if displayName == "" {
			displayName = model.DisplayName
		}
		priceFactor := model.PriceFactor
		if availableModel.PriceFactor != nil {
			priceFactor = *availableModel.PriceFactor
		}
		source := strings.TrimSpace(availableModel.Source)
		if source == "" {
			source = catalogSource
		}
		sortOrder := index
		if availableModel.SortOrder != nil {
			sortOrder = *availableModel.SortOrder
		}
		items = append(items, map[string]any{
			"id": model.ID, "actual_model_id": model.Upstream, "display_name": displayName,
			"description": description, "reasoning": model.Reasoning, "input_modalities": modalities,
			"context_window": model.MaxInputTokens, "max_output_tokens": model.MaxOutputTokens,
			"supports_vision": model.Vision, "supports_thinking": model.Reasoning, "price_factor": priceFactor,
			"source": source, "is_default": availableModel.IsDefault, "sort_order": sortOrder,
		})
	}
	body, err := json.Marshal(map[string]any{"object": "list", "data": items, "source": catalogSource})
	if err != nil {
		return nil, nil, newUpstreamModelSyncInternalError("Failed to encode Qoder model list", err)
	}
	return models, body, nil
}

type codeBuddyAvailableModel struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	DisplayName       string   `json:"displayName"`
	Description       string   `json:"description"`
	Tags              []string `json:"tags"`
	ContextWindow     int64    `json:"contextWindow"`
	MaxTokens         int64    `json:"maxTokens"`
	MaxInputTokens    int64    `json:"maxInputTokens"`
	MaxOutputTokens   int64    `json:"maxOutputTokens"`
	SupportsReasoning bool     `json:"supportsReasoning"`
	SupportsImages    bool     `json:"supportsImages"`
	SupportsToolCall  bool     `json:"supportsToolCall"`
	OnlyReasoning     bool     `json:"onlyReasoning"`
	Reasoning         struct {
		DefaultEffort      string   `json:"defaultEffort"`
		SupportedEfforts   []string `json:"supportedEfforts"`
		CanDisableThinking bool     `json:"canDisableThinking"`
	} `json:"reasoning"`
	ModelContextWindow struct {
		DefaultLength    int64   `json:"defaultLength"`
		SupportedLengths []int64 `json:"supportedLengths"`
	} `json:"modelContextWindow"`
}

func (s *AccountTestService) fetchCodeBuddyUpstreamModels(ctx context.Context, account *Account) ([]string, []byte, error) {
	if account == nil || s == nil || s.codeBuddyGatewayService == nil || s.codeBuddyGatewayService.tokens == nil || s.httpUpstream == nil {
		if account != nil && account.Platform == PlatformWorkBuddy {
			return nil, nil, newUpstreamModelSyncConfigError("WorkBuddy model catalog is unavailable", nil)
		}
		return fallbackCodeBuddyUpstreamModels()
	}
	identity := codeBuddyIdentity(account)
	if account.Platform == PlatformWorkBuddy && !workbuddy.IsGlobalDomain(identity.Domain) {
		return nil, nil, newUpstreamModelSyncConfigError("WorkBuddy accounts must use the workbuddy.ai realm", nil)
	}
	if codebuddy.IsChinaRealm(identity.Domain) {
		return nil, nil, newUpstreamModelSyncConfigError("CodeBuddy China accounts must use the codebuddy-china provider", nil)
	}
	if !identity.IsCLI() || strings.TrimSpace(identity.EnterpriseID) == "" {
		if account.Platform == PlatformWorkBuddy {
			return nil, nil, newUpstreamModelSyncConfigError("WorkBuddy requires a CLI OAuth credential and enterprise identity", nil)
		}
		return fallbackCodeBuddyUpstreamModels()
	}
	accessToken, err := s.codeBuddyGatewayService.tokens.AccessToken(ctx, account)
	if err != nil {
		if account.Platform == PlatformWorkBuddy {
			return nil, nil, newUpstreamModelSyncUpstreamError("Failed to obtain WorkBuddy access token", err)
		}
		return fallbackCodeBuddyUpstreamModels()
	}
	identity.AccessToken = accessToken
	hosts := codebuddy.ResolveHosts(identity.Domain)
	endpoint := strings.TrimRight(hosts.Chat, "/") + "/console/enterprises/" + url.PathEscape(identity.EnterpriseID) + "/config/models"
	if account.Platform == PlatformWorkBuddy {
		// WorkBuddy Global serves a JSON plugin catalogue here; its console
		// endpoint redirects to OIDC and must not be treated as a model list.
		endpoint = strings.TrimRight(hosts.Chat, "/") + "/v2/enterprises/personal/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		if account.Platform == PlatformWorkBuddy {
			return nil, nil, newUpstreamModelSyncUpstreamError("Failed to request WorkBuddy model catalog", err)
		}
		return fallbackCodeBuddyUpstreamModels()
	}
	req.Header = codebuddy.CLIHeaders(identity)
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpUpstream.Do(req, upstreamModelsProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return fallbackCodeBuddyUpstreamModels()
	}
	bodyLimit := resolveModelsListReadLimit(s.cfg)
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	_ = resp.Body.Close()
	if readErr != nil || int64(len(body)) > bodyLimit || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if account.Platform == PlatformWorkBuddy {
			return nil, nil, newUpstreamModelSyncUpstreamError("WorkBuddy model catalog request failed", nil)
		}
		return fallbackCodeBuddyUpstreamModels()
	}
	var payload struct {
		Data []codeBuddyAvailableModel `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Data) == 0 {
		if account.Platform == PlatformWorkBuddy {
			return nil, nil, newUpstreamModelSyncUpstreamError("WorkBuddy returned no supported models", nil)
		}
		return fallbackCodeBuddyUpstreamModels()
	}
	chatModels := make([]codeBuddyAvailableModel, 0, len(payload.Data))
	for _, model := range payload.Data {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			continue
		}
		if len(model.Tags) > 0 && !stringSliceContainsFold(model.Tags, "chat") {
			continue
		}
		chatModels = append(chatModels, model)
	}
	if len(chatModels) == 0 {
		if account.Platform == PlatformWorkBuddy {
			return nil, nil, newUpstreamModelSyncUpstreamError("WorkBuddy returned no chat-capable models", nil)
		}
		return fallbackCodeBuddyUpstreamModels()
	}
	if account.Platform == PlatformWorkBuddy {
		return buildWorkBuddyUpstreamModels(chatModels)
	}
	return buildCodeBuddyUpstreamModels(chatModels, "codebuddy_live")
}

// buildWorkBuddyUpstreamModels intentionally derives all capability fields
// from the account-scoped Global catalogue.  WorkBuddy model availability is
// entitlement-dependent, so the CodeBuddy curated table is not a fallback.
func buildWorkBuddyUpstreamModels(available []codeBuddyAvailableModel) ([]string, []byte, error) {
	models := make([]string, 0, len(available))
	items := make([]map[string]any, 0, len(available))
	seen := make(map[string]struct{}, len(available))
	for index, item := range available {
		upstreamID := strings.TrimSpace(item.ID)
		if upstreamID == "" {
			continue
		}
		modelID := "cb/" + strings.TrimPrefix(upstreamID, "cb/")
		key := strings.ToLower(modelID)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, modelID)
		displayName := strings.TrimSpace(item.DisplayName)
		if displayName == "" {
			displayName = strings.TrimSpace(item.Name)
		}
		if displayName == "" {
			displayName = modelID
		}
		contextWindow := item.ModelContextWindow.DefaultLength
		if contextWindow <= 0 {
			contextWindow = item.MaxInputTokens
		}
		if contextWindow <= 0 {
			contextWindow = item.ContextWindow
		}
		maxOutput := item.MaxOutputTokens
		if maxOutput <= 0 {
			maxOutput = item.MaxTokens
		}
		modalities := []string{"text"}
		if item.SupportsImages {
			modalities = append(modalities, "image")
		}
		reasoning := item.SupportsReasoning || item.OnlyReasoning || len(item.Reasoning.SupportedEfforts) > 0
		description := strings.TrimSpace(item.Description)
		if description == "" {
			description = "WorkBuddy model from the authenticated account catalogue."
		}
		items = append(items, map[string]any{
			"id": modelID, "actual_model_id": upstreamID, "display_name": displayName,
			"description": description, "reasoning": reasoning,
			"default_reasoning_level":    item.Reasoning.DefaultEffort,
			"supported_reasoning_levels": item.Reasoning.SupportedEfforts,
			"reasoning_can_be_disabled":  item.Reasoning.CanDisableThinking,
			"only_reasoning":             item.OnlyReasoning,
			"input_modalities":           modalities, "context_window": contextWindow,
			"supported_context_windows": item.ModelContextWindow.SupportedLengths,
			"max_output_tokens":         maxOutput, "supports_vision": item.SupportsImages,
			"supports_thinking": reasoning, "supports_tool_call": item.SupportsToolCall,
			"source": "workbuddy_live", "sort_order": index,
		})
	}
	if len(models) == 0 {
		return nil, nil, newUpstreamModelSyncUpstreamError("WorkBuddy returned no usable models", nil)
	}
	body, err := json.Marshal(map[string]any{"object": "list", "data": items, "source": "workbuddy_live"})
	if err != nil {
		return nil, nil, newUpstreamModelSyncInternalError("Failed to encode WorkBuddy model list", err)
	}
	return models, body, nil
}

func fallbackCodeBuddyUpstreamModels() ([]string, []byte, error) {
	available := make([]codeBuddyAvailableModel, 0, len(codebuddy.Models))
	for _, model := range codebuddy.Models {
		available = append(available, codeBuddyAvailableModel{
			ID: model.Upstream, Name: model.DisplayName, DisplayName: model.DisplayName,
			ContextWindow: int64(model.ContextWindow), MaxTokens: 32000,
		})
	}
	return buildCodeBuddyUpstreamModels(available, "codebuddy_fallback")
}

func fallbackCodeBuddyChinaUpstreamModels() ([]string, []byte, error) {
	models := make([]string, 0, len(codebuddychina.Models))
	items := make([]map[string]any, 0, len(codebuddychina.Models))
	for index, model := range codebuddychina.Models {
		models = append(models, model.ID)
		modalities := []string{"text"}
		if model.Upstream == "glm-5v-turbo" {
			modalities = append(modalities, "image")
		}
		items = append(items, map[string]any{
			"id": model.ID, "actual_model_id": model.Upstream, "display_name": model.DisplayName,
			"description": "CodeBuddy China curated model.", "family": model.Family,
			"input_modalities": modalities, "supports_vision": len(modalities) > 1,
			"source": "codebuddy_china_curated", "sort_order": index,
		})
	}
	body, err := json.Marshal(map[string]any{"object": "list", "data": items, "source": "codebuddy_china_curated"})
	if err != nil {
		return nil, nil, newUpstreamModelSyncInternalError("Failed to encode CodeBuddy China model list", err)
	}
	return models, body, nil
}

func buildCodeBuddyUpstreamModels(available []codeBuddyAvailableModel, source string) ([]string, []byte, error) {
	models := make([]string, 0, len(available))
	items := make([]map[string]any, 0, len(available))
	seen := make(map[string]struct{}, len(available))
	for index, item := range available {
		upstreamID := strings.TrimSpace(item.ID)
		model := codebuddy.ResolveModel(upstreamID)
		modelID := model.ID
		if upstreamID == "" {
			modelID = "cb/"
		}
		key := strings.ToLower(modelID)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, modelID)
		displayName := strings.TrimSpace(item.DisplayName)
		if displayName == "" {
			displayName = strings.TrimSpace(item.Name)
		}
		if displayName == "" {
			displayName = model.DisplayName
		}
		if displayName == "" {
			displayName = modelID
		}
		contextWindow := item.ContextWindow
		if contextWindow <= 0 {
			contextWindow = int64(model.ContextWindow)
		}
		maxTokens := item.MaxTokens
		if maxTokens <= 0 {
			maxTokens = 32000
		}
		reasoning := model.Reasoning || codeBuddyModelLooksReasoning(upstreamID)
		description := strings.TrimSpace(item.Description)
		if description == "" {
			description = "CodeBuddy model via the configured account."
		}
		modalities := []string{"text"}
		if model.Vision {
			modalities = append(modalities, "image")
		}
		items = append(items, map[string]any{
			"id": modelID, "actual_model_id": upstreamID, "display_name": displayName,
			"description": description, "reasoning": reasoning, "input_modalities": modalities,
			"context_window": contextWindow, "max_output_tokens": maxTokens,
			"supports_vision": model.Vision, "supports_thinking": reasoning, "source": source, "sort_order": index,
		})
	}
	body, err := json.Marshal(map[string]any{"object": "list", "data": items, "source": source})
	if err != nil {
		return nil, nil, newUpstreamModelSyncInternalError("Failed to encode CodeBuddy model list", err)
	}
	return models, body, nil
}

func codeBuddyModelLooksReasoning(modelID string) bool {
	modelID = strings.ToLower(modelID)
	return strings.Contains(modelID, "gpt-5") || strings.Contains(modelID, "gemini") || strings.Contains(modelID, "deepseek") || strings.Contains(modelID, "kimi")
}

func stringSliceContainsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), wanted) {
			return true
		}
	}
	return false
}

func (s *AccountTestService) fetchKiroUpstreamModels(ctx context.Context, account *Account) ([]string, []byte, error) {
	if s.kiroGatewayService == nil || s.kiroGatewayService.tokenProvider == nil {
		return nil, nil, newUpstreamModelSyncConfigError("Kiro token provider is not configured", nil)
	}
	if s.httpUpstream == nil {
		return nil, nil, newUpstreamModelSyncConfigError("Upstream HTTP client is not configured", nil)
	}
	token, err := s.kiroGatewayService.tokenProvider.Token(ctx, account)
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Failed to get Kiro access token", err)
	}
	region := strings.ToLower(kiroCredential(account, "region"))
	host := "q.us-east-1.amazonaws.com"
	if strings.HasPrefix(region, "eu-") {
		host = "q.eu-central-1.amazonaws.com"
	}
	machineID := stableKiroMachineID(account)
	models := make([]json.RawMessage, 0, 32)
	bodyLimit := resolveModelsListReadLimit(s.cfg)
	totalModelBytes := int64(0)
	nextToken := ""
	seenTokens := make(map[string]struct{})
	for page := 0; page < 20; page++ {
		if len(nextToken) > 4096 {
			return nil, nil, newUpstreamModelSyncUpstreamError("Kiro model list pagination token is too large", nil)
		}
		if nextToken != "" {
			if _, duplicate := seenTokens[nextToken]; duplicate {
				return nil, nil, newUpstreamModelSyncUpstreamError("Kiro model list repeated a pagination token", nil)
			}
			seenTokens[nextToken] = struct{}{}
		}
		query := url.Values{"origin": []string{"AI_EDITOR"}, "maxResults": []string{"50"}, "profileArn": []string{kiroProfileARN(account)}}
		if nextToken != "" {
			query.Set("nextToken", nextToken)
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/ListAvailableModels?"+query.Encode(), nil)
		if reqErr != nil {
			return nil, nil, newUpstreamModelSyncConfigError("Invalid Kiro model list URL", reqErr)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", kiroUserAgent(machineID))
		req.Header.Set("X-Amz-User-Agent", kiroAmzUserAgent(machineID))
		req.Header.Set("X-Amzn-Codewhisperer-Optout", "true")
		resp, reqErr := s.httpUpstream.Do(req, upstreamModelsProxyURL(account), account.ID, account.Concurrency)
		if reqErr != nil {
			return nil, nil, newUpstreamModelSyncUpstreamError("Failed to request Kiro model list", reqErr)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, nil, newUpstreamModelSyncUpstreamError("Failed to read Kiro model list", readErr)
		}
		if int64(len(body)) > bodyLimit {
			return nil, nil, newUpstreamModelSyncUpstreamError("Kiro model list response is too large", nil)
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, nil, &UpstreamModelSyncError{Kind: UpstreamModelSyncErrorUpstream, Message: fmt.Sprintf("Kiro model list request failed with HTTP %d", resp.StatusCode), StatusCode: resp.StatusCode}
		}
		var pageResult struct {
			Models    []json.RawMessage `json:"models"`
			NextToken string            `json:"nextToken"`
		}
		if err := json.Unmarshal(body, &pageResult); err != nil {
			return nil, nil, newUpstreamModelSyncUpstreamError("Kiro model list response was not valid JSON", err)
		}
		for _, rawModel := range pageResult.Models {
			totalModelBytes += int64(len(rawModel))
			if totalModelBytes > bodyLimit {
				return nil, nil, newUpstreamModelSyncUpstreamError("Kiro model catalog is too large", nil)
			}
			normalized, normalizeErr := normalizeKiroModelEntry(rawModel)
			if normalizeErr != nil {
				return nil, nil, newUpstreamModelSyncUpstreamError("Kiro model list response contained an invalid model", normalizeErr)
			}
			models = append(models, normalized)
		}
		if strings.TrimSpace(pageResult.NextToken) == "" {
			nextToken = ""
			break
		}
		nextToken = pageResult.NextToken
	}
	if strings.TrimSpace(nextToken) != "" {
		return nil, nil, newUpstreamModelSyncUpstreamError("Kiro model list exceeded the pagination limit", nil)
	}
	combined, err := json.Marshal(struct {
		Models []json.RawMessage `json:"models"`
	}{Models: models})
	if err != nil {
		return nil, nil, newUpstreamModelSyncInternalError("Failed to normalize Kiro model list", err)
	}
	ids, err := extractUpstreamModelIDs(combined)
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Kiro model list response was not valid JSON", err)
	}
	if len(ids) == 0 {
		return nil, nil, newUpstreamModelSyncUpstreamError("Kiro returned no supported models", nil)
	}
	return ids, combined, nil
}

type kiroUpstreamModel struct {
	ModelID                            string          `json:"modelId"`
	ModelName                          string          `json:"modelName"`
	Description                        string          `json:"description"`
	SupportedInputTypes                []string        `json:"supportedInputTypes"`
	TokenLimits                        kiroTokenLimits `json:"tokenLimits"`
	AdditionalModelRequestFieldsSchema json.RawMessage `json:"additionalModelRequestFieldsSchema"`
}

type kiroTokenLimits struct {
	MaxInputTokens  int64 `json:"maxInputTokens"`
	MaxOutputTokens int64 `json:"maxOutputTokens"`
}

func normalizeKiroModelEntry(raw json.RawMessage) (json.RawMessage, error) {
	var model kiroUpstreamModel
	if err := json.Unmarshal(raw, &model); err != nil {
		return nil, err
	}
	modelID := strings.TrimSpace(model.ModelID)
	if modelID == "" {
		return nil, errors.New("modelId is missing")
	}
	reasoning := kiroSchemaSupportsThinking(model.AdditionalModelRequestFieldsSchema)
	entry := map[string]any{
		"id":                modelID,
		"modelId":           modelID,
		"display_name":      strings.TrimSpace(model.ModelName),
		"description":       strings.TrimSpace(model.Description),
		"reasoning":         reasoning,
		"input_modalities":  normalizeCodexInputModalities(model.SupportedInputTypes),
		"context_window":    model.TokenLimits.MaxInputTokens,
		"max_output_tokens": model.TokenLimits.MaxOutputTokens,
	}
	if reasoning {
		entry["default_reasoning_level"] = "medium"
		entry["supported_reasoning_levels"] = []string{"low", "medium", "high"}
	}
	return json.Marshal(entry)
}

func kiroSchemaSupportsThinking(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return false
	}
	properties, _ := schema["properties"].(map[string]any)
	_, supported := properties["thinking"]
	return supported
}

func (s *AccountTestService) buildUpstreamModelsRequest(ctx context.Context, account *Account) (*http.Request, error) {
	switch {
	case account.Platform == PlatformAntigravity:
		return s.buildAntigravityAPIKeyModelsRequest(ctx, account)
	case account.Platform == PlatformKiro:
		return nil, newUpstreamModelSyncUnsupportedError("Kiro model discovery uses its native paginated endpoint", nil)
	case account.IsGrok():
		return s.buildGrokUpstreamModelsRequest(ctx, account)
	case account.IsOpenAI() || account.IsCNProvider():
		// 国产 OpenAI 兼容供应商（kimi/zhipu/deepseek）复用 OpenAI /v1/models 探测。
		return s.buildOpenAIUpstreamModelsRequest(ctx, account)
	case account.IsGemini():
		return s.buildGeminiUpstreamModelsRequest(ctx, account)
	case account.IsAnthropic():
		return s.buildAnthropicUpstreamModelsRequest(ctx, account)
	default:
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported platform for upstream model sync: %s", account.Platform), nil,
		)
	}
}

func (s *AccountTestService) buildGrokUpstreamModelsRequest(ctx context.Context, account *Account) (*http.Request, error) {
	if account == nil {
		return nil, newUpstreamModelSyncConfigError("Account is required", nil)
	}

	var (
		authToken         string
		normalizedBaseURL string
		isOAuth           = account.IsGrokOAuth()
	)
	switch account.Type {
	case AccountTypeAPIKey:
		authToken = strings.TrimSpace(account.GetCredential("api_key"))
		if authToken == "" {
			return nil, newUpstreamModelSyncConfigError("No Grok API key is available", nil)
		}

		baseURL := strings.TrimSpace(account.GetCredential("base_url"))
		if baseURL == "" {
			baseURL = "https://api.x.ai"
		}
		validatedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return nil, newUpstreamModelSyncConfigError("Invalid Grok base URL", err)
		}
		normalizedBaseURL = validatedBaseURL
	case AccountTypeOAuth:
		if s.grokTokenProvider == nil {
			return nil, newUpstreamModelSyncConfigError("Grok token provider is not configured", nil)
		}
		accessToken, err := s.grokTokenProvider.GetAccessTokenForManualTest(ctx, account)
		if err != nil {
			return nil, newUpstreamModelSyncUpstreamError("Failed to get Grok access token", err)
		}
		authToken = strings.TrimSpace(accessToken)
		if authToken == "" {
			return nil, newUpstreamModelSyncConfigError("No Grok access token is available", nil)
		}

		validator, err := grokBaseURLValidator(account, s.cfg)
		if err != nil {
			return nil, newUpstreamModelSyncConfigError("Invalid Grok base URL", err)
		}
		baseURL := account.GetGrokBaseURL()
		if s.settingService != nil {
			baseURL = s.settingService.ResolveGrokBaseURL(ctx, account)
		}
		validatedBaseURL, err := validator(baseURL)
		if err != nil {
			return nil, newUpstreamModelSyncConfigError("Invalid Grok base URL", err)
		}
		normalizedBaseURL = validatedBaseURL
	default:
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported Grok account type for upstream model sync: %s", account.Type), nil,
		)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, buildOpenAIModelsURL(normalizedBaseURL), nil)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid Grok model list URL", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+authToken)
	if isOAuth {
		// The shared HTTP transport adds the official CLI marker/version for the
		// exact proxy host. Keep the request builder aligned with the other Grok
		// probes and only forward account identity headers to that trusted host.
		applyGrokCLIHeaders(req.Header)
		if isGrokCLIProxyTarget(req.URL.String()) {
			if userID := strings.TrimSpace(account.GetCredential("sub")); userID != "" {
				req.Header.Set("X-UserID", userID)
			}
			if email := strings.TrimSpace(account.GetCredential("email")); email != "" {
				req.Header.Set("X-Email", email)
			}
		}
	}
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

func (s *AccountTestService) buildAnthropicUpstreamModelsRequest(ctx context.Context, account *Account) (*http.Request, error) {
	if account.IsBedrock() || account.Type == AccountTypeServiceAccount {
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported Anthropic account type for upstream model sync: %s", account.Type), nil,
		)
	}

	baseURL := "https://api.anthropic.com"
	authHeaderName := ""
	authHeaderValue := ""
	apiKeyAuthToken := ""
	betaHeader := ""

	if account.IsOAuth() {
		accessToken := strings.TrimSpace(account.GetCredential("access_token"))
		if accessToken == "" && s.claudeTokenProvider != nil {
			token, tokenErr := s.claudeTokenProvider.GetAccessToken(ctx, account)
			if tokenErr != nil {
				return nil, newUpstreamModelSyncUpstreamError("Failed to get Anthropic access token", tokenErr)
			}
			accessToken = strings.TrimSpace(token)
		}
		if accessToken == "" {
			return nil, newUpstreamModelSyncConfigError("No Anthropic access token is available", nil)
		}
		authHeaderName = "Authorization"
		authHeaderValue = "Bearer " + accessToken
		betaHeader = claude.DefaultBetaHeader
	} else if account.Type == AccountTypeAPIKey {
		apiKey := strings.TrimSpace(account.GetCredential("api_key"))
		if apiKey == "" {
			return nil, newUpstreamModelSyncConfigError("No Anthropic API key is available", nil)
		}
		baseURL = account.GetBaseURL()
		if strings.TrimSpace(baseURL) == "" {
			baseURL = "https://api.anthropic.com"
		}
		apiKeyAuthToken = apiKey
		betaHeader = claude.APIKeyBetaHeader
	} else {
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported Anthropic account type for upstream model sync: %s", account.Type), nil,
		)
	}

	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid Anthropic base URL", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, buildV1ModelsURL(normalizedBaseURL), nil)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid Anthropic model list URL", err)
	}
	for key, value := range claude.DefaultHeaders {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", betaHeader)
	if authHeaderName != "" {
		req.Header.Set(authHeaderName, authHeaderValue)
	} else {
		// Ollama Cloud Anthropic 兼容端点按实际 base_url 强制 Bearer，其余保持
		// extra/default 行为。
		setAnthropicAPIKeyAuthHeader(req.Header, account, apiKeyAuthToken, normalizedBaseURL)
	}
	// 账号级请求头覆写：模型列表探测与真实转发保持一致的最终头
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

func (s *AccountTestService) buildAntigravityAPIKeyModelsRequest(ctx context.Context, account *Account) (*http.Request, error) {
	if account.Type != AccountTypeAPIKey {
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported Antigravity account type for upstream model sync: %s", account.Type), nil,
		)
	}
	apiKey := strings.TrimSpace(account.GetCredential("api_key"))
	if apiKey == "" {
		return nil, newUpstreamModelSyncConfigError("No Antigravity API key is available", nil)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(account.GetCredential("base_url")), "/")
	if baseURL == "" {
		return nil, newUpstreamModelSyncConfigError("Antigravity API-key base URL is required for upstream model sync", nil)
	}
	if !strings.HasSuffix(strings.ToLower(baseURL), "/antigravity") {
		return nil, newUpstreamModelSyncUnsupportedError(
			"Antigravity API-key upstream model sync requires a compatible gateway base URL ending in /antigravity; use Antigravity OAuth for official Cloud Code upstreams",
			nil,
		)
	}
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid Antigravity base URL", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, buildV1ModelsURL(normalizedBaseURL), nil)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid Antigravity model list URL", err)
	}
	for key, value := range claude.DefaultHeaders {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", claude.APIKeyBetaHeader)
	req.Header.Set("x-api-key", apiKey)
	return req, nil
}

func (s *AccountTestService) buildOpenAIUpstreamModelsRequest(ctx context.Context, account *Account) (*http.Request, error) {
	if account.IsOpenAIOAuth() {
		return s.buildOpenAIOAuthUpstreamModelsRequest(ctx, account)
	}
	return buildOpenAIAPIKeyModelsRequest(ctx, account, s.validateUpstreamBaseURL)
}

// buildOpenAIAPIKeyModelsRequest is shared by admin discovery and public model
// listing. Codex content negotiation is intentionally absent from this request.
func buildOpenAIAPIKeyModelsRequest(ctx context.Context, account *Account, validateBaseURL func(string) (string, error)) (*http.Request, error) {
	if account.Type != AccountTypeAPIKey {
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported OpenAI account type for upstream model sync: %s", account.Type), nil,
		)
	}
	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return nil, newUpstreamModelSyncConfigError("No OpenAI API key is available", nil)
	}

	// 协议感知：Anthropic 协议账号的凭证 base_url 指向 /anthropic 端点，模型
	// 列表同步需使用 OpenAI 格式 base（供应商 × 模式默认）。
	baseURL := account.GetOpenAIFormatBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.openai.com"
	}
	normalizedBaseURL, err := validateBaseURL(baseURL)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid OpenAI base URL", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, buildOpenAIModelsURL(normalizedBaseURL), nil)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid OpenAI model list URL", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	// 账号级请求头覆写：模型列表探测与真实转发保持一致的最终头
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// buildOpenAIOAuthUpstreamModelsRequest uses ChatGPT's Codex model manifest.
// OAuth subscriptions do not expose the public Platform API /v1/models endpoint,
// so treating them like API-key accounts makes the admin sync button fail locally.
func (s *AccountTestService) buildOpenAIOAuthUpstreamModelsRequest(ctx context.Context, account *Account) (*http.Request, error) {
	credentialAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Failed to resolve OpenAI account credentials", err)
	}
	if !credentialAccount.IsOpenAIOAuth() {
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported OpenAI account type for upstream model sync: %s", credentialAccount.Type), nil,
		)
	}

	modelsURL, err := buildCodexModelsManifestURL(
		chatgptCodexModelsURL,
		false,
		CodexCanonicalClientVersion(),
	)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid OpenAI Codex model list URL", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL.String(), nil)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid OpenAI Codex model list request", err)
	}

	if credentialAccount.IsOpenAIAgentIdentity() {
		authHeaders, authErr := buildAgentIdentityAuthenticationHeaders(
			ctx,
			s.accountRepo,
			s.agentIdentityWS,
			&s.agentIdentityTaskMu,
			credentialAccount,
		)
		if authErr != nil {
			return nil, newUpstreamModelSyncUpstreamError("Failed to build OpenAI Agent Identity authentication", authErr)
		}
		for key, values := range authHeaders {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	} else {
		accessToken := strings.TrimSpace(credentialAccount.GetOpenAIAccessToken())
		if accessToken == "" {
			return nil, newUpstreamModelSyncConfigError("No OpenAI access token is available", nil)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	identity := resolveCodexOutboundIdentity(credentialAccount.GetOpenAIUserAgent())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Originator", identity.originator)
	req.Header.Set("User-Agent", identity.userAgent)
	req.Header.Set("Version", identity.version)
	setOpenAIChatGPTAccountHeaders(req.Header, credentialAccount)
	credentialAccount.ApplyHeaderOverrides(req.Header)
	enforceCodexIdentityHeadersWithUA(req.Header, credentialAccount.GetOpenAIUserAgent())
	return req, nil
}

func (s *AccountTestService) buildGeminiUpstreamModelsRequest(ctx context.Context, account *Account) (*http.Request, error) {
	baseURL := account.GetGeminiBaseURL(geminicli.AIStudioBaseURL)
	if strings.TrimSpace(baseURL) == "" {
		baseURL = geminicli.AIStudioBaseURL
	}
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid Gemini base URL", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, buildGeminiModelsURL(normalizedBaseURL), nil)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid Gemini model list URL", err)
	}
	req.Header.Set("Accept", "application/json")

	switch account.Type {
	case AccountTypeAPIKey:
		apiKey := strings.TrimSpace(account.GetCredential("api_key"))
		if apiKey == "" {
			return nil, newUpstreamModelSyncConfigError("No Gemini API key is available", nil)
		}
		req.Header.Set("x-goog-api-key", apiKey)
	case AccountTypeOAuth:
		if strings.TrimSpace(account.GetCredential("project_id")) != "" {
			return nil, newUpstreamModelSyncUnsupportedError("Gemini Code Assist model listing is not supported by this sync button", nil)
		}
		if s.geminiTokenProvider == nil {
			return nil, newUpstreamModelSyncConfigError("Gemini token provider is not configured", nil)
		}
		accessToken, tokenErr := s.geminiTokenProvider.GetAccessToken(ctx, account)
		if tokenErr != nil {
			return nil, newUpstreamModelSyncUpstreamError("Failed to get Gemini access token", tokenErr)
		}
		accessToken = strings.TrimSpace(accessToken)
		if accessToken == "" {
			return nil, newUpstreamModelSyncConfigError("No Gemini access token is available", nil)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
	default:
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported Gemini account type for upstream model sync: %s", account.Type), nil,
		)
	}

	return req, nil
}

func (s *AccountTestService) fetchAntigravityOAuthUpstreamModels(ctx context.Context, account *Account) ([]string, error) {
	if s.antigravityGatewayService == nil || s.antigravityGatewayService.GetTokenProvider() == nil {
		return nil, newUpstreamModelSyncConfigError("Antigravity token provider is not configured", nil)
	}

	accessToken, err := s.antigravityGatewayService.GetTokenProvider().GetAccessToken(ctx, account)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to get Antigravity access token", err)
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, newUpstreamModelSyncConfigError("No Antigravity access token is available", nil)
	}

	client, err := antigravity.NewClient(upstreamModelsProxyURL(account))
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Failed to configure Antigravity client", err)
	}
	modelsResp, _, err := client.FetchAvailableModels(
		ctx,
		accessToken,
		strings.TrimSpace(account.GetCredential("project_id")),
		resolveModelsListReadLimit(s.cfg),
	)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to fetch Antigravity available models", err)
	}
	if modelsResp == nil || len(modelsResp.Models) == 0 {
		return nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}

	models := make([]string, 0, len(modelsResp.Models))
	for modelID := range modelsResp.Models {
		models = append(models, strings.TrimSpace(modelID))
	}
	return dedupeAndSortModelIDs(models), nil
}

func (s *AccountTestService) doUpstreamModelsRequest(req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s.tlsFPProfileService == nil {
		return s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, nil)
	}
	return s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
}

func upstreamModelsProxyURL(account *Account) string {
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func buildV1ModelsURL(base string) string {
	normalized := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(normalized, "/v1/models") {
		return normalized
	}
	if strings.HasSuffix(normalized, "/v1") {
		return normalized + "/models"
	}
	return normalized + "/v1/models"
}

func buildOpenAIModelsURL(base string) string {
	return buildOpenAIEndpointURL(base, "/v1/models")
}

func buildGeminiModelsURL(base string) string {
	normalized := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(normalized, "/v1beta/models") {
		return normalized
	}
	if strings.HasSuffix(normalized, "/v1beta") {
		return normalized + "/models"
	}
	return normalized + "/v1beta/models"
}

type upstreamModelEntry struct {
	ID           string          `json:"id"`
	Slug         string          `json:"slug"`
	Model        string          `json:"model"`
	ModelID      string          `json:"modelId"`
	ModelIDSnake string          `json:"model_id"`
	Name         string          `json:"name"`
	Meta         json.RawMessage `json:"_meta"`
}

type upstreamModelEntryMetadata struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Model        string `json:"model"`
	ModelID      string `json:"modelId"`
	ModelIDSnake string `json:"model_id"`
	Name         string `json:"name"`
}

type upstreamModelCapabilityEntry struct {
	upstreamModelEntry
	DisplayName              string                     `json:"display_name"`
	Description              string                     `json:"description"`
	Reasoning                *bool                      `json:"reasoning"`
	DefaultReasoningLevel    string                     `json:"default_reasoning_level"`
	SupportedReasoningLevels []json.RawMessage          `json:"supported_reasoning_levels"`
	ReasoningOptions         []modelsDevReasoningOption `json:"reasoning_options"`
	InputModalities          []string                   `json:"input_modalities"`
	Modalities               modelsDevModalities        `json:"modalities"`
	ContextWindow            int64                      `json:"context_window"`
	MaxContextWindow         int64                      `json:"max_context_window"`
	MaxOutputTokens          int64                      `json:"max_output_tokens"`
	Limit                    modelsDevLimit             `json:"limit"`
}

func extractUpstreamModelIDs(body []byte) ([]string, error) {
	return extractUpstreamModelIDsWithSelector(body, upstreamModelEntryID)
}

func extractGrokUpstreamModelIDs(body []byte) ([]string, error) {
	return extractUpstreamModelIDsWithSelector(body, grokUpstreamModelEntryID)
}

func extractUpstreamModelCatalog(body []byte, grok bool) ([]string, map[string]UpstreamModelMetadata, error) {
	entries, err := extractUpstreamModelRawEntries(body)
	if err != nil {
		return nil, nil, err
	}
	selectID := upstreamModelEntryID
	if grok {
		selectID = grokUpstreamModelEntryID
	}

	models := make([]string, 0, len(entries))
	metadata := make(map[string]UpstreamModelMetadata)
	for _, raw := range entries {
		var capability upstreamModelCapabilityEntry
		if err := json.Unmarshal(raw, &capability); err != nil {
			continue
		}
		modelID := strings.TrimSpace(selectID(capability.upstreamModelEntry))
		if modelID == "" {
			continue
		}
		models = append(models, modelID)
		entry := upstreamMetadataFromCapabilityEntry(modelID, capability)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err == nil {
			entry.CodexToolCapabilities = make(map[string]json.RawMessage)
			applyCodexToolCapabilities(entry.CodexToolCapabilities, fields, true)
		}
		if upstreamModelMetadataIsUseful(entry) {
			metadata[modelID] = entry
		}
	}
	return dedupeAndSortModelIDs(models), metadata, nil
}

func extractUpstreamModelRawEntries(body []byte) ([]json.RawMessage, error) {
	var response struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &response); err == nil && (response.Data != nil || response.Models != nil) {
		entries := make([]json.RawMessage, 0, len(response.Data)+len(response.Models))
		entries = append(entries, response.Data...)
		entries = append(entries, response.Models...)
		return entries, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("parse upstream model catalog: %w", err)
	}
	return entries, nil
}

func upstreamMetadataFromCapabilityEntry(modelID string, entry upstreamModelCapabilityEntry) UpstreamModelMetadata {
	levels := reasoningLevelsFromRawEntries(entry.SupportedReasoningLevels)
	if len(levels) == 0 {
		levels = reasoningLevelsFromModelsDevOptions(entry.ReasoningOptions)
	}
	reasoning := entry.Reasoning
	if reasoning == nil && len(levels) > 0 {
		inferred := len(levels) != 1 || levels[0] != "none"
		reasoning = &inferred
	}
	modalities := entry.InputModalities
	if len(modalities) == 0 {
		modalities = entry.Modalities.Input
	}
	contextWindow := entry.ContextWindow
	if contextWindow <= 0 {
		contextWindow = entry.MaxContextWindow
	}
	if contextWindow <= 0 {
		contextWindow = entry.Limit.Context
	}
	maxOutputTokens := entry.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = entry.Limit.Output
	}
	defaultReasoningLevel := normalizeReasoningLevel(entry.DefaultReasoningLevel)
	if defaultReasoningLevel == "" && len(levels) > 0 {
		defaultReasoningLevel = levels[0]
	}
	displayName := strings.TrimSpace(entry.DisplayName)
	if displayName == "" && strings.TrimSpace(entry.Name) != "" && strings.TrimSpace(entry.Name) != modelID {
		displayName = strings.TrimSpace(entry.Name)
	}
	return UpstreamModelMetadata{
		ID:                       modelID,
		DisplayName:              displayName,
		Description:              strings.TrimSpace(entry.Description),
		Reasoning:                reasoning,
		DefaultReasoningLevel:    defaultReasoningLevel,
		SupportedReasoningLevels: levels,
		InputModalities:          normalizeCodexInputModalities(modalities),
		ContextWindow:            contextWindow,
		MaxOutputTokens:          maxOutputTokens,
	}
}

func reasoningLevelsFromRawEntries(entries []json.RawMessage) []string {
	levels := make([]string, 0, len(entries))
	for _, raw := range entries {
		var effort string
		if err := json.Unmarshal(raw, &effort); err == nil {
			levels = append(levels, effort)
			continue
		}
		var level struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(raw, &level); err == nil {
			levels = append(levels, level.Effort)
		}
	}
	return normalizeReasoningLevels(levels)
}

func normalizeReasoningLevels(levels []string) []string {
	seen := make(map[string]struct{}, len(levels))
	normalized := make([]string, 0, len(levels))
	for _, level := range levels {
		level = normalizeReasoningLevel(level)
		if level == "" {
			continue
		}
		if _, exists := seen[level]; exists {
			continue
		}
		seen[level] = struct{}{}
		normalized = append(normalized, level)
	}
	return normalized
}

func normalizeReasoningLevel(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	switch level {
	case "off", "disabled":
		return "none"
	case "extra-high", "extra_high":
		return "xhigh"
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return level
	default:
		return ""
	}
}

func normalizeCodexInputModalities(modalities []string) []string {
	seen := make(map[string]struct{}, len(modalities))
	normalized := make([]string, 0, len(modalities))
	for _, modality := range modalities {
		modality = strings.ToLower(strings.TrimSpace(modality))
		if modality != "text" && modality != "image" {
			continue
		}
		if _, exists := seen[modality]; exists {
			continue
		}
		seen[modality] = struct{}{}
		normalized = append(normalized, modality)
	}
	return normalized
}

func extractUpstreamModelIDsWithSelector(body []byte, selectID func(upstreamModelEntry) string) ([]string, error) {
	var response struct {
		Data   []upstreamModelEntry `json:"data"`
		Models []upstreamModelEntry `json:"models"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		var arrayResponse []upstreamModelEntry
		if arrayErr := json.Unmarshal(body, &arrayResponse); arrayErr != nil {
			return nil, fmt.Errorf("parse upstream model list: %w", err)
		}

		models := make([]string, 0, len(arrayResponse))
		for _, entry := range arrayResponse {
			models = append(models, selectID(entry))
		}
		return dedupeAndSortModelIDs(models), nil
	}

	models := make([]string, 0, len(response.Data)+len(response.Models))
	for _, entry := range response.Data {
		models = append(models, selectID(entry))
	}
	for _, entry := range response.Models {
		models = append(models, selectID(entry))
	}

	if len(models) == 0 {
		var arrayResponse []upstreamModelEntry
		if err := json.Unmarshal(body, &arrayResponse); err == nil {
			for _, entry := range arrayResponse {
				models = append(models, selectID(entry))
			}
		}
	}

	return dedupeAndSortModelIDs(models), nil
}

func upstreamModelEntryID(entry upstreamModelEntry) string {
	modelID := strings.TrimSpace(entry.ID)
	if modelID == "" {
		modelID = strings.TrimSpace(entry.ModelID)
	}
	if modelID == "" {
		modelID = strings.TrimSpace(entry.ModelIDSnake)
	}
	if modelID == "" {
		modelID = strings.TrimSpace(entry.Slug)
	}
	if modelID == "" {
		modelID = strings.TrimSpace(entry.Name)
	}
	return strings.TrimPrefix(modelID, "models/")
}

func grokUpstreamModelEntryID(entry upstreamModelEntry) string {
	candidates := []string{
		entry.Model,
		entry.ModelID,
		entry.ModelIDSnake,
		entry.ID,
		entry.Slug,
	}
	if len(entry.Meta) > 0 {
		var meta upstreamModelEntryMetadata
		if err := json.Unmarshal(entry.Meta, &meta); err == nil {
			candidates = append(candidates,
				meta.Model,
				meta.ModelID,
				meta.ModelIDSnake,
				meta.ID,
				meta.Slug,
				meta.Name,
			)
		}
	}
	// `name` is a display label in the Grok catalog, so keep it as the final
	// compatibility fallback rather than preferring it over protocol model IDs.
	candidates = append(candidates, entry.Name)
	for _, candidate := range candidates {
		modelID := strings.TrimSpace(candidate)
		if modelID != "" {
			return strings.TrimPrefix(modelID, "models/")
		}
	}
	return ""
}

func dedupeAndSortModelIDs(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	result := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		result = append(result, model)
	}
	sort.Strings(result)
	return result
}
