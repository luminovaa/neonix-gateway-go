package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

var githubLinkJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

const githubIdentityRowsSQL = `
SELECT
    a.id,
    COALESCE(NULLIF(BTRIM(a.extra->>'neonix_legacy_account_id'), ''), a.id::text) AS public_id,
    COALESCE(NULLIF(BTRIM(a.extra->>'neonix_legacy_email'), ''), NULLIF(BTRIM(a.credentials->>'email'), ''), '') AS email,
    COALESCE(NULLIF(BTRIM(a.credentials->>'github_username'), ''), NULLIF(BTRIM(a.credentials->>'githubUsername'), ''), '') AS username,
    a.status,
    CASE WHEN LOWER(COALESCE(a.extra->>'neonix_legacy_enabled', 'true')) = 'false' THEN FALSE ELSE TRUE END AS enabled,
    CASE WHEN COALESCE(a.extra->>'github_created_at_ms', '') ~ '^[0-9]+$' THEN (a.extra->>'github_created_at_ms')::bigint END AS github_created_at,
    CASE WHEN COALESCE(a.extra->>'github_eligible_at_ms', '') ~ '^[0-9]+$' THEN (a.extra->>'github_eligible_at_ms')::bigint END AS github_eligible_at,
    l.status AS link_status,
    COALESCE(NULLIF(BTRIM(linked.extra->>'neonix_legacy_account_id'), ''), linked.id::text) AS linked_codebuddy_account_id,
    l.last_error_code,
    EXISTS (SELECT 1 FROM account_github_identity_secrets s WHERE s.account_id = a.id) AS has_secret
FROM accounts a
LEFT JOIN LATERAL (
    SELECT status, codebuddy_account_id, last_error_code
    FROM github_codebuddy_links
    WHERE github_account_id = a.id
    ORDER BY CASE WHEN status IN ('linking', 'active') THEN 0 ELSE 1 END, updated_at DESC, id DESC
    LIMIT 1
) l ON TRUE
LEFT JOIN accounts linked ON linked.id = l.codebuddy_account_id AND linked.deleted_at IS NULL
WHERE a.deleted_at IS NULL
  AND (a.platform = 'github' OR LOWER(COALESCE(a.extra->>'neonix_legacy_provider', '')) = 'github')
ORDER BY github_created_at DESC NULLS LAST, a.created_at DESC, a.id DESC`

func (r *accountRepository) ListGitHubPickerAccounts(ctx context.Context) ([]service.RegisterGitHubPickerAccount, error) {
	if r == nil || r.sql == nil {
		return nil, errors.New("account repository is not configured")
	}
	rows, err := r.sql.QueryContext(ctx, githubIdentityRowsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now().UnixMilli()
	result := make([]service.RegisterGitHubPickerAccount, 0)
	for rows.Next() {
		var internalID int64
		var item service.RegisterGitHubPickerAccount
		var createdAt, eligibleAt sql.NullInt64
		var linkStatus, linkedID, lastError sql.NullString
		var hasSecret bool
		if err := rows.Scan(&internalID, &item.ID, &item.Email, &item.Username, &item.Status, &item.Enabled, &createdAt, &eligibleAt, &linkStatus, &linkedID, &lastError, &hasSecret); err != nil {
			return nil, err
		}
		if createdAt.Valid {
			value := createdAt.Int64
			item.GitHubCreatedAt = &value
			age := (now - value) / int64(24*time.Hour/time.Millisecond)
			if age < 0 {
				age = 0
			}
			item.AgeDays = &age
		}
		if eligibleAt.Valid {
			value := eligibleAt.Int64
			item.GitHubEligibleAt = &value
		}
		if linkStatus.Valid {
			item.LinkStatus = &linkStatus.String
		}
		if linkedID.Valid {
			item.LinkedCodeBuddyAccountID = &linkedID.String
		}
		if lastError.Valid {
			item.LastError = lastError.String
		}
		item.Eligible = item.Status == service.StatusActive && item.Enabled && hasSecret && eligibleAt.Valid && eligibleAt.Int64 <= now && (!linkStatus.Valid || linkStatus.String == "failed" || linkStatus.String == "revoked")
		result = append(result, item)
	}
	return result, rows.Err()
}

type githubReservationRow struct {
	internalID int64
	publicID   string
	email      string
	username   string
	status     string
	enabled    bool
	createdAt  sql.NullInt64
	eligibleAt sql.NullInt64
	linkStatus sql.NullString
	envelope   string
}

func (r *accountRepository) ReserveGitHubAccounts(ctx context.Context, ids []string, jobID string) ([]service.PythonRegisterAccount, error) {
	unique, err := normalizeGitHubPickerIDs(ids)
	if err != nil || !githubLinkJobIDPattern.MatchString(jobID) {
		return nil, errors.New("invalid GitHub reservation request")
	}
	if r == nil || r.sql == nil || r.credentialCodec == nil || r.credentialCodecErr != nil {
		return nil, errors.New("GitHub identity secret storage is not configured")
	}
	beginner, ok := r.sql.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return nil, errors.New("GitHub identity transaction storage is not configured")
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
SELECT
    a.id, COALESCE(NULLIF(BTRIM(a.extra->>'neonix_legacy_account_id'), ''), a.id::text),
    COALESCE(NULLIF(BTRIM(a.extra->>'neonix_legacy_email'), ''), NULLIF(BTRIM(a.credentials->>'email'), ''), ''),
    COALESCE(NULLIF(BTRIM(a.credentials->>'github_username'), ''), NULLIF(BTRIM(a.credentials->>'githubUsername'), ''), ''),
    a.status, CASE WHEN LOWER(COALESCE(a.extra->>'neonix_legacy_enabled', 'true')) = 'false' THEN FALSE ELSE TRUE END,
    CASE WHEN COALESCE(a.extra->>'github_created_at_ms', '') ~ '^[0-9]+$' THEN (a.extra->>'github_created_at_ms')::bigint END,
    CASE WHEN COALESCE(a.extra->>'github_eligible_at_ms', '') ~ '^[0-9]+$' THEN (a.extra->>'github_eligible_at_ms')::bigint END,
    l.status, s.secret_envelope
FROM accounts a
JOIN account_github_identity_secrets s ON s.account_id = a.id
LEFT JOIN github_codebuddy_links l ON l.github_account_id = a.id AND l.status IN ('linking', 'active')
WHERE a.deleted_at IS NULL
  AND (a.platform = 'github' OR LOWER(COALESCE(a.extra->>'neonix_legacy_provider', '')) = 'github')
  AND (a.id::text = ANY($1) OR a.extra->>'neonix_legacy_account_id' = ANY($1))
ORDER BY a.id
FOR UPDATE OF a`, pq.Array(unique))
	if err != nil {
		return nil, err
	}
	byID := make(map[string]githubReservationRow, len(unique))
	for rows.Next() {
		var row githubReservationRow
		if err := rows.Scan(&row.internalID, &row.publicID, &row.email, &row.username, &row.status, &row.enabled, &row.createdAt, &row.eligibleAt, &row.linkStatus, &row.envelope); err != nil {
			_ = rows.Close()
			return nil, err
		}
		byID[row.publicID] = row
		byID[fmt.Sprint(row.internalID)] = row
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	now := time.Now().UnixMilli()
	result := make([]service.PythonRegisterAccount, 0, len(unique))
	selectedInternal := make(map[int64]struct{}, len(unique))
	for _, id := range unique {
		row, exists := byID[id]
		if !exists || row.status != service.StatusActive || !row.enabled || !row.eligibleAt.Valid || row.eligibleAt.Int64 > now || row.linkStatus.Valid {
			return nil, errors.New("GitHub identity is unavailable")
		}
		if _, duplicate := selectedInternal[row.internalID]; duplicate {
			return nil, errors.New("duplicate GitHub identity")
		}
		selectedInternal[row.internalID] = struct{}{}
		secret, err := r.openGitHubIdentitySecret(row.envelope)
		if err != nil {
			return nil, err
		}
		item := service.PythonRegisterAccount{
			ID: row.publicID, Email: row.email, Username: row.username, Password: secret.Password,
			Cookies: cloneGitHubCookies(secret.Cookies), UserAgent: secret.UserAgent, Proxy: secret.Proxy, Provider: "github",
		}
		if row.createdAt.Valid {
			item.GitHubCreatedAt = row.createdAt.Int64
		}
		if row.eligibleAt.Valid {
			item.GitHubEligibleAt = row.eligibleAt.Int64
		}
		result = append(result, item)
	}
	for accountID := range selectedInternal {
		if _, err := tx.ExecContext(ctx, `INSERT INTO github_codebuddy_links (github_account_id, job_id, status) VALUES ($1, $2, 'linking')`, accountID, jobID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *accountRepository) CompleteGitHubLink(ctx context.Context, publicGitHubID string, codeBuddyAccountID int64, jobID string, update *service.RegisterGitHubSessionUpdate) error {
	if strings.TrimSpace(publicGitHubID) == "" || codeBuddyAccountID <= 0 || !githubLinkJobIDPattern.MatchString(jobID) {
		return errors.New("invalid GitHub link completion")
	}
	if r == nil || r.sql == nil || r.credentialCodec == nil || r.credentialCodecErr != nil {
		return errors.New("GitHub identity secret storage is not configured")
	}
	beginner, ok := r.sql.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return errors.New("GitHub identity transaction storage is not configured")
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var githubID int64
	var envelope string
	err = tx.QueryRowContext(ctx, `
SELECT a.id, s.secret_envelope
FROM accounts a
JOIN account_github_identity_secrets s ON s.account_id = a.id
JOIN github_codebuddy_links l ON l.github_account_id = a.id
WHERE a.deleted_at IS NULL
  AND (a.id::text = $1 OR a.extra->>'neonix_legacy_account_id' = $1)
  AND l.job_id = $2 AND l.status = 'linking'
FOR UPDATE OF a, l`, strings.TrimSpace(publicGitHubID), jobID).Scan(&githubID, &envelope)
	if err != nil {
		return err
	}
	var platform string
	if err := tx.QueryRowContext(ctx, `SELECT platform FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, codeBuddyAccountID).Scan(&platform); err != nil || platform != service.PlatformCodeBuddy {
		return errors.New("CodeBuddy account is unavailable")
	}
	if update != nil && (len(update.Cookies) > 0 || strings.TrimSpace(update.UserAgent) != "") {
		secret, err := r.openGitHubIdentitySecret(envelope)
		if err != nil {
			return err
		}
		if len(update.Cookies) > 0 {
			secret.Cookies = cloneGitHubCookies(update.Cookies)
		}
		if strings.TrimSpace(update.UserAgent) != "" {
			secret.UserAgent = update.UserAgent
		}
		sealed, err := r.sealGitHubIdentitySecret(secret)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_github_identity_secrets SET secret_envelope = $2, updated_at = NOW() WHERE account_id = $1`, githubID, sealed); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `
UPDATE github_codebuddy_links
SET codebuddy_account_id = $1, status = 'active', last_error_code = NULL, updated_at = NOW()
WHERE github_account_id = $2 AND job_id = $3 AND status = 'linking'`, codeBuddyAccountID, githubID, jobID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return errors.New("GitHub identity reservation is missing")
	}
	return tx.Commit()
}

func (r *accountRepository) ReleaseGitHubReservations(ctx context.Context, jobID, email, reasonCode string) error {
	if r == nil || r.sql == nil || !githubLinkJobIDPattern.MatchString(jobID) {
		return errors.New("invalid GitHub reservation release")
	}
	reasonCode = normalizeGitHubFailureCode(reasonCode)
	query := `
UPDATE github_codebuddy_links l
SET status = 'failed', last_error_code = $2, updated_at = NOW()
WHERE l.job_id = $1 AND l.status = 'linking'`
	args := []any{jobID, reasonCode}
	if email = strings.TrimSpace(email); email != "" {
		query += ` AND EXISTS (SELECT 1 FROM accounts a WHERE a.id = l.github_account_id AND LOWER(COALESCE(a.extra->>'neonix_legacy_email', a.credentials->>'email', '')) = LOWER($3))`
		args = append(args, email)
	}
	_, err := r.sql.ExecContext(ctx, query, args...)
	return err
}

func (r *accountRepository) StoreGitHubIdentitySecret(ctx context.Context, accountID int64, secret service.RegisterGitHubIdentitySecret) error {
	if r == nil || r.sql == nil || accountID <= 0 {
		return errors.New("GitHub identity secret storage is not configured")
	}
	envelope, err := r.sealGitHubIdentitySecret(secret)
	if err != nil {
		return err
	}
	_, err = r.sql.ExecContext(ctx, `
INSERT INTO account_github_identity_secrets (account_id, secret_envelope)
VALUES ($1, $2)
ON CONFLICT (account_id) DO UPDATE
SET secret_envelope = EXCLUDED.secret_envelope, updated_at = NOW()`, accountID, envelope)
	return err
}

func (r *accountRepository) RevealLinkedGitHubIdentityPassword(ctx context.Context, codeBuddyAccountID int64) (string, error) {
	if r == nil || r.sql == nil || r.credentialCodec == nil || r.credentialCodecErr != nil || codeBuddyAccountID <= 0 {
		return "", errors.New("GitHub identity secret storage is not configured")
	}
	var envelope string
	rows, err := r.sql.QueryContext(ctx, `
SELECT s.secret_envelope
FROM github_codebuddy_links l
JOIN accounts github ON github.id = l.github_account_id AND github.deleted_at IS NULL
JOIN accounts codebuddy ON codebuddy.id = l.codebuddy_account_id AND codebuddy.deleted_at IS NULL
JOIN account_github_identity_secrets s ON s.account_id = github.id
WHERE l.codebuddy_account_id = $1
  AND l.status = 'active'
  AND codebuddy.platform = $2
ORDER BY l.updated_at DESC, l.id DESC
LIMIT 1`, codeBuddyAccountID, service.PlatformCodeBuddy)
	if errors.Is(err, sql.ErrNoRows) {
		return "", service.ErrLinkedGitHubIdentityNotFound
	}
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", service.ErrLinkedGitHubIdentityNotFound
	}
	if err := rows.Scan(&envelope); err != nil {
		return "", err
	}
	secret, err := r.openGitHubIdentitySecret(envelope)
	if err != nil {
		return "", err
	}
	return secret.Password, nil
}

func (r *accountRepository) openGitHubIdentitySecret(envelope string) (service.RegisterGitHubIdentitySecret, error) {
	if r == nil || r.credentialCodec == nil {
		return service.RegisterGitHubIdentitySecret{}, errors.New("GitHub identity secret storage is not configured")
	}
	raw, err := r.credentialCodec.Open(envelope)
	if err != nil {
		return service.RegisterGitHubIdentitySecret{}, errors.New("GitHub identity secret cannot be opened")
	}
	var secret service.RegisterGitHubIdentitySecret
	if json.Unmarshal(raw, &secret) != nil || !validGitHubIdentitySecret(secret) {
		return service.RegisterGitHubIdentitySecret{}, errors.New("GitHub identity secret is invalid")
	}
	return secret, nil
}

func (r *accountRepository) sealGitHubIdentitySecret(secret service.RegisterGitHubIdentitySecret) (string, error) {
	if r == nil || r.credentialCodec == nil || !validGitHubIdentitySecret(secret) {
		return "", errors.New("GitHub identity secret is invalid")
	}
	raw, err := json.Marshal(secret)
	if err != nil || len(raw) > 1<<20 {
		return "", errors.New("GitHub identity secret is invalid")
	}
	return r.credentialCodec.Seal(raw)
}

func validGitHubIdentitySecret(secret service.RegisterGitHubIdentitySecret) bool {
	if strings.TrimSpace(secret.Password) == "" || len(secret.Password) > 4096 || len(secret.Cookies) == 0 || len(secret.Cookies) > 512 || len(secret.UserAgent) > 2048 || len(secret.Proxy) > 2048 {
		return false
	}
	for _, cookie := range secret.Cookies {
		name, nameOK := cookie["name"].(string)
		value, valueOK := cookie["value"].(string)
		if !nameOK || !valueOK || strings.TrimSpace(name) == "" || len(name) > 256 || len(value) > 16384 {
			return false
		}
	}
	return true
}

func normalizeGitHubPickerIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > 20000 {
		return nil, errors.New("invalid GitHub identity selection")
	}
	result := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 256 {
			return nil, errors.New("invalid GitHub identity selection")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) == 0 {
		return nil, errors.New("invalid GitHub identity selection")
	}
	return result, nil
}

func cloneGitHubCookies(cookies []map[string]any) []map[string]any {
	result := make([]map[string]any, len(cookies))
	for i := range cookies {
		result[i] = make(map[string]any, len(cookies[i]))
		for key, value := range cookies[i] {
			result[i][key] = value
		}
	}
	return result
}

func normalizeGitHubFailureCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if !regexp.MustCompile(`^[A-Z0-9_]{1,80}$`).MatchString(value) {
		return "REGISTER_GITHUB_LINK_FAILED"
	}
	return value
}
