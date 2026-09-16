package repository

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

type automationPasswordEnvelopeMatcher struct {
	codec    *credentials.Envelope
	password string
}

func (m automationPasswordEnvelopeMatcher) Match(value driver.Value) bool {
	envelope, ok := value.(string)
	if !ok {
		return false
	}
	plaintext, err := m.codec.Open(envelope)
	return err == nil && string(plaintext) == m.password
}

func TestGrokReloginEligibilityExcludesManualPause(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	tests := []struct {
		name    string
		account service.Account
		want    bool
	}{
		{name: "credential error", account: service.Account{Status: service.StatusError}, want: true},
		{name: "legacy expired", account: service.Account{Status: service.StatusExpired}, want: true},
		{name: "legacy exhausted", account: service.Account{Status: "exhausted"}, want: true},
		{name: "auto paused by expiry", account: service.Account{Status: service.StatusActive, AutoPauseOnExpired: true, ExpiresAt: &past}, want: true},
		{name: "manual pause", account: service.Account{Status: service.StatusDisabled, AutoPauseOnExpired: true, ExpiresAt: &past}, want: false},
		{name: "healthy", account: service.Account{Status: service.StatusActive, AutoPauseOnExpired: true, ExpiresAt: &future}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isGrokReloginAccount(tc.account, now))
		})
	}
}

func TestRegisterAutomationPasswordIsEncryptedAndPreservesBytes(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	codec, err := credentials.New([]byte("01234567890123456789012345678901"))
	require.NoError(t, err)
	password := "  exact password\t"
	repo := &accountRepository{sql: db, credentialCodec: codec}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO account_register_automation_secrets (account_id, password_envelope)")).
		WithArgs(int64(7), automationPasswordEnvelopeMatcher{codec: codec, password: password}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.StoreRegisterAutomationPassword(context.Background(), 7, password))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadRegisterAutomationPasswordsDecryptsOnlyBackendEnvelope(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	codec, err := credentials.New([]byte("01234567890123456789012345678901"))
	require.NoError(t, err)
	envelope, err := codec.Seal([]byte("  password bytes  "))
	require.NoError(t, err)
	repo := &accountRepository{sql: db, credentialCodec: codec}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT account_id, password_envelope\nFROM account_register_automation_secrets\nWHERE account_id = ANY($1)")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "password_envelope"}).AddRow(int64(7), envelope))
	got, err := repo.loadRegisterAutomationPasswords(context.Background(), []int64{7})
	require.NoError(t, err)
	require.Equal(t, "  password bytes  ", got[7])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGrokReloginCandidateSummaryCountsWithoutDecryptingSecrets(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &accountRepository{sql: db}
	mock.ExpectQuery(`(?s)SELECT.*COUNT\(\*\) FILTER.*account_register_automation_secrets.*neonix_legacy_email.*credentials->>'email'.*WHERE platform = \$1`).
		WithArgs(service.PlatformGrok, service.StatusError, service.StatusExpired, "exhausted", service.StatusActive).
		WillReturnRows(sqlmock.NewRows([]string{"eligible", "total"}).AddRow(3, 7))
	summary, err := repo.GrokReloginCandidateSummary(context.Background())
	require.NoError(t, err)
	require.Equal(t, service.RegisterReloginCandidateSummary{Count: 3, TotalProvider: 7}, summary)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQoderInjectCandidateUsesOnlyPATAndPreservesDisplayMetadata(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	account := service.Account{
		ID: 17, Name: "Qoder owner", Platform: service.PlatformQoder, Status: service.StatusDisabled, CreatedAt: created,
		Credentials: map[string]any{
			"apiKey": "pt-personal-secret", "refresh_token": "must-not-go-to-worker", "email": "owner@example.com",
		},
		Extra: map[string]any{
			"neonix_legacy_account_id": "legacy-qoder", "neonix_legacy_idp": "Qoder",
			"neonix_legacy_tags": []any{"keep"},
		},
	}
	candidate, ok := qoderInjectCandidateFromAccount(account)
	require.True(t, ok)
	require.Equal(t, "legacy-qoder", candidate.ID)
	require.Equal(t, "owner@example.com", candidate.Email)
	require.Equal(t, "Qoder owner", candidate.Nickname)
	require.Equal(t, []string{"keep"}, candidate.Tags)
	require.Equal(t, created.UnixMilli(), candidate.CreatedAt)
	require.Equal(t, map[string]any{"token": "pt-personal-secret"}, candidate.Credentials)
	require.NotContains(t, candidate.Credentials, "refresh_token")
}

func TestQoderInjectCandidateRejectsAlreadyTrialMissingEmailAndInvalidPAT(t *testing.T) {
	base := service.Account{
		ID: 17, Platform: service.PlatformQoder, Credentials: map[string]any{"token": "pt-valid", "email": "owner@example.com"}, Extra: map[string]any{},
	}
	tests := []struct {
		name   string
		mutate func(*service.Account)
	}{
		{name: "tag", mutate: func(a *service.Account) { a.Extra["neonix_legacy_tags"] = []any{"PRO-TRIAL"} }},
		{name: "subscription", mutate: func(a *service.Account) { a.Extra["neonix_legacy_subscription"] = map[string]any{"title": "Pro Trial"} }},
		{name: "user type", mutate: func(a *service.Account) { a.Credentials["userType"] = "professional_trial" }},
		{name: "plan", mutate: func(a *service.Account) { a.Credentials["plan"] = "Trial" }},
		{name: "missing email", mutate: func(a *service.Account) { delete(a.Credentials, "email") }},
		{name: "invalid PAT", mutate: func(a *service.Account) { a.Credentials["token"] = "ordinary-token" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			account := base
			account.Credentials = map[string]any{"token": "pt-valid", "email": "owner@example.com"}
			account.Extra = map[string]any{}
			tc.mutate(&account)
			_, ok := qoderInjectCandidateFromAccount(account)
			require.False(t, ok)
		})
	}
}

func TestQoderInjectCandidateSummaryCountsWithoutReturningPAT(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &accountRepository{sql: db}
	mock.ExpectQuery(`(?s)SELECT.*COUNT\(\*\) FILTER.*credentials->>'token'.*CHAR_LENGTH\(BTRIM\(pat.value\)\) <= 4096.*LOWER\(BTRIM\(tag.value\)\).*neonix_legacy_subscription.*BETWEEN 1 AND 320.*WHERE platform = \$1`).
		WithArgs(service.PlatformQoder).
		WillReturnRows(sqlmock.NewRows([]string{"eligible", "total"}).AddRow(4, 9))
	summary, err := repo.QoderInjectCandidateSummary(context.Background())
	require.NoError(t, err)
	require.Equal(t, service.RegisterQoderInjectCandidateSummary{Count: 4, TotalProvider: 9}, summary)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRestoreRegisterReloginAccountClearsRuntimeState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &accountRepository{sql: db}
	mock.ExpectExec(`(?s)WITH updated AS \(.*extra = COALESCE\(extra, '\{\}'::jsonb\) - 'model_rate_limits'.*INSERT INTO scheduler_outbox`).
		WithArgs(service.StatusActive, int64(9), service.PlatformGrok, service.SchedulerOutboxEventAccountChanged).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.RestoreRegisterReloginAccount(context.Background(), 9))
	require.NoError(t, mock.ExpectationsWereMet())
}
