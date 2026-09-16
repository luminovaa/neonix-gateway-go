package dto

import (
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRedactCredentials_NilInput(t *testing.T) {
	out, status := RedactCredentials(nil)
	require.Nil(t, out)
	require.Nil(t, status)
}

func TestRedactCredentials_StripsSensitiveKeysAndReportsStatus(t *testing.T) {
	in := map[string]any{
		"refresh_token":         "rt-secret",
		"access_token":          "at-secret",
		"api_key":               "sk-secret",
		"aws_secret_access_key": "aws-secret",
		"service_account_json":  map[string]any{"private_key": "..."},
		"private_key":           "raw-key",
		"agent_private_key":     "agent-key-secret",
		// 非敏感
		"base_url":      "https://api.example.com",
		"model_mapping": map[string]any{"foo": "bar"},
		"project_id":    "proj-1",
		"expires_at":    int64(123456),
	}

	out, status := RedactCredentials(in)

	require.NotContains(t, out, "refresh_token")
	require.NotContains(t, out, "access_token")
	require.NotContains(t, out, "api_key")
	require.NotContains(t, out, "aws_secret_access_key")
	require.NotContains(t, out, "service_account_json")
	require.NotContains(t, out, "private_key")
	require.NotContains(t, out, "agent_private_key")

	require.Equal(t, "https://api.example.com", out["base_url"])
	require.Equal(t, map[string]any{"foo": "bar"}, out["model_mapping"])
	require.Equal(t, "proj-1", out["project_id"])
	require.Equal(t, int64(123456), out["expires_at"])

	require.True(t, status["has_refresh_token"])
	require.True(t, status["has_access_token"])
	require.True(t, status["has_api_key"])
	require.True(t, status["has_aws_secret_access_key"])
	require.True(t, status["has_service_account_json"])
	require.True(t, status["has_private_key"])
	require.True(t, status["has_agent_private_key"])

	// 状态 map 不应携带非敏感键的 has_*
	require.NotContains(t, status, "has_base_url")
	require.NotContains(t, status, "has_project_id")
}

func TestRedactCredentials_EmptyValuesNotMarkedPresent(t *testing.T) {
	in := map[string]any{
		"refresh_token": "",
		"access_token":  nil,
		"api_key":       false,
		"id_token":      "actual-id",
	}
	out, status := RedactCredentials(in)
	require.Empty(t, out, "敏感键即使为空也不应出现在 redacted output")
	require.False(t, status["has_refresh_token"])
	require.False(t, status["has_access_token"])
	require.False(t, status["has_api_key"])
	require.True(t, status["has_id_token"])
}

func TestRedactCredentials_DoesNotMutateInput(t *testing.T) {
	in := map[string]any{
		"refresh_token": "secret",
		"base_url":      "x",
	}
	_, _ = RedactCredentials(in)
	require.Equal(t, "secret", in["refresh_token"], "原始 map 不应被修改")
	require.Equal(t, "x", in["base_url"])
}

func TestRedactCredentials_AllKnownSensitiveKeys(t *testing.T) {
	keys := service.SensitiveCredentialKeys
	in := make(map[string]any, len(keys))
	for _, k := range keys {
		in[k] = "filled"
	}
	out, status := RedactCredentials(in)
	require.Empty(t, out)
	for _, k := range keys {
		require.True(t, status["has_"+k], "key %s 应在 status 中标记为已配置", k)
	}
}

func TestRedactCredentials_CodeBuddyCamelCaseSecrets(t *testing.T) {
	in := map[string]any{
		"accessToken": "access-secret", "refreshToken": "refresh-secret",
		"apiKey": "api-secret", "authToken": "auth-secret",
		"sessionToken": "session-secret", "rawCookies": "cookie-secret",
		"userId": "safe-user-id", "enterpriseId": "safe-enterprise-id",
	}
	out, status := RedactCredentials(in)
	for _, key := range []string{"accessToken", "refreshToken", "apiKey", "authToken", "sessionToken", "rawCookies"} {
		require.NotContains(t, out, key)
		require.True(t, status["has_"+key], key)
	}
	require.Equal(t, "safe-user-id", out["userId"])
	require.Equal(t, "safe-enterprise-id", out["enterpriseId"])
}
