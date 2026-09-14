package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNeonixProxyStatsBaselineMigrationIsPersistentAndSingleton(t *testing.T) {
	data, err := FS.ReadFile("243_neonix_proxy_stats_baseline.sql")
	require.NoError(t, err)
	sql := string(data)
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS neonix_proxy_stats_baseline")
	require.Contains(t, sql, "CHECK (id = 1)")
	require.Contains(t, sql, "ON CONFLICT (id) DO NOTHING")
}
