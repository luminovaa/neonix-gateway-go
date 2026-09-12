package legacy

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestNormalizeSettingsAllowlistsAndRedactsNestedProxySecrets(t *testing.T) {
	settings, report := NormalizeSettings(map[string]json.RawMessage{
		"theme":                   json.RawMessage(`"dark"`),
		"proxySettings":           json.RawMessage(`{"host":"127.0.0.1","apiKey":"secret","password":"pw"}`),
		"githubOAuthClientSecret": json.RawMessage(`"secret"`),
	})
	require.Equal(t, 3, report.Total)
	require.Equal(t, 2, report.Applied)
	require.JSONEq(t, `{"host":"127.0.0.1"}`, settings["proxySettings"])
	require.NotContains(t, mustJSON(t, settings), "secret")
}

func TestNormalizeAPIKeysBlocksMissingSecretsWithoutLeakingThem(t *testing.T) {
	keys, report := NormalizeAPIKeys([]APIKey{
		{ID: "legacy-1", UserID: "old-user", Name: "default", Key: "neon-secret", IsActive: true, CreatedAt: 1710000000000},
		{ID: "legacy-2", Name: "missing", KeyHash: "hash-only", IsActive: true},
	}, func() time.Time { return time.UnixMilli(1710000000000).UTC() })
	require.Len(t, keys, 1)
	require.Equal(t, "legacy-1", keys[0].LegacyID)
	require.Equal(t, "neon-secret", keys[0].Key)
	require.Equal(t, 1, report.Failed)
	require.Equal(t, "API_KEY_SECRET_MISSING", report.Issues[0].Code)
	require.NotContains(t, mustJSON(t, report), "neon-secret")
}

func TestNormalizeAPIKeysRejectsDuplicateSecretsAndBoundsNames(t *testing.T) {
	longName := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	keys, report := NormalizeAPIKeys([]APIKey{
		{ID: "one", Name: longName, Key: "same", IsActive: true},
		{ID: "two", Name: "duplicate", Key: "same", IsActive: true},
	}, func() time.Time { return time.UnixMilli(1710000000000).UTC() })
	require.Len(t, keys, 1)
	require.Equal(t, apiKeyImportNameLen, len([]rune(keys[0].Name)))
	require.Equal(t, "DUPLICATE_API_KEY_SECRET", report.Issues[0].Code)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return string(data)
}

func TestImportAPIKeysIntoPostgresMapsToSingleOperatorWithoutReportingSecrets(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id FROM users").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(99)))
	mock.ExpectQuery("SELECT api_key_id FROM neonix_legacy_api_key_ids").WithArgs("legacy-1").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT id FROM api_keys").WithArgs("neon-secret").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("INSERT INTO api_keys").WithArgs(int64(99), "neon-secret", "default", "active", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	mock.ExpectExec("INSERT INTO neonix_legacy_api_key_ids").WithArgs("legacy-1", int64(7), "legacy-user").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	report, err := ImportAPIKeysIntoPostgres(context.Background(), db, []NormalizedAPIKey{{
		LegacyID: "legacy-1", LegacyUserID: "legacy-user", Name: "default", Key: "neon-secret", Status: "active",
	}}, func() time.Time { return time.UnixMilli(1710000000000).UTC() })
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)
	require.NotContains(t, mustJSON(t, report), "neon-secret")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestImportSettingsIntoPostgresUsesOneTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO settings").WithArgs("theme", `"dark"`, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	report, err := ImportSettingsIntoPostgres(context.Background(), db, NormalizedSettings{"theme": `"dark"`}, func() time.Time {
		return time.UnixMilli(1710000000000).UTC()
	})
	require.NoError(t, err)
	require.Equal(t, 1, report.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}
