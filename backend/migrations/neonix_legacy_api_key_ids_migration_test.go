package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNeonixLegacyAPIKeyIDMigrationIsIdempotentAndSecretFree(t *testing.T) {
	data, err := FS.ReadFile("239_neonix_legacy_api_key_ids.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(data))
	require.Contains(t, sql, "create table if not exists neonix_legacy_api_key_ids")
	require.Contains(t, sql, "legacy_id")
	require.Contains(t, sql, "api_key_id")
	require.NotContains(t, sql, "key text")
	require.NotContains(t, sql, "access_token")
}
