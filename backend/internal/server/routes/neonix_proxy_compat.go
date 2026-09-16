package routes

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/config"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

const neonixProxyConfigSettingKey = "neonix_proxy_config"

var neonixProxyConfigFields = map[string]struct{}{
	"enabled":                          {},
	"port":                             {},
	"host":                             {},
	"enableMultiAccount":               {},
	"selectedAccountIds":               {},
	"logRequests":                      {},
	"logStreamEvents":                  {},
	"maxConcurrent":                    {},
	"perAccountMaxConcurrent":          {},
	"perAccountQueueTimeoutMs":         {},
	"maxRetries":                       {},
	"retryDelayMs":                     {},
	"preferredEndpoint":                {},
	"tokenRefreshBeforeExpiry":         {},
	"autoStart":                        {},
	"autoContinueRounds":               {},
	"enableServerSideToolAutoContinue": {},
	"clientDrivenToolExecution":        {},
	"disableTools":                     {},
	"autoSwitchOnQuotaExhausted":       {},
	"loadBalancer":                     {},
	"modelMappings":                    {},
	"tls":                              {},
}

var neonixProxySecretFields = map[string]struct{}{
	"apiKey":   {},
	"apiKeys":  {},
	"password": {},
	"token":    {},
	"secret":   {},
	"cert":     {},
	"key":      {},
}

func neonixProxyDefaults(cfg *config.Config) map[string]any {
	host := "0.0.0.0"
	port := 0
	if cfg != nil {
		host = strings.TrimSpace(cfg.Server.Host)
		if host == "" {
			host = "0.0.0.0"
		}
		port = cfg.Server.Port
	}
	return map[string]any{
		"enabled":                          true,
		"port":                             port,
		"host":                             host,
		"enableMultiAccount":               true,
		"selectedAccountIds":               []any{},
		"logRequests":                      true,
		"logStreamEvents":                  false,
		"maxConcurrent":                    0,
		"perAccountMaxConcurrent":          0,
		"perAccountQueueTimeoutMs":         0,
		"maxRetries":                       2,
		"retryDelayMs":                     250,
		"autoStart":                        true,
		"autoSwitchOnQuotaExhausted":       true,
		"loadBalancer":                     "least_in_flight",
		"modelMappings":                    []any{},
		"enableServerSideToolAutoContinue": false,
		"clientDrivenToolExecution":        true,
		"disableTools":                     false,
	}
}

func readNeonixProxyConfig(ctx context.Context, settingService *service.SettingService, cfg *config.Config) (map[string]any, error) {
	defaults := neonixProxyDefaults(cfg)
	if settingService == nil {
		return defaults, nil
	}
	value, err := settingService.GetNeonixSetting(ctx, neonixProxyConfigSettingKey)
	if err != nil {
		return nil, err
	}
	stored, ok := value.(map[string]any)
	if !ok {
		return defaults, nil
	}
	for key, value := range stored {
		if _, allowed := neonixProxyConfigFields[key]; allowed {
			if _, secret := neonixProxySecretFields[key]; !secret {
				defaults[key] = value
			}
		}
	}
	return normalizeNeonixProxyConfig(defaults), nil
}

func normalizeNeonixProxyConfig(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		if _, allowed := neonixProxyConfigFields[key]; !allowed {
			continue
		}
		if _, secret := neonixProxySecretFields[key]; secret {
			continue
		}
		if key == "tls" {
			result[key] = normalizeNeonixTLSConfig(value)
			continue
		}
		result[key] = value
	}
	if host, ok := result["host"].(string); ok {
		result["host"] = strings.TrimSpace(host)
	}
	if port, ok := numberAsInt(result["port"]); ok {
		if port < 0 || port > 65535 {
			delete(result, "port")
		} else {
			result["port"] = port
		}
	}
	for _, key := range []string{"maxConcurrent", "perAccountMaxConcurrent", "perAccountQueueTimeoutMs", "maxRetries", "retryDelayMs", "tokenRefreshBeforeExpiry", "autoContinueRounds"} {
		if value, exists := result[key]; exists {
			if number, ok := numberAsInt(value); !ok || number < 0 || number > 1_000_000 {
				delete(result, key)
			} else {
				result[key] = number
			}
		}
	}
	if selected, ok := result["selectedAccountIds"].([]any); ok && len(selected) > 1000 {
		result["selectedAccountIds"] = selected[:1000]
	}
	if mappings, ok := result["modelMappings"].([]any); ok && len(mappings) > 500 {
		result["modelMappings"] = mappings[:500]
	}
	return result
}

func numberAsInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed < float64(-1<<31) || typed > float64(1<<31-1) || math.Trunc(typed) != typed {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		parsed, err := strconv.Atoi(string(typed))
		return parsed, err == nil
	default:
		return 0, false
	}
}

func normalizeNeonixTLSConfig(value any) map[string]any {
	input, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]any, 3)
	for _, key := range []string{"enabled", "certPath", "keyPath"} {
		if item, exists := input[key]; exists {
			result[key] = item
		}
	}
	return result
}

func (r *neonixProxyConfigRuntime) get(ctx *gin.Context) {
	value, err := readNeonixProxyConfig(ctx.Request.Context(), r.settingService, r.cfg)
	if err != nil {
		response.ErrorWithDetails(ctx, http.StatusInternalServerError, "Proxy configuration is unavailable", "PROXY_CONFIG_UNAVAILABLE", nil)
		return
	}
	response.Success(ctx, value)
}

func (r *neonixProxyConfigRuntime) update(ctx *gin.Context) {
	var patch map[string]any
	if err := ctx.ShouldBindJSON(&patch); err != nil || patch == nil {
		response.ErrorWithDetails(ctx, http.StatusBadRequest, "Proxy configuration payload is invalid", "PROXY_CONFIG_INVALID", nil)
		return
	}
	current, err := readNeonixProxyConfig(ctx.Request.Context(), r.settingService, r.cfg)
	if err != nil {
		response.ErrorWithDetails(ctx, http.StatusInternalServerError, "Proxy configuration is unavailable", "PROXY_CONFIG_UNAVAILABLE", nil)
		return
	}
	for key, value := range patch {
		if _, secret := neonixProxySecretFields[key]; secret {
			continue
		}
		if _, allowed := neonixProxyConfigFields[key]; !allowed {
			continue
		}
		current[key] = value
	}
	normalized := normalizeNeonixProxyConfig(current)
	if err := r.settingService.SetNeonixSetting(ctx.Request.Context(), neonixProxyConfigSettingKey, normalized); err != nil {
		response.ErrorWithDetails(ctx, http.StatusInternalServerError, "Proxy configuration could not be saved", "PROXY_CONFIG_SAVE_FAILED", nil)
		return
	}
	response.Success(ctx, normalized)
}

type neonixProxyConfigRuntime struct {
	settingService *service.SettingService
	cfg            *config.Config
}

func newNeonixProxyConfigRuntime(settingService *service.SettingService, cfg *config.Config) *neonixProxyConfigRuntime {
	return &neonixProxyConfigRuntime{settingService: settingService, cfg: cfg}
}

func (r *neonixProxyConfigRuntime) status(ctx *gin.Context) {
	value, err := readNeonixProxyConfig(ctx.Request.Context(), r.settingService, r.cfg)
	if err != nil {
		response.ErrorWithDetails(ctx, http.StatusInternalServerError, "Proxy configuration is unavailable", "PROXY_CONFIG_UNAVAILABLE", nil)
		return
	}
	port, _ := numberAsInt(value["port"])
	host, _ := value["host"].(string)
	response.Success(ctx, gin.H{
		"running": true,
		"host":    host,
		"port":    port,
		"mode":    "gateway",
		"stats":   gin.H{},
	})
}
