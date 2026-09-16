package legacy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// NeonixSettingKeys is the intentionally small, non-secret preference surface
// used by the current Neonix web client. Provider credentials and passwords
// are excluded even if an export contains them under another key.
var NeonixSettingKeys = []string{
	"theme", "language", "privacyMode", "autoRefresh", "usageApiType",
	"useKProxyForApi", "logStreamEvents", "switchTarget", "autoSwitchThreshold",
	"autoSwitchInterval", "usagePrecision", "loginPrivateMode", "globalShortcut",
	"closeAction", "proxySettings", "autoWarmupEnabled", "response_footer_enabled",
	"response_footer_text",
}

type NormalizedSettings map[string]string

type SettingsReport struct {
	Total   int     `json:"total"`
	Applied int     `json:"applied"`
	Failed  int     `json:"failed"`
	Issues  []Issue `json:"issues,omitempty"`
}

// NormalizeSettings keeps only allowlisted keys and strips credential-shaped
// nested values from proxy preferences. The returned JSON strings are written
// verbatim to the Go settings table and are never included in a report.
func NormalizeSettings(input map[string]json.RawMessage) (NormalizedSettings, SettingsReport) {
	report := SettingsReport{Total: len(input)}
	allowed := make(map[string]struct{}, len(NeonixSettingKeys))
	for _, key := range NeonixSettingKeys {
		allowed[key] = struct{}{}
	}
	result := make(NormalizedSettings)
	for key, raw := range input {
		if _, ok := allowed[key]; !ok {
			continue
		}
		if len(raw) == 0 || !json.Valid(raw) {
			report.Failed++
			report.Issues = append(report.Issues, Issue{ID: key, Severity: "blocked", Code: "SETTING_VALUE_INVALID"})
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			report.Failed++
			report.Issues = append(report.Issues, Issue{ID: key, Severity: "blocked", Code: "SETTING_VALUE_INVALID"})
			continue
		}
		value = sanitizeSettingValue(value)
		encoded, err := json.Marshal(value)
		if err != nil {
			report.Failed++
			report.Issues = append(report.Issues, Issue{ID: key, Severity: "blocked", Code: "SETTING_VALUE_INVALID"})
			continue
		}
		result[key] = string(encoded)
		report.Applied++
	}
	return result, report
}

func sanitizeSettingValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "apikey" || normalized == "apikeys" || normalized == "password" || normalized == "token" || normalized == "secret" || normalized == "key" || normalized == "cert" {
				continue
			}
			out[key] = sanitizeSettingValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, sanitizeSettingValue(item))
		}
		return out
	default:
		return value
	}
}

// ImportSettingsIntoPostgres applies settings in a single transaction. It is
// separate from account/key import so a failed preference write cannot mutate
// either credential store; the command reports each phase explicitly.
func ImportSettingsIntoPostgres(ctx context.Context, db *sql.DB, settings NormalizedSettings, now func() time.Time) (SettingsReport, error) {
	report := SettingsReport{Total: len(settings)}
	if len(settings) == 0 {
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
		return report, fmt.Errorf("%w: begin settings transaction", ErrImportDB)
	}
	defer func() { _ = tx.Rollback() }()
	for key, value := range settings {
		if len(key) > 100 {
			return report, fmt.Errorf("%w: setting key too long", ErrImportDB)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, updated_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`, key, value, now().UTC()); err != nil {
			return report, fmt.Errorf("%w: persist setting", ErrImportDB)
		}
		report.Applied++
	}
	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("%w: commit settings transaction", ErrImportDB)
	}
	return report, nil
}
