package main

import (
	"os"
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/migration/legacy"
	"github.com/stretchr/testify/require"
)

func TestReadMigrationSourceAcceptsCamelAndSnakeCaseAPIKeys(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "migration-*.json")
	require.NoError(t, err)
	_, err = file.WriteString(`{"accounts":[],"apiKeys":[{"id":"camel","key":"secret-camel"}],"api_keys":[{"id":"snake","key":"secret-snake"}]}`)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	accounts, keys, usage, access, settings, err := readMigrationSource(file.Name())
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.Len(t, keys, 1)
	require.Equal(t, "camel", keys[0].ID)
	require.IsType(t, legacy.APIKey{}, keys[0])
	require.Nil(t, settings)
	require.Nil(t, usage)
	require.Nil(t, access)
}

func TestReadMigrationSourceAcceptsSnakeCaseAPIKeyHistory(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "migration-*.json")
	require.NoError(t, err)
	_, err = file.WriteString(`{"api_key_usage":[{"id":11,"apiKeyId":"legacy","timestamp":1710000000000}],"api_key_access_logs":[{"id":22,"apiKeyId":"legacy","ipAddress":"203.0.113.1","accessedAt":1710000001000}]}`)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, _, usage, access, _, err := readMigrationSource(file.Name())
	require.NoError(t, err)
	require.Len(t, usage, 1)
	require.Len(t, access, 1)
	require.Equal(t, "legacy", usage[0].APIKeyID)
	require.Equal(t, "203.0.113.1", access[0].IPAddress)
}
