package routes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/provider/opencode"
)

type neonixModelCatalog struct {
	db         *sql.DB
	httpClient *http.Client
}

func modelPathID(c *gin.Context) string {
	return strings.TrimPrefix(strings.TrimSpace(c.Param("id")), "/")
}

type neonixModelInput struct {
	ID                    string         `json:"id"`
	Name                  string         `json:"name"`
	Description           string         `json:"description"`
	Provider              string         `json:"provider"`
	Source                string         `json:"source"`
	Status                string         `json:"status"`
	MaxInputTokens        *int64         `json:"maxInputTokens"`
	MaxOutputTokens       *int64         `json:"maxOutputTokens"`
	SupportsToolCalling   *bool          `json:"supportsToolCalling"`
	SupportsPromptCaching *bool          `json:"supportsPromptCaching"`
	SupportedInputTypes   []string       `json:"supportedInputTypes"`
	ComboRoutes           []string       `json:"comboRoutes"`
	RawData               map[string]any `json:"rawData"`
}

type neonixModel struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Description    string         `json:"description,omitempty"`
	Provider       string         `json:"provider"`
	Source         string         `json:"source"`
	Status         string         `json:"status"`
	IsDeleted      bool           `json:"isDeleted"`
	UpdatedByAdmin bool           `json:"updatedByAdmin"`
	RawData        map[string]any `json:"-"`
	UpdatedAt      int64          `json:"updatedAt"`
}

func (m neonixModel) MarshalJSON() ([]byte, error) {
	type plain neonixModel
	out := map[string]any{}
	b, _ := json.Marshal(plain(m))
	_ = json.Unmarshal(b, &out)
	for key, value := range m.RawData {
		if _, reserved := out[key]; !reserved {
			out[key] = value
		}
	}
	if limits, ok := m.RawData["tokenLimits"].(map[string]any); ok {
		if value, exists := limits["maxInputTokens"]; exists {
			out["maxInputTokens"] = value
		}
		if value, exists := limits["maxOutputTokens"]; exists && value != nil {
			out["maxOutputTokens"] = value
		}
	}
	if _, exists := out["maxOutputTokens"]; !exists {
		if value, ok := m.RawData["max_output_tokens"]; ok {
			out["maxOutputTokens"] = value
		}
	}
	if caching, ok := m.RawData["promptCaching"].(map[string]any); ok {
		if value, exists := caching["supportsPromptCaching"]; exists {
			out["supportsPromptCaching"] = value
		}
	} else if value, exists := m.RawData["supports_caching"]; exists {
		out["supportsPromptCaching"] = value
	}
	return json.Marshal(out)
}

func (s *neonixModelCatalog) list(c *gin.Context) {
	if s == nil || s.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Model catalog is unavailable", "errorCode": "MODEL_CATALOG_UNAVAILABLE"})
		return
	}
	rows, err := s.db.QueryContext(c.Request.Context(), `SELECT model_id, model_name, description, provider, source, status, is_deleted, updated_by_admin, raw_data, updated_at FROM neonix_model_catalog WHERE is_deleted = FALSE ORDER BY model_name`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load models", "errorCode": "MODEL_LIST_FAILED"})
		return
	}
	defer rows.Close()
	models := make([]neonixModel, 0)
	for rows.Next() {
		var m neonixModel
		var raw []byte
		var updated time.Time
		if err := rows.Scan(&m.ID, &m.Name, &m.Description, &m.Provider, &m.Source, &m.Status, &m.IsDeleted, &m.UpdatedByAdmin, &raw, &updated); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load models", "errorCode": "MODEL_LIST_FAILED"})
			return
		}
		_ = json.Unmarshal(raw, &m.RawData)
		m.UpdatedAt = updated.UnixMilli()
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load models", "errorCode": "MODEL_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, models)
}

func normalizeModelInput(input *neonixModelInput) error {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Provider = strings.TrimSpace(input.Provider)
	input.Source = strings.TrimSpace(input.Source)
	input.Status = strings.ToUpper(strings.TrimSpace(input.Status))
	if input.ID == "" {
		return errModelIDRequired
	}
	if input.Name == "" {
		input.Name = input.ID
	}
	if input.Provider == "" {
		input.Provider = "kiro"
	}
	if input.Source == "" {
		input.Source = "manual"
	}
	if input.Status == "" {
		input.Status = "AVAILABLE"
	}
	if input.Status != "AVAILABLE" && input.Status != "MAINTENANCE" {
		return errModelStatusInvalid
	}
	if input.RawData == nil {
		input.RawData = map[string]any{}
	}
	input.RawData["modelProvider"] = input.Provider
	input.RawData["status"] = input.Status
	if input.MaxInputTokens != nil {
		input.RawData["tokenLimits"] = map[string]any{"maxInputTokens": *input.MaxInputTokens, "maxOutputTokens": input.MaxOutputTokens}
	}
	if input.MaxOutputTokens != nil {
		input.RawData["maxOutputTokens"] = *input.MaxOutputTokens
	}
	if input.SupportsToolCalling != nil {
		input.RawData["supportsToolCalling"] = *input.SupportsToolCalling
	}
	if input.SupportsPromptCaching != nil {
		input.RawData["supports_caching"] = *input.SupportsPromptCaching
	}
	if input.SupportedInputTypes != nil {
		input.RawData["supportedInputTypes"] = input.SupportedInputTypes
	}
	if input.ComboRoutes != nil {
		input.RawData["comboRoutes"] = input.ComboRoutes
	}
	return nil
}

var errModelIDRequired = &modelValidationError{"MODEL_ID_REQUIRED", "Model ID is required"}
var errModelStatusInvalid = &modelValidationError{"MODEL_STATUS_INVALID", "Model status must be AVAILABLE or MAINTENANCE"}

type modelValidationError struct{ code, message string }

func (e *modelValidationError) Error() string { return e.message }

func (s *neonixModelCatalog) create(c *gin.Context) { s.save(c, false) }
func (s *neonixModelCatalog) update(c *gin.Context) { s.save(c, true) }
func (s *neonixModelCatalog) save(c *gin.Context, update bool) {
	if s == nil || s.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Model catalog is unavailable", "errorCode": "MODEL_CATALOG_UNAVAILABLE"})
		return
	}
	var input neonixModelInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid model payload", "errorCode": "MODEL_PAYLOAD_INVALID"})
		return
	}
	if update {
		input.ID = modelPathID(c)
		var exists bool
		var existingRaw []byte
		var existingName, existingDescription, existingProvider, existingSource, existingStatus string
		if err := s.db.QueryRowContext(c.Request.Context(), `SELECT EXISTS(SELECT 1 FROM neonix_model_catalog WHERE model_id=$1), COALESCE((SELECT model_name FROM neonix_model_catalog WHERE model_id=$1), ''), COALESCE((SELECT description FROM neonix_model_catalog WHERE model_id=$1), ''), COALESCE((SELECT provider FROM neonix_model_catalog WHERE model_id=$1), ''), COALESCE((SELECT source FROM neonix_model_catalog WHERE model_id=$1), ''), COALESCE((SELECT status FROM neonix_model_catalog WHERE model_id=$1), ''), COALESCE((SELECT raw_data FROM neonix_model_catalog WHERE model_id=$1), '{}'::jsonb)`, input.ID).Scan(&exists, &existingName, &existingDescription, &existingProvider, &existingSource, &existingStatus, &existingRaw); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load model", "errorCode": "MODEL_LOAD_FAILED"})
			return
		}
		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "Model not found", "errorCode": "MODEL_NOT_FOUND"})
			return
		}
		if strings.TrimSpace(input.Name) == "" {
			input.Name = existingName
		}
		if input.Description == "" {
			input.Description = existingDescription
		}
		if strings.TrimSpace(input.Provider) == "" {
			input.Provider = existingProvider
		}
		if strings.TrimSpace(input.Source) == "" {
			input.Source = existingSource
		}
		if strings.TrimSpace(input.Status) == "" {
			input.Status = existingStatus
		}
		merged := map[string]any{}
		if err := json.Unmarshal(existingRaw, &merged); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load model", "errorCode": "MODEL_LOAD_FAILED"})
			return
		}
		for key, value := range input.RawData {
			merged[key] = value
		}
		input.RawData = merged
	}
	if err := normalizeModelInput(&input); err != nil {
		validation := err.(*modelValidationError)
		c.JSON(http.StatusBadRequest, gin.H{"error": validation.message, "errorCode": validation.code})
		return
	}
	raw, _ := json.Marshal(input.RawData)
	_, err := s.db.ExecContext(c.Request.Context(), `INSERT INTO neonix_model_catalog (model_id, model_name, description, provider, source, status, raw_data, updated_by_admin, is_deleted, deleted_at) VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,FALSE,NULL) ON CONFLICT (model_id) DO UPDATE SET model_name=EXCLUDED.model_name, description=EXCLUDED.description, provider=EXCLUDED.provider, source=EXCLUDED.source, status=EXCLUDED.status, raw_data=EXCLUDED.raw_data, updated_by_admin=TRUE, is_deleted=FALSE, deleted_at=NULL, updated_at=NOW()`, input.ID, input.Name, input.Description, input.Provider, input.Source, input.Status, raw)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save model", "errorCode": "MODEL_SAVE_FAILED"})
		return
	}
	responseStatus := http.StatusCreated
	if update {
		responseStatus = http.StatusOK
	}
	s.get(c, input.ID, responseStatus)
}

func (s *neonixModelCatalog) get(c *gin.Context, id string, status int) {
	var m neonixModel
	var raw []byte
	var updated time.Time
	err := s.db.QueryRowContext(c.Request.Context(), `SELECT model_id, model_name, description, provider, source, status, is_deleted, updated_by_admin, raw_data, updated_at FROM neonix_model_catalog WHERE model_id=$1`, id).Scan(&m.ID, &m.Name, &m.Description, &m.Provider, &m.Source, &m.Status, &m.IsDeleted, &m.UpdatedByAdmin, &raw, &updated)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Model not found", "errorCode": "MODEL_NOT_FOUND"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load model", "errorCode": "MODEL_LOAD_FAILED"})
		return
	}
	_ = json.Unmarshal(raw, &m.RawData)
	m.UpdatedAt = updated.UnixMilli()
	c.JSON(status, m)
}

func (s *neonixModelCatalog) delete(c *gin.Context) {
	if s == nil || s.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Model catalog is unavailable", "errorCode": "MODEL_CATALOG_UNAVAILABLE"})
		return
	}
	result, err := s.db.ExecContext(c.Request.Context(), `UPDATE neonix_model_catalog SET is_deleted=TRUE, updated_by_admin=TRUE, deleted_at=NOW(), updated_at=NOW() WHERE model_id=$1 AND is_deleted=FALSE`, modelPathID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete model", "errorCode": "MODEL_DELETE_FAILED"})
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Model not found", "errorCode": "MODEL_NOT_FOUND"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type openCodeSyncResult struct {
	Provider  string `json:"provider"`
	Upserted  int    `json:"upserted"`
	Removed   int    `json:"removed"`
	Preserved int    `json:"preserved"`
	Bootstrap bool   `json:"bootstrap"`
	Warning   string `json:"warning,omitempty"`
}

func (s *neonixModelCatalog) syncOpenCode(c *gin.Context) {
	if s == nil || s.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Model catalog is unavailable", "errorCode": "MODEL_CATALOG_UNAVAILABLE"})
		return
	}
	models, err := opencode.FetchFreeModels(c.Request.Context(), s.httpClient)
	bootstrap := false
	warning := ""
	if err != nil {
		hasRows, countErr := s.hasOpenCodeRows(c.Request.Context())
		if countErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to inspect model catalog", "errorCode": "MODEL_SYNC_FAILED"})
			return
		}
		if hasRows {
			c.JSON(http.StatusBadGateway, gin.H{"error": "OpenCode Zen model catalog is temporarily unavailable; existing models were preserved", "errorCode": "MODEL_SYNC_UPSTREAM_UNAVAILABLE"})
			return
		}
		models = opencode.FallbackModels()
		bootstrap = true
		warning = "OpenCode Zen upstream was unavailable; bootstrapped the built-in free catalog"
	}
	result, err := s.persistOpenCodeModels(c.Request.Context(), models, bootstrap)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to synchronize OpenCode Zen models", "errorCode": "MODEL_SYNC_FAILED"})
		return
	}
	result.Warning = warning
	c.JSON(http.StatusOK, result)
}

func (s *neonixModelCatalog) hasOpenCodeRows(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM neonix_model_catalog WHERE provider='oc')`).Scan(&exists)
	return exists, err
}

func (s *neonixModelCatalog) persistOpenCodeModels(ctx context.Context, models []opencode.FreeModel, bootstrap bool) (openCodeSyncResult, error) {
	result := openCodeSyncResult{Provider: "oc", Bootstrap: bootstrap}
	if len(models) == 0 {
		return result, errors.New("empty OpenCode Zen catalog")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	activeIDs := make([]string, 0, len(models))
	for _, model := range models {
		modelID := "oc/" + strings.TrimPrefix(strings.TrimSpace(model.ID), "oc/")
		actualID := strings.TrimPrefix(strings.TrimSpace(model.ID), "oc/")
		if actualID == "" {
			return result, errors.New("empty OpenCode Zen model ID")
		}
		raw, marshalErr := json.Marshal(map[string]any{
			"id": modelID, "modelProvider": "oc", "actualModelId": actualID,
			"sortOrder": model.SortOrder, "supportedInputTypes": []string{"text"},
			"ownedBy": model.OwnedBy, "remote": model.Remote,
			"tokenLimits": map[string]any{"maxInputTokens": 64000, "maxOutputTokens": 8192},
		})
		if marshalErr != nil {
			return result, marshalErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO neonix_model_catalog (model_id, model_name, description, provider, source, status, raw_data, updated_by_admin, is_deleted, deleted_at) VALUES ($1,$2,$3,'oc','oc','AVAILABLE',$4,FALSE,FALSE,NULL) ON CONFLICT (model_id) DO UPDATE SET model_name=CASE WHEN neonix_model_catalog.updated_by_admin THEN neonix_model_catalog.model_name ELSE EXCLUDED.model_name END, description=CASE WHEN neonix_model_catalog.updated_by_admin THEN neonix_model_catalog.description ELSE EXCLUDED.description END, provider=CASE WHEN neonix_model_catalog.updated_by_admin THEN neonix_model_catalog.provider ELSE 'oc' END, source=CASE WHEN neonix_model_catalog.updated_by_admin THEN neonix_model_catalog.source ELSE 'oc' END, status=CASE WHEN neonix_model_catalog.updated_by_admin THEN neonix_model_catalog.status ELSE 'AVAILABLE' END, raw_data=CASE WHEN neonix_model_catalog.updated_by_admin THEN neonix_model_catalog.raw_data ELSE EXCLUDED.raw_data END, is_deleted=CASE WHEN neonix_model_catalog.updated_by_admin THEN neonix_model_catalog.is_deleted ELSE FALSE END, deleted_at=CASE WHEN neonix_model_catalog.updated_by_admin THEN neonix_model_catalog.deleted_at ELSE NULL END, updated_at=CASE WHEN neonix_model_catalog.updated_by_admin THEN neonix_model_catalog.updated_at ELSE NOW() END`, modelID, model.Name, model.Description, raw); err != nil {
			return result, err
		}
		activeIDs = append(activeIDs, modelID)
		result.Upserted++
	}
	rows, err := tx.QueryContext(ctx, `SELECT model_id, updated_by_admin FROM neonix_model_catalog WHERE provider='oc' AND is_deleted=FALSE`)
	if err != nil {
		return result, err
	}
	type catalogState struct {
		id    string
		admin bool
	}
	states := make([]catalogState, 0)
	for rows.Next() {
		var state catalogState
		if err := rows.Scan(&state.id, &state.admin); err != nil {
			_ = rows.Close()
			return result, err
		}
		states = append(states, state)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	active := make(map[string]struct{}, len(activeIDs))
	for _, id := range activeIDs {
		active[id] = struct{}{}
	}
	for _, state := range states {
		if _, found := active[state.id]; found {
			continue
		}
		if state.admin {
			result.Preserved++
			continue
		}
		updated, updateErr := tx.ExecContext(ctx, `UPDATE neonix_model_catalog SET is_deleted=TRUE, deleted_at=NOW(), updated_at=NOW() WHERE model_id=$1 AND provider='oc' AND updated_by_admin=FALSE AND is_deleted=FALSE`, state.id)
		if updateErr != nil {
			return result, updateErr
		}
		count, _ := updated.RowsAffected()
		result.Removed += int(count)
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}
