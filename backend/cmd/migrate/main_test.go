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

	accounts, keys, settings, err := readMigrationSource(file.Name())
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.Len(t, keys, 1)
	require.Equal(t, "camel", keys[0].ID)
	require.IsType(t, legacy.APIKey{}, keys[0])
	require.Nil(t, settings)
}
