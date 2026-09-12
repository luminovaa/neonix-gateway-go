package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDisableGroupProfitControlMigration(t *testing.T) {
	raw, err := FS.ReadFile("246_disable_group_profit_control.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "set profit_control_enabled = false")
	require.Contains(t, sql, "profit_min_margin = 0")
	require.Contains(t, sql, "profit_safety_buffer = 0")
}
