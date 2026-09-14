package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNeonixLegacyAPIKeyHistoryMigrationCreatesBoundedCompatibilityTables(t *testing.T) {
	data, err := FS.ReadFile("240_neonix_legacy_api_key_history.sql")
	require.NoError(t, err)
	sql := string(data)
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS neonix_legacy_api_key_usage",
		"CREATE TABLE IF NOT EXISTS neonix_legacy_api_key_access_logs",
		"REFERENCES api_keys(id) ON DELETE CASCADE",
	} {
		require.Contains(t, sql, fragment)
	}
	require.NotContains(t, strings.ToLower(sql), "access_token")
	require.NotContains(t, strings.ToLower(sql), "refresh_token")
}
