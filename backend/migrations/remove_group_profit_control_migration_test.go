package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoveGroupProfitControlMigration(t *testing.T) {
	raw, err := FS.ReadFile("248_remove_group_profit_control.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "create or replace function enqueue_group_auth_cache_invalidation")
	require.NotContains(t, sql, "old.profit_control_enabled")
	require.Contains(t, sql, "drop column if exists profit_control_enabled")
	require.Contains(t, sql, "drop column if exists profit_min_margin")
	require.Contains(t, sql, "drop column if exists profit_safety_buffer")
}
