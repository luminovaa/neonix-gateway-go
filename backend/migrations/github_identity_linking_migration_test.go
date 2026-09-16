package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGitHubIdentityLinkingMigrationKeepsSecretsAndReservationsServerSide(t *testing.T) {
	raw, err := FS.ReadFile("251_github_identity_linking.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "create table if not exists account_github_identity_secrets")
	require.Contains(t, sql, "secret_envelope text not null")
	require.Contains(t, sql, "create table if not exists github_codebuddy_links")
	require.Contains(t, sql, "where status in ('linking', 'active')")
	require.Contains(t, sql, "references accounts(id) on delete cascade")
}
