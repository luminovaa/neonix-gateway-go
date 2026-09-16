package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNeonixProxyLogViewStateMigrationKeepsAuditTablesImmutable(t *testing.T) {
	data, err := FS.ReadFile("244_neonix_proxy_log_view_state.sql")
	require.NoError(t, err)
	sql := string(data)
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS neonix_proxy_log_view_state")
	require.NotContains(t, sql, "DELETE FROM usage_logs")
	require.NotContains(t, sql, "DELETE FROM ops_error_logs")
}
