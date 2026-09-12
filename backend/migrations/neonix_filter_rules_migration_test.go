package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNeonixFilterRulesMigration(t *testing.T) {
	raw, err := FS.ReadFile("245_neonix_filter_rules.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "create table if not exists filter_rules")
	require.Contains(t, sql, "rule_id")
	require.Contains(t, sql, "unique")
}
