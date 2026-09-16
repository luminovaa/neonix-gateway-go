package repository

import (
	"context"
	"strings"

	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

// GetByLegacyID resolves a text/UUID id captured from the Neonix Node store.
// The mapping table is created by migration 239; returning the normal
// not-found sentinel keeps older installations safe until that migration has
// been applied.
func (r *apiKeyRepository) GetByLegacyID(ctx context.Context, legacyID string) (*service.APIKey, error) {
	legacyID = strings.TrimSpace(legacyID)
	if r == nil || r.sql == nil || legacyID == "" {
		return nil, service.ErrAPIKeyNotFound
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT api_key_id
		FROM neonix_legacy_api_key_ids
		WHERE legacy_id = $1
		LIMIT 1`, legacyID)
	if err != nil {
		// A rolling deployment can run before migration 239 exists. Do not
		// expose a SQL table name to the UI or treat it as a credential error.
		return nil, service.ErrAPIKeyNotFound
	}
	defer rows.Close()
	if !rows.Next() {
		if rows.Err() != nil {
			return nil, service.ErrAPIKeyNotFound
		}
		return nil, service.ErrAPIKeyNotFound
	}
	var apiKeyID int64
	if err := rows.Scan(&apiKeyID); err != nil {
		return nil, service.ErrAPIKeyNotFound
	}
	if rows.Err() != nil {
		return nil, service.ErrAPIKeyNotFound
	}
	key, err := r.GetByID(ctx, apiKeyID)
	if err != nil {
		return nil, err
	}
	return key, nil
}
