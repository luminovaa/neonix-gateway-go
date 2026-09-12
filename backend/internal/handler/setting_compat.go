package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
)

var neonixCompatibilitySettingKeys = []string{
	"theme",
	"language",
	"privacyMode",
	"autoRefresh",
	"usageApiType",
	"useKProxyForApi",
	"logStreamEvents",
	"switchTarget",
	"autoSwitchThreshold",
	"autoSwitchInterval",
	"usagePrecision",
	"loginPrivateMode",
	"globalShortcut",
	"closeAction",
	"proxySettings",
	"autoWarmupEnabled",
	"response_footer_enabled",
	"response_footer_text",
}

func (h *SettingHandler) neonixSettingsService(c *gin.Context) bool {
	if h == nil || h.settingService == nil {
		response.ErrorWithDetails(c, http.StatusInternalServerError, "Settings service is not configured", "SETTINGS_SERVICE_UNAVAILABLE", nil)
		return false
	}
	return true
}

// GetNeonixSettingsCompat implements GET /api/settings with the flat JSON
// object used by the existing Neonix web app.
func (h *SettingHandler) GetNeonixSettingsCompat(c *gin.Context) {
	if !h.neonixSettingsService(c) {
		return
	}
	values, err := h.settingService.GetNeonixSettings(c.Request.Context(), neonixCompatibilitySettingKeys)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// Match the Node defaults while keeping initialization idempotent.
	if _, ok := values["response_footer_enabled"]; !ok {
		values["response_footer_enabled"] = true
		_ = h.settingService.SetNeonixSetting(c.Request.Context(), "response_footer_enabled", true)
	}
	if value, ok := values["response_footer_text"]; !ok || strings.TrimSpace(valueAsString(value)) == "" {
		values["response_footer_text"] = "Powered by Neonix"
		_ = h.settingService.SetNeonixSetting(c.Request.Context(), "response_footer_text", "Powered by Neonix")
	}
	response.Success(c, values)
}

// GetNeonixSettingCompat implements GET /api/settings/:key.
func (h *SettingHandler) GetNeonixSettingCompat(c *gin.Context) {
	if !h.neonixSettingsService(c) {
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	if key == "" || len(key) > 128 {
		response.ErrorWithDetails(c, http.StatusBadRequest, "Invalid setting key", "SETTING_KEY_INVALID", nil)
		return
	}
	value, err := h.settingService.GetNeonixSetting(c.Request.Context(), key)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"key": key, "value": value})
}

// SetNeonixSettingCompat implements PUT /api/settings/:key.
func (h *SettingHandler) SetNeonixSettingCompat(c *gin.Context) {
	if !h.neonixSettingsService(c) {
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	if key == "" || len(key) > 128 {
		response.ErrorWithDetails(c, http.StatusBadRequest, "Invalid setting key", "SETTING_KEY_INVALID", nil)
		return
	}
	var payload struct {
		Value any `json:"value"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		response.ErrorWithDetails(c, http.StatusBadRequest, "Setting value is required", "SETTING_VALUE_INVALID", nil)
		return
	}
	if err := h.settingService.SetNeonixSetting(c.Request.Context(), key, payload.Value); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true})
}

// SetNeonixSettingsCompat implements PUT /api/settings with a flat object.
func (h *SettingHandler) SetNeonixSettingsCompat(c *gin.Context) {
	if !h.neonixSettingsService(c) {
		return
	}
	var values map[string]any
	if err := c.ShouldBindJSON(&values); err != nil || values == nil {
		response.ErrorWithDetails(c, http.StatusBadRequest, "Settings payload is required", "SETTINGS_PAYLOAD_INVALID", nil)
		return
	}
	if err := h.settingService.SetNeonixSettings(c.Request.Context(), values); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true})
}

func valueAsString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
