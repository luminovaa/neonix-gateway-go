package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDropAccountCredentialEnvelopesMigrationIsForwardOnly(t *testing.T) {
	raw, err := FS.ReadFile("252_drop_account_credential_envelopes.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "drop table if exists account_credential_envelopes")
	require.Contains(t, sql, "do not modify migration 238")

	legacy, err := FS.ReadFile("238_account_credential_envelopes.sql")
	require.NoError(t, err)
	require.Contains(t, strings.ToLower(string(legacy)), "create table if not exists account_credential_envelopes")
}
