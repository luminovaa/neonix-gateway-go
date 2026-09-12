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

func TestImportAPIKeyHistoryUsesMappedKeysAndIsIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO neonix_legacy_api_key_usage").
		WithArgs(int64(11), sqlmock.AnyArg(), "model-a", int64(12), int64(7), 0.25, true, "legacy-key").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO neonix_legacy_api_key_access_logs").
		WithArgs(int64(22), "203.0.113.1", "client", sqlmock.AnyArg(), "legacy-key").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	report, err := ImportAPIKeyHistoryIntoPostgres(context.Background(), db, []APIKeyUsage{{
		ID: 11, APIKeyID: "legacy-key", Timestamp: 1710000000000, Model: "model-a",
		InputTokens: 12, OutputTokens: 7, Credits: 0.25, Success: true,
	}}, []APIKeyAccessLog{{
		ID: 22, APIKeyID: "legacy-key", IPAddress: "203.0.113.1", UserAgent: "client", AccessedAt: 1710000001000,
	}})
	require.NoError(t, err)
	require.Equal(t, APIKeyHistoryReport{UsageTotal: 1, UsageApplied: 1, AccessTotal: 1, AccessApplied: 1}, report)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestImportAPIKeyHistoryRollsBackWhenLegacyKeyIsUnmapped(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO neonix_legacy_api_key_usage").
		WithArgs(int64(11), sqlmock.AnyArg(), "model-a", int64(0), int64(0), 0.0, false, "missing").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	_, err = ImportAPIKeyHistoryIntoPostgres(context.Background(), db, []APIKeyUsage{{
		ID: 11, APIKeyID: "missing", Timestamp: 1710000000000, Model: "model-a",
	}}, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrImportDB)
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
