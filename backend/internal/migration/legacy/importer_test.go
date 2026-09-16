package legacy

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
)

func importerCodec(t *testing.T) *credentials.Envelope {
	t.Helper()
	codec, err := credentials.New([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func importerAccount(t *testing.T, codec *credentials.Envelope, id, email string) NormalizedAccount {
	t.Helper()
	accounts, report := Convert([]Account{{
		ID: id, Provider: "antigravity", Email: email, Nickname: "Primary", IsActive: true,
		Status: "active", CreatedAt: 1735689600000,
		Credentials: json.RawMessage(`{"access_token":"opaque","refresh_token":"refresh"}`),
	}}, codec, Options{})
	if report.Migrated != 1 || len(accounts) != 1 {
		t.Fatalf("unexpected conversion report: %+v", report)
	}
	return accounts[0]
}

func TestImportIntoPostgresCreatesAccountAndKeepsSecretsOutOfReport(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	codec := importerCodec(t)
	account := importerAccount(t, codec, "legacy-1", "owner@example.com")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(findExistingAccountSQL)).
		WithArgs("legacy-1", "owner@example.com", "antigravity").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertImportedAccountSQL)).
		WithArgs("Primary", "antigravity", "oauth", sqlmock.AnyArg(), sqlmock.AnyArg(), 3, 50, "active", true, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(41)))
	mock.ExpectCommit()

	report, err := ImportIntoPostgres(context.Background(), db, []NormalizedAccount{account}, codec, ImportOptions{Now: func() time.Time {
		return time.Unix(1700000000, 0).UTC()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Total != 1 || report.Created != 1 || report.Updated != 0 || report.Failed != 0 {
		t.Fatalf("unexpected import report: %+v", report)
	}
	encoded, _ := json.Marshal(report)
	if string(encoded) == "" || regexp.MustCompile(`opaque|refresh`).Match(encoded) {
		t.Fatalf("import report contains credential material: %s", encoded)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestImportIntoPostgresUpdatesByLegacyIDWithoutOverwritingLocalState(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	codec := importerCodec(t)
	account := importerAccount(t, codec, "legacy-existing", "owner@example.com")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(findExistingAccountSQL)).
		WithArgs("legacy-existing", "owner@example.com", "antigravity").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(99)))
	mock.ExpectExec(regexp.QuoteMeta(updateImportedAccountSQL)).
		WithArgs("antigravity", "oauth", sqlmock.AnyArg(), sqlmock.AnyArg(), int64(99)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	report, err := ImportIntoPostgres(context.Background(), db, []NormalizedAccount{account}, codec, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Created != 0 || report.Updated != 1 || report.Failed != 0 {
		t.Fatalf("unexpected update report: %+v", report)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestImportIntoPostgresStoresGrokAutomationSecretSeparately(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	codec := importerCodec(t)
	accounts, conversion := Convert([]Account{{
		ID: "legacy-grok", Provider: "grok", Email: "grok@example.com", Password: "  password with spaces  ", IsActive: true, Status: "active",
		Credentials: json.RawMessage(`{"access_token":"access","refresh_token":"refresh","password":"must-not-persist"}`),
	}}, codec, Options{})
	if conversion.Migrated != 1 || len(accounts) != 1 {
		t.Fatalf("unexpected conversion report: %+v", conversion)
	}
	account := accounts[0]

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(findExistingAccountSQL)).
		WithArgs("legacy-grok", "grok@example.com", "grok").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertImportedAccountSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(52)))
	mock.ExpectExec(regexp.QuoteMeta(upsertRegisterAutomationSecretSQL)).
		WithArgs(int64(52), account.AutomationSecret).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	report, err := ImportIntoPostgres(context.Background(), db, accounts, codec, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Created != 1 || report.Failed != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestImportIntoPostgresStoresGitHubIdentitySecretSeparately(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	codec := importerCodec(t)
	accounts, conversion := Convert([]Account{{
		ID: "legacy-github", Provider: "github", Email: "github@example.com", Password: "password",
		Cookies: json.RawMessage(`[{"name":"session","value":"cookie"}]`), Credentials: json.RawMessage(`{"authMethod":"social"}`),
	}}, codec, Options{})
	if conversion.Migrated != 1 || len(accounts) != 1 {
		t.Fatalf("unexpected conversion report: %+v", conversion)
	}
	account := accounts[0]

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(findExistingAccountSQL)).
		WithArgs("legacy-github", "github@example.com", "github").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertImportedAccountSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(53)))
	mock.ExpectExec(regexp.QuoteMeta(upsertGitHubIdentitySecretSQL)).
		WithArgs(int64(53), account.GitHubSecret).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	report, err := ImportIntoPostgres(context.Background(), db, accounts, codec, ImportOptions{})
	if err != nil || report.Created != 1 || report.Failed != 0 {
		t.Fatalf("unexpected import result: report=%+v err=%v", report, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestImportIntoPostgresRollsBackAllRowsOnDatabaseFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	codec := importerCodec(t)
	first := importerAccount(t, codec, "legacy-first", "first@example.com")
	second := importerAccount(t, codec, "legacy-second", "second@example.com")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(findExistingAccountSQL)).
		WithArgs("legacy-first", "first@example.com", "antigravity").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertImportedAccountSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta(findExistingAccountSQL)).
		WithArgs("legacy-second", "second@example.com", "antigravity").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	report, err := ImportIntoPostgres(context.Background(), db, []NormalizedAccount{first, second}, codec, ImportOptions{})
	if err == nil {
		t.Fatal("expected database error")
	}
	if report.Created != 0 || report.Updated != 0 {
		t.Fatalf("rolled-back rows must not be reported as persisted: %+v", report)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareAccountsRejectsInvalidCredentialsWithoutDatabaseAccess(t *testing.T) {
	codec := importerCodec(t)
	account := NormalizedAccount{ID: "bad", Provider: "antigravity", Type: "oauth", Credentials: json.RawMessage(`not-json`)}
	prepared, issues := prepareAccounts([]NormalizedAccount{account}, codec, ImportOptions{})
	if len(prepared) != 0 || len(issues) != 1 || issues[0].Code != "CREDENTIALS_INVALID" {
		t.Fatalf("unexpected preflight result: prepared=%+v issues=%+v", prepared, issues)
	}
}

func TestPrepareAccountsRejectsTamperedAutomationSecretEnvelope(t *testing.T) {
	codec := importerCodec(t)
	account := importerAccount(t, codec, "bad-secret", "owner@example.com")
	account.SourceProvider = "grok"
	account.Provider = "grok"
	account.AutomationSecret = "neonix-credential-v1.tampered"
	prepared, issues := prepareAccounts([]NormalizedAccount{account}, codec, ImportOptions{})
	if len(prepared) != 0 || len(issues) != 1 || issues[0].Code != "AUTOMATION_SECRET_ENVELOPE_INVALID" {
		t.Fatalf("unexpected preflight result: prepared=%+v issues=%+v", prepared, issues)
	}
}

func TestImportExtraKeepsProviderSummarySourceIdentity(t *testing.T) {
	account := NormalizedAccount{ID: "legacy-codex", SourceProvider: "codex", Provider: "openai", Type: "oauth"}
	extra, err := importExtra(account)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(extra, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["source_provider"] != "codex" || decoded[legacyProviderKey] != "codex" {
		t.Fatalf("provider source identity was not preserved: %#v", decoded)
	}
}
