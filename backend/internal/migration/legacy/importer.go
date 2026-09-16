// Package legacy contains the deterministic migration path from the legacy
// Neonix account export into the Go account store.
package legacy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
)

const (
	legacyAccountIDKey       = "neonix_legacy_account_id"
	legacyProviderKey        = "neonix_legacy_provider"
	legacyEmailKey           = "neonix_legacy_email"
	legacyUserIDKey          = "neonix_legacy_user_id"
	legacyIdentityProvider   = "neonix_legacy_idp"
	legacyGroupIDKey         = "neonix_legacy_group_id"
	legacyTagsKey            = "neonix_legacy_tags"
	legacySubscriptionKey    = "neonix_legacy_subscription"
	legacyUsageKey           = "neonix_legacy_usage"
	legacyCheckedAtKey       = "neonix_legacy_last_checked_at_ms"
	legacyMigrationVersion   = 1
	legacyDefaultConcurrency = 3
	legacyDefaultPriority    = 50
)

// ImportOptions controls the database import. Now is injectable so migration
// reports and timestamp handling remain deterministic in tests.
type ImportOptions struct {
	Now func() time.Time
}

// ImportReport describes the durable part of an import. It intentionally has
// no credential fields or raw database errors.
type ImportReport struct {
	Total   int     `json:"total"`
	Created int     `json:"created"`
	Updated int     `json:"updated"`
	Failed  int     `json:"failed"`
	Issues  []Issue `json:"issues,omitempty"`
}

var (
	ErrImportBlocked = errors.New("legacy account import is blocked")
	ErrImportDB      = errors.New("legacy account import database operation failed")
)

// preparedAccount is the short-lived, decrypted representation used only
// while a single PostgreSQL transaction is open. It is never returned from
// the importer or included in an ImportReport.
type preparedAccount struct {
	ID               string
	Provider         string
	Type             string
	Name             string
	AutomationSecret string
	GitHubSecret     string
	Credentials      []byte
	Extra            []byte
	Status           string
	Schedulable      bool
	CreatedAt        *time.Time
	LastUsedAt       *time.Time
}

const findExistingAccountSQL = `
SELECT id
FROM accounts
WHERE deleted_at IS NULL
  AND (
    extra->>'neonix_legacy_account_id' = $1
    OR ($2 <> '' AND platform = $3 AND lower(extra->>'neonix_legacy_email') = lower($2))
  )
ORDER BY CASE WHEN extra->>'neonix_legacy_account_id' = $1 THEN 0 ELSE 1 END, id
LIMIT 1
FOR UPDATE`

const insertImportedAccountSQL = `
INSERT INTO accounts
  (name, platform, type, credentials, extra, concurrency, priority,
   rate_multiplier, status, schedulable, auto_pause_on_expired,
   created_at, updated_at, last_used_at)
VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, 1.0, $8, $9, true,
        COALESCE($10, NOW()), NOW(), $11)
RETURNING id`

const updateImportedAccountSQL = `
UPDATE accounts
SET platform = $1,
    type = $2,
    credentials = $3::jsonb,
    extra = COALESCE(extra, '{}'::jsonb) || $4::jsonb,
    updated_at = NOW()
WHERE id = $5 AND deleted_at IS NULL`

const upsertRegisterAutomationSecretSQL = `
INSERT INTO account_register_automation_secrets (account_id, password_envelope)
VALUES ($1, $2)
ON CONFLICT (account_id) DO UPDATE
SET password_envelope = EXCLUDED.password_envelope,
    updated_at = NOW()`

const upsertGitHubIdentitySecretSQL = `
INSERT INTO account_github_identity_secrets (account_id, secret_envelope)
VALUES ($1, $2)
ON CONFLICT (account_id) DO UPDATE
SET secret_envelope = EXCLUDED.secret_envelope,
    updated_at = NOW()`

// ImportIntoPostgres applies normalized accounts in one transaction. Existing
// rows are matched by the stable legacy ID marker, then by provider + email.
// Only credentials and migration markers are updated on an existing row;
// local name, groups, status, scheduling state, proxy, and runtime metadata
// remain operator-owned.
func ImportIntoPostgres(ctx context.Context, db *sql.DB, accounts []NormalizedAccount, codec *credentials.Envelope, opts ImportOptions) (ImportReport, error) {
	report := ImportReport{Total: len(accounts)}
	prepared, issues := prepareAccounts(accounts, codec, opts)
	if len(issues) > 0 {
		report.Failed = len(issues)
		report.Issues = issues
		return report, ErrImportBlocked
	}
	if len(prepared) == 0 {
		return report, nil
	}
	if db == nil {
		return report, fmt.Errorf("%w: database is nil", ErrImportDB)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("%w: begin transaction", ErrImportDB)
	}
	defer func() { _ = tx.Rollback() }()
	created, updated := 0, 0

	for _, account := range prepared {
		var existingID int64
		err := tx.QueryRowContext(ctx, findExistingAccountSQL, account.ID, legacyEmail(account.Extra), account.Provider).Scan(&existingID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := tx.QueryRowContext(ctx, insertImportedAccountSQL,
				account.Name, account.Provider, account.Type, account.Credentials, account.Extra,
				legacyDefaultConcurrency, legacyDefaultPriority, account.Status, account.Schedulable,
				account.CreatedAt, account.LastUsedAt).Scan(&existingID); err != nil {
				return report, fmt.Errorf("%w: insert account", ErrImportDB)
			}
			if account.AutomationSecret != "" {
				if _, err := tx.ExecContext(ctx, upsertRegisterAutomationSecretSQL, existingID, account.AutomationSecret); err != nil {
					return report, fmt.Errorf("%w: persist register automation secret", ErrImportDB)
				}
			}
			if account.GitHubSecret != "" {
				if _, err := tx.ExecContext(ctx, upsertGitHubIdentitySecretSQL, existingID, account.GitHubSecret); err != nil {
					return report, fmt.Errorf("%w: persist GitHub identity secret", ErrImportDB)
				}
			}
			created++
		case err != nil:
			return report, fmt.Errorf("%w: find account", ErrImportDB)
		default:
			result, err := tx.ExecContext(ctx, updateImportedAccountSQL,
				account.Provider, account.Type, account.Credentials, account.Extra, existingID)
			if err != nil {
				return report, fmt.Errorf("%w: update account", ErrImportDB)
			}
			affected, err := result.RowsAffected()
			if err != nil || affected != 1 {
				return report, fmt.Errorf("%w: account disappeared during update", ErrImportDB)
			}
			if account.AutomationSecret != "" {
				if _, err := tx.ExecContext(ctx, upsertRegisterAutomationSecretSQL, existingID, account.AutomationSecret); err != nil {
					return report, fmt.Errorf("%w: persist register automation secret", ErrImportDB)
				}
			}
			if account.GitHubSecret != "" {
				if _, err := tx.ExecContext(ctx, upsertGitHubIdentitySecretSQL, existingID, account.GitHubSecret); err != nil {
					return report, fmt.Errorf("%w: persist GitHub identity secret", ErrImportDB)
				}
			}
			updated++
		}
	}
	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("%w: commit transaction", ErrImportDB)
	}
	report.Created = created
	report.Updated = updated
	return report, nil
}

func prepareAccounts(accounts []NormalizedAccount, codec *credentials.Envelope, opts ImportOptions) ([]preparedAccount, []Issue) {
	prepared := make([]preparedAccount, 0, len(accounts))
	issues := make([]Issue, 0)
	seen := make(map[string]struct{}, len(accounts))
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	for _, account := range accounts {
		if account.ID == "" {
			issues = append(issues, Issue{ID: account.ID, Provider: account.SourceProvider, Severity: "blocked", Code: "ACCOUNT_ID_MISSING"})
			continue
		}
		if _, ok := seen[account.ID]; ok {
			issues = append(issues, Issue{ID: account.ID, Provider: account.SourceProvider, Severity: "blocked", Code: "DUPLICATE_ACCOUNT_ID"})
			continue
		}
		seen[account.ID] = struct{}{}
		if strings.TrimSpace(account.Provider) == "" || strings.TrimSpace(account.Type) == "" {
			issues = append(issues, Issue{ID: account.ID, Provider: account.SourceProvider, Severity: "blocked", Code: "ACCOUNT_TARGET_METADATA_MISSING"})
			continue
		}
		credentialsMap, err := account.CredentialsMap()
		if err != nil {
			issues = append(issues, Issue{ID: account.ID, Provider: account.SourceProvider, Severity: "blocked", Code: "CREDENTIALS_INVALID"})
			continue
		}
		credentialJSON, err := json.Marshal(credentialsMap)
		if err != nil {
			issues = append(issues, Issue{ID: account.ID, Provider: account.SourceProvider, Severity: "blocked", Code: "CREDENTIALS_SERIALIZE_FAILED"})
			continue
		}
		if account.AutomationSecret != "" {
			if codec == nil {
				issues = append(issues, Issue{ID: account.ID, Provider: account.SourceProvider, Severity: "blocked", Code: "AUTOMATION_SECRET_ENVELOPE_INVALID"})
				continue
			}
			secret, err := codec.Open(account.AutomationSecret)
			if err != nil || strings.TrimSpace(string(secret)) == "" || len(secret) > 4096 {
				issues = append(issues, Issue{ID: account.ID, Provider: account.SourceProvider, Severity: "blocked", Code: "AUTOMATION_SECRET_ENVELOPE_INVALID"})
				continue
			}
		}
		if account.GitHubSecret != "" {
			if codec == nil {
				issues = append(issues, Issue{ID: account.ID, Provider: account.SourceProvider, Severity: "blocked", Code: "GITHUB_IDENTITY_SECRET_ENVELOPE_INVALID"})
				continue
			}
			secret, err := codec.Open(account.GitHubSecret)
			var decoded legacyGitHubSecret
			if err != nil || len(secret) == 0 || len(secret) > 1<<20 || json.Unmarshal(secret, &decoded) != nil || !validLegacyGitHubSecret(decoded) {
				issues = append(issues, Issue{ID: account.ID, Provider: account.SourceProvider, Severity: "blocked", Code: "GITHUB_IDENTITY_SECRET_ENVELOPE_INVALID"})
				continue
			}
		}
		extra, err := importExtra(account)
		if err != nil {
			issues = append(issues, Issue{ID: account.ID, Provider: account.SourceProvider, Severity: "blocked", Code: "ACCOUNT_METADATA_INVALID"})
			continue
		}
		createdAt := unixMillis(account.CreatedAt)
		if createdAt == nil {
			fallback := now()
			createdAt = &fallback
		}
		prepared = append(prepared, preparedAccount{
			ID: account.ID, Provider: account.Provider, Type: account.Type,
			Name: accountName(account), AutomationSecret: account.AutomationSecret, GitHubSecret: account.GitHubSecret, Credentials: credentialJSON, Extra: extra,
			Status: importStatus(account), Schedulable: importSchedulable(account),
			CreatedAt: createdAt, LastUsedAt: unixMillis(account.LastUsedAt),
		})
	}
	return prepared, issues
}

func importExtra(account NormalizedAccount) ([]byte, error) {
	extra := map[string]any{
		legacyAccountIDKey:         account.ID,
		legacyProviderKey:          account.SourceProvider,
		"neonix_migration_version": legacyMigrationVersion,
	}
	if sourceProvider := strings.TrimSpace(account.SourceProvider); sourceProvider != "" {
		// The compatibility summary and Accounts UI use this stable field while
		// the legacy-prefixed marker remains the migration lookup identity.
		extra["source_provider"] = sourceProvider
	}
	if account.Email != "" {
		extra[legacyEmailKey] = strings.TrimSpace(account.Email)
	}
	if account.UserID != "" {
		extra[legacyUserIDKey] = account.UserID
	}
	if account.IDP != "" {
		extra[legacyIdentityProvider] = account.IDP
	}
	if account.GroupID != "" {
		extra[legacyGroupIDKey] = account.GroupID
	}
	if len(account.Tags) > 0 {
		extra[legacyTagsKey] = append([]string(nil), account.Tags...)
	}
	if account.LastCheckedAt != nil {
		extra[legacyCheckedAtKey] = *account.LastCheckedAt
	}
	if account.GitHubCreatedAt > 0 {
		extra["github_created_at_ms"] = account.GitHubCreatedAt
	}
	if account.GitHubEligibleAt > 0 {
		extra["github_eligible_at_ms"] = account.GitHubEligibleAt
	}
	if account.Enabled != nil {
		extra["neonix_legacy_enabled"] = *account.Enabled
	}
	for key, raw := range map[string]json.RawMessage{
		legacySubscriptionKey: account.Subscription,
		legacyUsageKey:        account.Usage,
	} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		if !json.Valid(raw) {
			return nil, errors.New("invalid legacy metadata")
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		extra[key] = value
	}
	return json.Marshal(extra)
}

func legacyEmail(extra []byte) string {
	var metadata map[string]any
	if json.Unmarshal(extra, &metadata) != nil {
		return ""
	}
	value, _ := metadata[legacyEmailKey].(string)
	return strings.TrimSpace(value)
}

func accountName(account NormalizedAccount) string {
	name := strings.TrimSpace(account.Nickname)
	if name == "" {
		name = strings.TrimSpace(account.Email)
	}
	if name == "" {
		name = strings.TrimSpace(account.SourceProvider)
	}
	if name == "" {
		name = "migrated-account"
	}
	if utf8.RuneCountInString(name) > 100 {
		runes := []rune(name)
		name = string(runes[:100])
	}
	return name
}

func importStatus(account NormalizedAccount) string {
	status := strings.ToLower(strings.TrimSpace(account.Status))
	if status != "" {
		return status
	}
	if account.IsActive && (account.Enabled == nil || *account.Enabled) {
		return "active"
	}
	return "disabled"
}

func importSchedulable(account NormalizedAccount) bool {
	if account.Enabled != nil && !*account.Enabled {
		return false
	}
	return account.IsActive && importStatus(account) == "active"
}

func unixMillis(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	// Reject clearly impossible timestamps instead of creating a far-future
	// account that can never be scheduled or expired.
	if value > 32503680000000 { // 3000-01-01 UTC
		return nil
	}
	converted := time.UnixMilli(value).UTC()
	return &converted
}
