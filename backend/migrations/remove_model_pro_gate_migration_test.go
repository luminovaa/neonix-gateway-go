package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoveModelProGateMigrationDropsPaidGate(t *testing.T) {
	data, err := FS.ReadFile("242_remove_model_pro_gate.sql")
	require.NoError(t, err)
	sql := string(data)
	require.Contains(t, sql, "DROP COLUMN IF EXISTS requires_pro")
}
