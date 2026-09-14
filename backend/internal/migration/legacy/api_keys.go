package legacy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// APIKey is the redacted-compatible source shape emitted by the legacy Node
// API-key store. Key is intentionally kept only in memory while importing; it
// is never copied into APIKeyIssue or an import report.
type APIKey struct {
	ID         string `json:"id"`
	UserID     string `json:"userId,omitempty"`
	Name       string `json:"name,omitempty"`
	Key        string `json:"key,omitempty"`
	KeyHash    string `json:"keyHash,omitempty"`
	KeyPrefix  string `json:"keyPrefix,omitempty"`
	CreatedAt  int64  `json:"createdAt,omitempty"`
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`
	IsActive   bool   `json:"isActive"`
}

// NormalizedAPIKey is a short-lived import value. Do not serialize this type
// or include it in any user-visible report because Key is a bearer secret.
type NormalizedAPIKey struct {
	LegacyID     string
	LegacyUserID string
	Name         string
	Key          string
	CreatedAt    *time.Time
	LastUsedAt   *time.Time
	Status       string
}

type APIKeyIssue struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
	Severity string `json:"severity"`
	Code     string `json:"code"`
}

type APIKeyReport struct {
	Total   int           `json:"total"`
	Created int           `json:"created"`
	Updated int           `json:"updated"`
	Failed  int           `json:"failed"`
	Issues  []APIKeyIssue `json:"issues,omitempty"`
}

var ErrAPIKeyImportBlocked = errors.New("legacy API-key import is blocked")

const (
	apiKeyImportMaxLen  = 128
	apiKeyImportNameLen = 100
)

// NormalizeAPIKeys validates source keys without exposing their credential
// bytes. A missing decrypted key is blocked because importing only a hash
// would make the existing client configuration unusable after cutover.
func NormalizeAPIKeys(keys []APIKey, now func() time.Time) ([]NormalizedAPIKey, APIKeyReport) {
	report := APIKeyReport{Total: len(keys)}
	if now == nil {
		now = time.Now
	}
	result := make([]NormalizedAPIKey, 0, len(keys))
	seenIDs := make(map[string]struct{}, len(keys))
	seenKeys := make(map[string]struct{}, len(keys))
	for _, source := range keys {
		id := strings.TrimSpace(source.ID)
		if id == "" {
			report.Failed++
			report.Issues = append(report.Issues, APIKeyIssue{Severity: "blocked", Code: "API_KEY_ID_MISSING"})
			continue
		}
		if _, exists := seenIDs[id]; exists {
			report.Failed++
			report.Issues = append(report.Issues, APIKeyIssue{ID: id, Severity: "blocked", Code: "DUPLICATE_API_KEY_ID"})
			continue
		}
		seenIDs[id] = struct{}{}
		key := strings.TrimSpace(source.Key)
		if key == "" {
			report.Failed++
			report.Issues = append(report.Issues, APIKeyIssue{ID: id, Severity: "blocked", Code: "API_KEY_SECRET_MISSING"})
			continue
		}
		if len(key) > apiKeyImportMaxLen {
			report.Failed++
			report.Issues = append(report.Issues, APIKeyIssue{ID: id, Severity: "blocked", Code: "API_KEY_SECRET_TOO_LONG"})
			continue
		}
		if _, exists := seenKeys[key]; exists {
			report.Failed++
			report.Issues = append(report.Issues, APIKeyIssue{ID: id, Severity: "blocked", Code: "DUPLICATE_API_KEY_SECRET"})
			continue
		}
		seenKeys[key] = struct{}{}
		name := strings.TrimSpace(source.Name)
		if name == "" {
			name = "migrated-key"
		}
		if utf8.RuneCountInString(name) > apiKeyImportNameLen {
			name = string([]rune(name)[:apiKeyImportNameLen])
		}
		createdAt := importTime(source.CreatedAt, now())
		status := "disabled"
		if source.IsActive {
			status = "active"
		}
		result = append(result, NormalizedAPIKey{
			LegacyID: id, LegacyUserID: strings.TrimSpace(source.UserID), Name: name,
			Key: key, CreatedAt: createdAt, LastUsedAt: importTime(source.LastUsedAt, time.Time{}), Status: status,
		})
		report.Created++
	}
	if len(report.Issues) > 0 {
		report.Created = 0
	}
	return result, report
}

func importTime(milliseconds int64, fallback time.Time) *time.Time {
	if milliseconds <= 0 || milliseconds > 32503680000000 {
		if fallback.IsZero() {
			return nil
		}
		value := fallback.UTC()
		return &value
	}
	value := time.UnixMilli(milliseconds).UTC()
	return &value
}

// ImportAPIKeysIntoPostgres imports normalized keys in one transaction. Every
// key is attached to the first active admin, which is the documented Neonix
// single-operator deployment model. Existing local metadata is retained on a
// mapped key; only the bearer key and last-used timestamp are refreshed.
func ImportAPIKeysIntoPostgres(ctx context.Context, db *sql.DB, keys []NormalizedAPIKey, now func() time.Time) (APIKeyReport, error) {
	report := APIKeyReport{Total: len(keys)}
	if len(keys) == 0 {
		return report, nil
	}
	if db == nil {
		return report, fmt.Errorf("%w: database is nil", ErrImportDB)
	}
	if now == nil {
		now = time.Now
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("%w: begin API-key transaction", ErrImportDB)
	}
	defer func() { _ = tx.Rollback() }()
	var operatorID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM users
		WHERE role = 'admin' AND deleted_at IS NULL
		ORDER BY id LIMIT 1`).Scan(&operatorID); err != nil {
		return report, fmt.Errorf("%w: operator user is unavailable", ErrImportDB)
	}
	for _, key := range keys {
		var mappedID int64
		err := tx.QueryRowContext(ctx, `
			SELECT api_key_id FROM neonix_legacy_api_key_ids
			WHERE legacy_id = $1 FOR UPDATE`, key.LegacyID).Scan(&mappedID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// A key with the same secret may have been imported by a previous
			// interrupted run; reuse it instead of creating a duplicate.
			err = tx.QueryRowContext(ctx, `
				SELECT id FROM api_keys
				WHERE key = $1 AND deleted_at IS NULL
				LIMIT 1`, key.Key).Scan(&mappedID)
			if errors.Is(err, sql.ErrNoRows) {
				createdAt := key.CreatedAt
				if createdAt == nil {
					fallback := now().UTC()
					createdAt = &fallback
				}
				if err := tx.QueryRowContext(ctx, `
					INSERT INTO api_keys
					  (user_id, key, name, status, created_at, updated_at, last_used_at)
					VALUES ($1, $2, $3, $4, $5, NOW(), $6)
					RETURNING id`, operatorID, key.Key, key.Name, key.Status, createdAt, key.LastUsedAt).Scan(&mappedID); err != nil {
					return report, fmt.Errorf("%w: insert API key", ErrImportDB)
				}
				report.Created++
			} else if err != nil {
				return report, fmt.Errorf("%w: find API key by secret", ErrImportDB)
			} else {
				report.Updated++
			}
		case err != nil:
			return report, fmt.Errorf("%w: find API-key mapping", ErrImportDB)
		default:
			if _, err := tx.ExecContext(ctx, `
				UPDATE api_keys SET key = $1, last_used_at = $2, updated_at = NOW()
				WHERE id = $3 AND deleted_at IS NULL`, key.Key, key.LastUsedAt, mappedID); err != nil {
				return report, fmt.Errorf("%w: update API key", ErrImportDB)
			}
			report.Updated++
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO neonix_legacy_api_key_ids (legacy_id, api_key_id, legacy_user_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (legacy_id) DO UPDATE SET
			  api_key_id = EXCLUDED.api_key_id,
			  legacy_user_id = EXCLUDED.legacy_user_id`, key.LegacyID, mappedID, key.LegacyUserID); err != nil {
			return report, fmt.Errorf("%w: persist API-key mapping", ErrImportDB)
		}
	}
	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("%w: commit API-key transaction", ErrImportDB)
	}
	return report, nil
}
