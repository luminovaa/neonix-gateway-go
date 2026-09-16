package routes

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

type neonixProxyStats struct{ db *sql.DB }

type neonixProxyStatsTotals struct {
	SuccessRequests int64   `json:"successRequests"`
	FailedRequests  int64   `json:"failedRequests"`
	InputTokens     int64   `json:"inputTokens"`
	OutputTokens    int64   `json:"outputTokens"`
	TotalCredits    float64 `json:"totalCredits"`
}

type neonixProxyStatsResponse struct {
	TotalRequests   int64   `json:"totalRequests"`
	SuccessRequests int64   `json:"successRequests"`
	FailedRequests  int64   `json:"failedRequests"`
	TotalCredits    float64 `json:"totalCredits"`
	InputTokens     int64   `json:"inputTokens"`
	OutputTokens    int64   `json:"outputTokens"`
	ActiveRequests  int64   `json:"activeRequests"`
}

const neonixProxyStatsTotalsSQL = `SELECT
    (SELECT COUNT(*) FROM usage_logs),
    (SELECT COUNT(*) FROM ops_error_logs WHERE COALESCE(status_code, 0) >= 400),
    (SELECT COALESCE(SUM(input_tokens + cache_creation_tokens + cache_read_tokens), 0) FROM usage_logs),
    (SELECT COALESCE(SUM(output_tokens), 0) FROM usage_logs),
    (SELECT COALESCE(SUM(actual_cost), 0) FROM usage_logs)`

func (s *neonixProxyStats) totals(ctx context.Context) (neonixProxyStatsTotals, error) {
	var totals neonixProxyStatsTotals
	err := s.db.QueryRowContext(ctx, neonixProxyStatsTotalsSQL).Scan(&totals.SuccessRequests, &totals.FailedRequests, &totals.InputTokens, &totals.OutputTokens, &totals.TotalCredits)
	return totals, err
}

func (s *neonixProxyStats) get(c *gin.Context) {
	if s == nil || s.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Proxy statistics are unavailable", "errorCode": "PROXY_STATS_UNAVAILABLE"})
		return
	}
	totals, err := s.totals(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load proxy statistics", "errorCode": "PROXY_STATS_LOAD_FAILED"})
		return
	}
	baseline := neonixProxyStatsTotals{}
	err = s.db.QueryRowContext(c.Request.Context(), `SELECT success_requests, failed_requests, input_tokens, output_tokens, total_credits FROM neonix_proxy_stats_baseline WHERE id=1`).Scan(&baseline.SuccessRequests, &baseline.FailedRequests, &baseline.InputTokens, &baseline.OutputTokens, &baseline.TotalCredits)
	if err != nil && err != sql.ErrNoRows {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load proxy statistics", "errorCode": "PROXY_STATS_LOAD_FAILED"})
		return
	}
	response := neonixProxyStatsResponse{
		SuccessRequests: nonNegativeInt64(totals.SuccessRequests - baseline.SuccessRequests),
		FailedRequests:  nonNegativeInt64(totals.FailedRequests - baseline.FailedRequests),
		InputTokens:     nonNegativeInt64(totals.InputTokens - baseline.InputTokens),
		OutputTokens:    nonNegativeInt64(totals.OutputTokens - baseline.OutputTokens),
		TotalCredits:    nonNegativeFloat(totals.TotalCredits - baseline.TotalCredits),
	}
	response.TotalRequests = response.SuccessRequests + response.FailedRequests
	c.JSON(http.StatusOK, response)
}

func (s *neonixProxyStats) reset(c *gin.Context) {
	if s == nil || s.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Proxy statistics are unavailable", "errorCode": "PROXY_STATS_UNAVAILABLE"})
		return
	}
	totals, err := s.totals(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset proxy statistics", "errorCode": "PROXY_STATS_RESET_FAILED"})
		return
	}
	_, err = s.db.ExecContext(c.Request.Context(), `INSERT INTO neonix_proxy_stats_baseline (id, success_requests, failed_requests, input_tokens, output_tokens, total_credits, reset_at) VALUES (1,$1,$2,$3,$4,$5,NOW()) ON CONFLICT (id) DO UPDATE SET success_requests=EXCLUDED.success_requests, failed_requests=EXCLUDED.failed_requests, input_tokens=EXCLUDED.input_tokens, output_tokens=EXCLUDED.output_tokens, total_credits=EXCLUDED.total_credits, reset_at=EXCLUDED.reset_at`, totals.SuccessRequests, totals.FailedRequests, totals.InputTokens, totals.OutputTokens, totals.TotalCredits)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset proxy statistics", "errorCode": "PROXY_STATS_RESET_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func nonNegativeInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
func nonNegativeFloat(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}
