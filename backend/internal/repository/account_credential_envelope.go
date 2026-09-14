package repository

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/lib/pq"
	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
)

const accountCredentialEnvelopeSelectSQL = `
SELECT account_id, envelope
FROM account_credential_envelopes
WHERE account_id = ANY($1)`

// credentialEnvelopeFromEnvironment loads the key shared by the cutover
// importer and the Go runtime. An unset value keeps the compatibility column
// available; a malformed value is returned as an error so the runtime fails
// closed instead of silently serving a plaintext credential path.
func credentialEnvelopeFromEnvironment() (*credentials.Envelope, error) {
	raw := strings.TrimSpace(os.Getenv("NEONIX_CREDENTIAL_KEY"))
	if raw == "" {
		return nil, nil
	}
	if len(raw) != 64 {
		return nil, errors.New("credential envelope key must be a 64-character hex value")
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, errors.New("credential envelope key is not valid hex")
	}
	codec, err := credentials.New(key)
	if err != nil {
		return nil, errors.New("credential envelope key is invalid")
	}
	return codec, nil
}

func loadAccountCredentialEnvelopes(ctx context.Context, exec sqlExecutor, accountIDs []int64) (map[int64]string, error) {
	result := make(map[int64]string)
	if exec == nil || len(accountIDs) == 0 {
		return result, nil
	}
	unique := make([]int64, 0, len(accountIDs))
	seen := make(map[int64]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	for start := 0; start < len(unique); start += postgresParameterBatchSize {
		end := start + postgresParameterBatchSize
		if end > len(unique) {
			end = len(unique)
		}
		rows, err := exec.QueryContext(ctx, accountCredentialEnvelopeSelectSQL, pq.Array(unique[start:end]))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var envelope string
			if err := rows.Scan(&id, &envelope); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if strings.TrimSpace(envelope) != "" {
				result[id] = envelope
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func openAccountCredentialEnvelope(codec *credentials.Envelope, envelope string) (map[string]any, error) {
	if codec == nil || strings.TrimSpace(envelope) == "" {
		return nil, errors.New("credential envelope is not configured")
	}
	plaintext, err := codec.Open(envelope)
	if err != nil {
		return nil, errors.New("credential envelope cannot be opened")
	}
	var values map[string]any
	if err := json.Unmarshal(plaintext, &values); err != nil || values == nil {
		return nil, errors.New("credential envelope payload is invalid")
	}
	return values, nil
}

func sealAccountCredentials(codec *credentials.Envelope, values map[string]any) (string, error) {
	if codec == nil {
		return "", errors.New("credential envelope is not configured")
	}
	payload, err := json.Marshal(normalizeJSONMap(values))
	if err != nil {
		return "", fmt.Errorf("marshal credential payload: %w", err)
	}
	return codec.Seal(payload)
}

func persistAccountCredentialEnvelope(ctx context.Context, exec sqlExecutor, accountID int64, envelope string) error {
	if exec == nil || accountID <= 0 || strings.TrimSpace(envelope) == "" {
		return errors.New("credential envelope persistence is not configured")
	}
	_, err := exec.ExecContext(ctx, `
INSERT INTO account_credential_envelopes (account_id, envelope, key_version)
VALUES ($1, $2, 1)
ON CONFLICT (account_id) DO UPDATE
SET envelope = EXCLUDED.envelope,
    key_version = EXCLUDED.key_version,
    updated_at = NOW()`, accountID, envelope)
	return err
}

func syncAccountCredentialEnvelopes(ctx context.Context, exec sqlExecutor, codec *credentials.Envelope, accountIDs []int64) error {
	if codec == nil || exec == nil || len(accountIDs) == 0 {
		return nil
	}
	unique := make([]int64, 0, len(accountIDs))
	seen := make(map[int64]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	type row struct {
		id          int64
		credentials []byte
	}
	rowsToSeal := make([]row, 0, len(unique))
	for start := 0; start < len(unique); start += postgresParameterBatchSize {
		end := start + postgresParameterBatchSize
		if end > len(unique) {
			end = len(unique)
		}
		rows, err := exec.QueryContext(ctx, `SELECT id, credentials FROM accounts WHERE id = ANY($1) AND deleted_at IS NULL`, pq.Array(unique[start:end]))
		if err != nil {
			return err
		}
		for rows.Next() {
			var item row
			if err := rows.Scan(&item.id, &item.credentials); err != nil {
				_ = rows.Close()
				return err
			}
			rowsToSeal = append(rowsToSeal, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	for _, item := range rowsToSeal {
		var values map[string]any
		if len(item.credentials) == 0 || string(item.credentials) == "null" {
			values = map[string]any{}
		} else if err := json.Unmarshal(item.credentials, &values); err != nil || values == nil {
			return errors.New("account credentials JSON is invalid")
		}
		envelope, err := sealAccountCredentials(codec, values)
		if err != nil {
			return err
		}
		if err := persistAccountCredentialEnvelope(ctx, exec, item.id, envelope); err != nil {
			return err
		}
	}
	return nil
}

// compile-time assertion keeps the helper usable with *sql.DB and *sql.Tx.
var _ sqlExecutor = (*sql.DB)(nil)

func (r *accountRepository) credentialEnvelopeConfigError() error {
	if r == nil || r.credentialCodecErr == nil {
		return nil
	}
	return errors.New("credential envelope configuration is invalid")
}
