package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNeonixModelCatalogMigrationProtectsAdminAndSoftDeleteState(t *testing.T) {
	data, err := FS.ReadFile("241_neonix_model_catalog.sql")
	require.NoError(t, err)
	sql := string(data)
	require.Contains(t, sql, "updated_by_admin")
	require.Contains(t, sql, "is_deleted")
	require.Contains(t, sql, "raw_data")
}
