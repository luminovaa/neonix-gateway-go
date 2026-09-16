package legacy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type APIKeyUsage struct {
	ID           int64   `json:"id"`
	APIKeyID     string  `json:"apiKeyId"`
	Timestamp    int64   `json:"timestamp"`
	Model        string  `json:"model"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	Credits      float64 `json:"credits"`
	Success      bool    `json:"success"`
}

type APIKeyAccessLog struct {
	ID         int64  `json:"id"`
	APIKeyID   string `json:"apiKeyId"`
	IPAddress  string `json:"ipAddress"`
	UserAgent  string `json:"userAgent"`
	AccessedAt int64  `json:"accessedAt"`
}

type APIKeyHistoryReport struct {
	UsageTotal    int `json:"usageTotal"`
	UsageApplied  int `json:"usageApplied"`
	AccessTotal   int `json:"accessTotal"`
	AccessApplied int `json:"accessApplied"`
}

func ImportAPIKeyHistoryIntoPostgres(ctx context.Context, db *sql.DB, usage []APIKeyUsage, access []APIKeyAccessLog) (APIKeyHistoryReport, error) {
	report := APIKeyHistoryReport{UsageTotal: len(usage), AccessTotal: len(access)}
	if len(usage) == 0 && len(access) == 0 {
		return report, nil
	}
	if db == nil {
		return report, fmt.Errorf("%w: database is nil", ErrImportDB)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("%w: begin API-key history transaction", ErrImportDB)
	}
	defer func() { _ = tx.Rollback() }()
	for _, row := range usage {
		if row.ID <= 0 || strings.TrimSpace(row.APIKeyID) == "" || row.Timestamp <= 0 || row.InputTokens < 0 || row.OutputTokens < 0 || row.Credits < 0 {
			return report, fmt.Errorf("%w: invalid API-key usage history row", ErrImportDB)
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO neonix_legacy_api_key_usage
			  (legacy_id, api_key_id, occurred_at, model, input_tokens, output_tokens, credits, success)
			SELECT $1, mapping.api_key_id, $2, $3, $4, $5, $6, $7
			FROM neonix_legacy_api_key_ids mapping WHERE mapping.legacy_id = $8
			ON CONFLICT (legacy_id) DO UPDATE SET
			  api_key_id = EXCLUDED.api_key_id, occurred_at = EXCLUDED.occurred_at,
			  model = EXCLUDED.model, input_tokens = EXCLUDED.input_tokens,
			  output_tokens = EXCLUDED.output_tokens, credits = EXCLUDED.credits, success = EXCLUDED.success`,
			row.ID, time.UnixMilli(row.Timestamp).UTC(), row.Model, row.InputTokens, row.OutputTokens, row.Credits, row.Success, strings.TrimSpace(row.APIKeyID))
		if err != nil {
			return report, fmt.Errorf("%w: persist API-key usage history", ErrImportDB)
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 1 {
			return report, fmt.Errorf("%w: API-key usage references an unmapped key", ErrImportDB)
		}
		report.UsageApplied++
	}
	for _, row := range access {
		ip := strings.TrimSpace(row.IPAddress)
		if row.ID <= 0 || strings.TrimSpace(row.APIKeyID) == "" || row.AccessedAt <= 0 || ip == "" {
			return report, fmt.Errorf("%w: invalid API-key access history row", ErrImportDB)
		}
		userAgent := strings.TrimSpace(row.UserAgent)
		if len(userAgent) > 512 {
			userAgent = userAgent[:512]
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO neonix_legacy_api_key_access_logs
			  (legacy_id, api_key_id, ip_address, user_agent, accessed_at)
			SELECT $1, mapping.api_key_id, $2, NULLIF($3, ''), $4
			FROM neonix_legacy_api_key_ids mapping WHERE mapping.legacy_id = $5
			ON CONFLICT (legacy_id) DO UPDATE SET
			  api_key_id = EXCLUDED.api_key_id, ip_address = EXCLUDED.ip_address,
			  user_agent = EXCLUDED.user_agent, accessed_at = EXCLUDED.accessed_at`,
			row.ID, ip, userAgent, time.UnixMilli(row.AccessedAt).UTC(), strings.TrimSpace(row.APIKeyID))
		if err != nil {
			return report, fmt.Errorf("%w: persist API-key access history", ErrImportDB)
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 1 {
			return report, fmt.Errorf("%w: API-key access references an unmapped key", ErrImportDB)
		}
		report.AccessApplied++
	}
	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("%w: commit API-key history transaction", ErrImportDB)
	}
	return report, nil
}
