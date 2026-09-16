package admin

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	"github.com/luminovaa/neonix-gateway-go/internal/provider"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

// ResilienceCompat exposes the read-only account/pool snapshot required by the
// Neonix Proxy Resilience tab. Runtime counters that are not yet owned by Go
// stay explicit zeroes instead of being inferred from stale Node state.
func (h *AccountHandler) ResilienceCompat(c *gin.Context) {
	if h == nil || h.adminService == nil {
		response.InternalError(c, "Resilience service is not configured")
		return
	}
	accounts, err := h.adminService.ListAccountsForSchedulerScoreFilter(c.Request.Context(), "", "", "", "", 0, "")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	coverageSnapshots := make([]provider.AccountSnapshot, 0, len(accounts))
	for _, account := range accounts {
		sourceProvider := ""
		if account.Extra != nil {
			if value, ok := account.Extra["source_provider"].(string); ok {
				sourceProvider = strings.TrimSpace(value)
			}
		}
		coverageSnapshots = append(coverageSnapshots, provider.AccountSnapshot{
			SourceProvider: sourceProvider, Platform: account.Platform,
			Status: account.Status, Schedulable: account.IsSchedulable(),
		})
	}
	summary := provider.BuildSummary(coverageSnapshots)
	coverage := make([]gin.H, 0, len(summary))
	for _, item := range summary {
		status := "ready"
		reason := ""
		if item.TotalAccounts == 0 {
			status, reason = "missing", "no_accounts"
		} else if item.ActiveAccounts == 0 {
			status, reason = "unavailable", "no_usable_accounts"
		}
		coverage = append(coverage, gin.H{
			"id": item.ID, "name": item.Name, "totalAccounts": item.TotalAccounts,
			"usableAccounts": item.ActiveAccounts, "runtimeAccounts": item.ActiveAccounts,
			"status": status, "reason": reason,
		})
	}

	modelFilter := strings.ToLower(strings.TrimSpace(c.Query("model")))
	providerFilter := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	items := make([]gin.H, 0, len(accounts))
	for _, account := range accounts {
		accountProvider := account.Platform
		if account.Extra != nil {
			if source, ok := account.Extra["source_provider"].(string); ok && strings.TrimSpace(source) != "" {
				accountProvider = strings.TrimSpace(source)
			}
		}
		if providerFilter != "" && !strings.Contains(strings.ToLower(accountProvider), providerFilter) {
			continue
		}
		if modelFilter != "" && !strings.Contains(strings.ToLower(account.GetCredential("model")), modelFilter) {
			continue
		}
		reasons := resilienceAccountReasons(&account)
		email := strings.TrimSpace(account.GetCredential("email"))
		if email == "" {
			email = account.Name
		}
		items = append(items, gin.H{
			"id": strconv.FormatInt(account.ID, 10), "email": email, "provider": accountProvider,
			"tier": resilienceAccountTier(&account), "requestCount": 0, "errorCount": 0,
			"eligible": account.IsSchedulable(), "reasons": reasons,
			"queue":     gin.H{"accountId": strconv.FormatInt(account.ID, 10), "active": 0, "queued": 0, "maxConcurrent": account.EffectiveLoadFactor()},
			"modelLock": nil,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i]["email"].(string)) < strings.ToLower(items[j]["email"].(string))
	})
	page := positiveIntQuery(c.Query("page"), 1)
	pageSize := positiveIntQuery(c.Query("pageSize"), 25)
	if pageSize > 100 {
		pageSize = 100
	}
	total := len(items)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pages := (total + pageSize - 1) / pageSize
	if pages == 0 {
		pages = 1
	}
	response.Success(c, gin.H{
		"running":    true,
		"config":     gin.H{"loadBalancer": "round_robin", "perAccountMaxConcurrent": 0, "perAccountQueueTimeoutMs": 0},
		"warmup":     gin.H{"enabled": false, "intervalMs": 0, "running": false, "lastRunAt": 0, "success": 0, "failed": 0, "skipped": 0},
		"transport":  gin.H{"proxiedRequests": 0, "directFallbacks": 0, "directRequests": 0, "proxyFailures": 0},
		"modelLocks": []any{}, "coverage": coverage, "accounts": items[start:end],
		"pagination": gin.H{"page": page, "pageSize": pageSize, "totalItems": total, "totalPages": pages},
		"summary":    gin.H{"accounts": total, "eligibleAccounts": countEligible(items), "activeRequests": 0, "queuedRequests": 0, "modelLocks": 0},
	})
}

func (h *AccountHandler) ClearModelLocksCompat(c *gin.Context) {
	response.Success(c, gin.H{"ok": true, "cleared": 0})
}

func resilienceAccountReasons(account *service.Account) []string {
	if account == nil {
		return []string{"missing"}
	}
	reasons := make([]string, 0, 4)
	if account.Status != service.StatusActive || !account.Schedulable {
		if account.IsQuotaExceeded() {
			reasons = append(reasons, "quota_exhausted")
		} else {
			reasons = append(reasons, "inactive")
		}
	}
	if account.ExpiresAt != nil && account.AutoPauseOnExpired && !account.ExpiresAt.After(time.Now()) {
		reasons = append(reasons, "token_expired")
	}
	if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(time.Now()) {
		reasons = append(reasons, "queue_busy")
	}
	if account.OverloadUntil != nil && account.OverloadUntil.After(time.Now()) {
		reasons = append(reasons, "queue_busy")
	}
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(time.Now()) {
		reasons = append(reasons, "queue_busy")
	}
	return reasons
}

func resilienceAccountTier(account *service.Account) string {
	if account == nil {
		return "default"
	}
	if tier := strings.TrimSpace(account.GetCredential("tier")); tier != "" {
		return tier
	}
	return "default"
}

func countEligible(items []gin.H) int {
	count := 0
	for _, item := range items {
		if eligible, _ := item["eligible"].(bool); eligible {
			count++
		}
	}
	return count
}

func positiveIntQuery(raw string, fallback int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
