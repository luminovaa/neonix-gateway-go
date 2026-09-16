package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegisterAutomationSecretsMigrationKeepsPasswordOutsideAccounts(t *testing.T) {
	raw, err := FS.ReadFile("250_register_automation_secrets.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "create table if not exists account_register_automation_secrets")
	require.Contains(t, sql, "password_envelope text not null")
	require.Contains(t, sql, "references accounts(id) on delete cascade")
	require.NotContains(t, sql, "alter table accounts")
}
