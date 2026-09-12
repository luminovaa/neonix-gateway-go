package legacy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ModelCatalogRow is the non-secret models_cache export shape. RawData only
// contains model capabilities, limits, routing metadata, and display state.
type ModelCatalogRow struct {
	ModelID        string          `json:"modelId"`
	ModelName      string          `json:"modelName"`
	Description    string          `json:"description"`
	RequiresPro    bool            `json:"requiresPro"`
	RawData        json.RawMessage `json:"rawData"`
	UpdatedAt      int64           `json:"updatedAt"`
	Provider       string          `json:"provider"`
	Source         string          `json:"source"`
	IsDeleted      bool            `json:"isDeleted"`
	UpdatedByAdmin bool            `json:"updatedByAdmin"`
	CreatedAt      int64           `json:"createdAt"`
	DeletedAt      *int64          `json:"deletedAt"`
}

type ModelCatalogReport struct {
	Total   int `json:"total"`
	Applied int `json:"applied"`
}

func ImportModelCatalogIntoPostgres(ctx context.Context, db *sql.DB, models []ModelCatalogRow, now func() time.Time) (ModelCatalogReport, error) {
	report := ModelCatalogReport{Total: len(models)}
	if len(models) == 0 {
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
		return report, fmt.Errorf("%w: begin model catalog transaction", ErrImportDB)
	}
	defer func() { _ = tx.Rollback() }()
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ModelID)
		if id == "" {
			return report, fmt.Errorf("%w: model ID is missing", ErrImportDB)
		}
		if _, exists := seen[id]; exists {
			return report, fmt.Errorf("%w: duplicate model ID", ErrImportDB)
		}
		seen[id] = struct{}{}
		name := strings.TrimSpace(model.ModelName)
		if name == "" {
			name = id
		}
		provider := strings.TrimSpace(model.Provider)
		if provider == "" {
			provider = "kiro"
		}
		source := strings.TrimSpace(model.Source)
		if source == "" {
			source = provider
		}
		raw := model.RawData
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		if !json.Valid(raw) {
			return report, fmt.Errorf("%w: invalid model metadata", ErrImportDB)
		}
		status := "AVAILABLE"
		var metadata map[string]any
		if json.Unmarshal(raw, &metadata) == nil {
			if value, ok := metadata["status"].(string); ok && strings.EqualFold(value, "MAINTENANCE") {
				status = "MAINTENANCE"
			}
		}
		createdAt := unixMillis(model.CreatedAt)
		if createdAt == nil {
			value := now().UTC()
			createdAt = &value
		}
		updatedAt := unixMillis(model.UpdatedAt)
		if updatedAt == nil {
			updatedAt = createdAt
		}
		deletedAt := (*time.Time)(nil)
		if model.DeletedAt != nil {
			deletedAt = unixMillis(*model.DeletedAt)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO neonix_model_catalog (model_id, model_name, description, provider, source, requires_pro, status, raw_data, updated_by_admin, is_deleted, created_at, updated_at, deleted_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (model_id) DO UPDATE SET model_name=EXCLUDED.model_name, description=EXCLUDED.description, provider=EXCLUDED.provider, source=EXCLUDED.source, requires_pro=EXCLUDED.requires_pro, status=EXCLUDED.status, raw_data=EXCLUDED.raw_data, updated_by_admin=EXCLUDED.updated_by_admin, is_deleted=EXCLUDED.is_deleted, updated_at=EXCLUDED.updated_at, deleted_at=EXCLUDED.deleted_at`, id, name, model.Description, provider, source, model.RequiresPro, status, []byte(raw), model.UpdatedByAdmin, model.IsDeleted, createdAt, updatedAt, deletedAt); err != nil {
			return report, fmt.Errorf("%w: persist model catalog", ErrImportDB)
		}
		report.Applied++
	}
	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("%w: commit model catalog transaction", ErrImportDB)
	}
	return report, nil
}
