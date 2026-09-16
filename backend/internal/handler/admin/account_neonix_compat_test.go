package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/provider"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNeonixCreateInputNormalizesBYOKAccount(t *testing.T) {
	input, err := neonixCreateInput(neonixAccountWrite{
		Provider: "byok-deepseek", Nickname: "personal deepseek",
		Credentials: map[string]any{
			"apiKey":     "secret",
			"byokConfig": map[string]any{"models": []any{"deepseek-chat", "deepseek-reasoner"}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, service.PlatformDeepseek, input.Platform)
	require.Equal(t, service.AccountTypeUpstream, input.Type)
	require.Equal(t, "secret", input.Credentials["api_key"])
	require.Equal(t, "https://api.deepseek.com", input.Credentials["base_url"])
	require.Equal(t, service.APIProtocolAdaptive, input.Credentials["api_protocol"])
	require.Equal(t, "byok-deepseek", input.Extra["source_provider"])
	require.Equal(t, map[string]any{
		"deepseek/deepseek-chat":     "deepseek-chat",
		"deepseek/deepseek-reasoner": "deepseek-reasoner",
	}, input.Credentials["model_mapping"])
}

func TestNeonixCreateInputRejectsRetiredProvider(t *testing.T) {
	_, err := neonixCreateInput(neonixAccountWrite{Provider: "codebuff", Credentials: map[string]any{"token": "secret"}})
	require.EqualError(t, err, "unsupported provider")
}

func TestNeonixAccountViewUsesSourceProviderAndNeverReturnsSecrets(t *testing.T) {
	now := time.Now().UTC()
	view := neonixAccountView(&service.Account{
		ID: 42, Name: "DeepSeek", Platform: provider.TargetPlatform("codex"), Type: service.AccountTypeUpstream,
		Credentials: map[string]any{
			"api_key": "secret",
			"byok_config": map[string]any{
				"models": []any{"deepseek-chat"}, "openaiBaseUrl": "https://api.deepseek.com",
				"openaiHeaders": map[string]any{"X-Secret": "secret"},
			},
		},
		Extra: map[string]any{"source_provider": "byok-deepseek"}, Status: service.StatusActive, Schedulable: true, CreatedAt: now, LastUsedAt: &now,
	})
	require.Equal(t, "42", view["id"])
	require.Equal(t, "byok-deepseek", view["provider"])
	credentials := view["credentials"].(map[string]any)
	require.NotContains(t, credentials, "api_key")
	cfg := credentials["byokConfig"].(map[string]any)
	require.NotContains(t, cfg, "openaiHeaders")
	require.Equal(t, []any{"deepseek-chat"}, cfg["models"])
}

func TestValidateBYOKBaseURLBlocksLocalNetworks(t *testing.T) {
	for _, raw := range []string{"http://api.example.com/v1", "https://localhost/v1", "https://127.0.0.1/v1", "https://10.0.0.2/v1", "https://metadata.local/v1"} {
		require.Error(t, validateBYOKBaseURL(raw), raw)
	}
	require.NoError(t, validateBYOKBaseURL("https://api.example.com/v1"))
}

func TestBYOKPresetCatalogHasStableUniqueIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, preset := range byokPresets {
		require.False(t, seen[preset.ID])
		seen[preset.ID] = true
		require.NotEmpty(t, preset.Name)
		require.NotEmpty(t, preset.Prefix)
	}
	require.True(t, seen["byok-custom"])
}

func TestMissingProviderCredentialUsesNormalizedCredentialNames(t *testing.T) {
	require.Equal(t, "access token is required", missingProviderCredential("kiro", map[string]any{}))
	require.Empty(t, missingProviderCredential("kiro", map[string]any{"access_token": "token"}))
	require.Equal(t, "Qoder PAT or session is required", missingProviderCredential("qoder", map[string]any{}))
	require.Empty(t, missingProviderCredential("qoder", map[string]any{"token": "pat"}))
	require.Equal(t, "API key is required", missingProviderCredential("oc", map[string]any{}))
}

func TestBYOKTargetPlatformsUseNativeAdaptiveAdapters(t *testing.T) {
	require.Equal(t, service.PlatformZhipu, byokTargetPlatform("byok-zai"))
	require.Equal(t, service.PlatformDeepseek, byokTargetPlatform("byok-deepseek"))
	require.Equal(t, service.PlatformKimi, byokTargetPlatform("byok-moonshot"))
	require.Equal(t, service.PlatformMiniMax, byokTargetPlatform("byok-minimax"))
	require.Equal(t, provider.TargetPlatform("codex"), byokTargetPlatform("byok-custom"))
}

func TestRevealLinkedIdentitySecretCompatIsNoStoreAndMinimal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAccountHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/api/accounts/:id/linked-identity/reveal", h.RevealLinkedIdentitySecretCompat)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/accounts/7/linked-identity/reveal", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	var payload struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, map[string]any{"password": "github-password"}, payload.Data)
}
