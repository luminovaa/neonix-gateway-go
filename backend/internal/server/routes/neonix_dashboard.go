package routes

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type neonixDashboard struct{ db *sql.DB }

type dashboardAggregate struct {
	TotalRequests     int64   `json:"totalRequests"`
	SuccessRequests   int64   `json:"successRequests"`
	FailedRequests    int64   `json:"failedRequests"`
	InputTokens       int64   `json:"inputTokens"`
	OutputTokens      int64   `json:"outputTokens"`
	CacheReadTokens   int64   `json:"cacheReadTokens,omitempty"`
	TotalCredits      float64 `json:"totalCredits"`
	TotalResponseTime int64   `json:"totalResponseTime,omitempty"`
}

type dashboardModel struct {
	Rank  int    `json:"rank,omitempty"`
	Model string `json:"model"`
	dashboardAggregate
	SuccessRate float64 `json:"successRate,omitempty"`
	TPS         float64 `json:"tps,omitempty"`
}

type dashboardSeries struct {
	Date  string `json:"date"`
	Label string `json:"label"`
	Hour  *int   `json:"hour,omitempty"`
	dashboardAggregate
}

type publicStatsSummary struct {
	TotalTokens   int64 `json:"totalTokens"`
	TotalRequests int64 `json:"totalRequests"`
}

type publicModelStats struct {
	Model         string `json:"model"`
	TotalTokens   int64  `json:"totalTokens"`
	TotalRequests int64  `json:"totalRequests"`
}

func dashboardPeriod(value string, now time.Time) (string, time.Time, int) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch value {
	case "7d":
		return "7d", today.AddDate(0, 0, -6), 7
	case "30d":
		return "30d", today.AddDate(0, 0, -29), 30
	default:
		return "today", today, 24
	}
}

func leaderboardStart(value string, now time.Time) (string, *time.Time) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch value {
	case "today":
		return value, &today
	case "24h":
		start := now.Add(-24 * time.Hour)
		return value, &start
	case "7d":
		start := today.AddDate(0, 0, -6)
		return value, &start
	case "30d":
		start := today.AddDate(0, 0, -29)
		return value, &start
	default:
		return "all", nil
	}
}

func (d *neonixDashboard) payload(ctx context.Context, period string, start time.Time, slots int) (gin.H, error) {
	summary, err := d.summary(ctx, start)
	if err != nil {
		return nil, err
	}
	models, err := d.models(ctx, &start, 20)
	if err != nil {
		return nil, err
	}
	seriesRows, err := d.series(ctx, period, start)
	if err != nil {
		return nil, err
	}
	series := fillDashboardSeries(period, start, slots, seriesRows)
	logs, err := (&neonixProxyLogs{db: d.db}).query(ctx, 100, start.UnixMilli())
	if err != nil {
		return nil, err
	}
	recent := logs
	if len(recent) > 25 {
		recent = recent[:25]
	}
	failed := make([]neonixProxyRequestLog, 0, 10)
	for _, item := range logs {
		if !item.Success {
			failed = append(failed, item)
			if len(failed) == 10 {
				break
			}
		}
	}
	return gin.H{"period": period, "summary": summary, "series": series, "models": models, "recentRequests": recent, "failedRequests": failed}, nil
}

func (d *neonixDashboard) summary(ctx context.Context, start time.Time) (dashboardAggregate, error) {
	var out dashboardAggregate
	err := d.db.QueryRowContext(ctx, dashboardSummarySQL, start).Scan(&out.SuccessRequests, &out.FailedRequests, &out.InputTokens, &out.OutputTokens, &out.CacheReadTokens, &out.TotalCredits, &out.TotalResponseTime)
	out.TotalRequests = out.SuccessRequests + out.FailedRequests
	return out, err
}

func (d *neonixDashboard) models(ctx context.Context, start *time.Time, limit int) ([]dashboardModel, error) {
	rows, err := d.db.QueryContext(ctx, dashboardModelsSQL, start, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]dashboardModel, 0)
	for rows.Next() {
		var m dashboardModel
		if err := rows.Scan(&m.Model, &m.SuccessRequests, &m.FailedRequests, &m.InputTokens, &m.OutputTokens, &m.TotalCredits, &m.TotalResponseTime); err != nil {
			return nil, err
		}
		m.TotalRequests = m.SuccessRequests + m.FailedRequests
		m.SuccessRate = percentage(m.SuccessRequests, m.TotalRequests)
		m.TPS = throughput(m.OutputTokens, m.TotalResponseTime)
		m.Rank = len(out) + 1
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *neonixDashboard) series(ctx context.Context, period string, start time.Time) ([]dashboardSeries, error) {
	query := dashboardDailySQL
	if period == "today" {
		query = dashboardHourlySQL
	}
	rows, err := d.db.QueryContext(ctx, query, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]dashboardSeries, 0)
	for rows.Next() {
		var r dashboardSeries
		var bucket time.Time
		if err := rows.Scan(&bucket, &r.SuccessRequests, &r.FailedRequests, &r.InputTokens, &r.OutputTokens, &r.TotalCredits); err != nil {
			return nil, err
		}
		r.TotalRequests = r.SuccessRequests + r.FailedRequests
		r.Date = bucket.UTC().Format(time.RFC3339)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *neonixDashboard) me(c *gin.Context) { d.respondDashboard(c) }

func (d *neonixDashboard) publicStats(c *gin.Context) {
	if d == nil || d.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Public stats are unavailable", "errorCode": "PUBLIC_STATS_UNAVAILABLE"})
		return
	}
	var summary publicStatsSummary
	if err := d.db.QueryRowContext(c.Request.Context(), publicStatsSummarySQL).Scan(&summary.TotalRequests, &summary.TotalTokens); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load public stats", "errorCode": "PUBLIC_STATS_LOAD_FAILED"})
		return
	}
	rows, err := d.db.QueryContext(c.Request.Context(), publicStatsModelsSQL, 5)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load public stats", "errorCode": "PUBLIC_STATS_LOAD_FAILED"})
		return
	}
	defer rows.Close()
	models := make([]publicModelStats, 0, 5)
	for rows.Next() {
		var model publicModelStats
		if err := rows.Scan(&model.Model, &model.TotalRequests, &model.TotalTokens); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load public stats", "errorCode": "PUBLIC_STATS_LOAD_FAILED"})
			return
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load public stats", "errorCode": "PUBLIC_STATS_LOAD_FAILED"})
		return
	}
	c.Header("Cache-Control", "public, max-age=60")
	c.JSON(http.StatusOK, gin.H{"summary": summary, "popularModels": models})
}
func (d *neonixDashboard) respondDashboard(c *gin.Context) {
	if d == nil || d.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Dashboard is unavailable", "errorCode": "DASHBOARD_UNAVAILABLE"})
		return
	}
	period, start, slots := dashboardPeriod(c.Query("period"), time.Now())
	payload, err := d.payload(c.Request.Context(), period, start, slots)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load dashboard", "errorCode": "DASHBOARD_LOAD_FAILED"})
		return
	}
	c.JSON(http.StatusOK, payload)
}

func (d *neonixDashboard) leaderboard(c *gin.Context) {
	if d == nil || d.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Leaderboard is unavailable", "errorCode": "LEADERBOARD_UNAVAILABLE"})
		return
	}
	period, start := leaderboardStart(c.Query("period"), time.Now())
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	models, err := d.models(c.Request.Context(), start, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load leaderboard", "errorCode": "LEADERBOARD_LOAD_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"period": period, "limit": limit, "models": models})
}

func fillDashboardSeries(period string, start time.Time, slots int, rows []dashboardSeries) []dashboardSeries {
	byKey := map[string]dashboardSeries{}
	layout := "2006-01-02"
	if period == "today" {
		layout = "2006-01-02T15"
	}
	for _, r := range rows {
		t, _ := time.Parse(time.RFC3339, r.Date)
		byKey[t.UTC().Format(layout)] = r
	}
	out := make([]dashboardSeries, 0, slots)
	for i := 0; i < slots; i++ {
		t := start.Add(time.Duration(i) * 24 * time.Hour)
		if period == "today" {
			t = start.Add(time.Duration(i) * time.Hour)
		}
		key := t.Format(layout)
		r := byKey[key]
		r.Date = t.Format(time.RFC3339)
		if period == "today" {
			hour := t.Hour()
			r.Hour = &hour
			r.Label = fmt.Sprintf("%02d:00", (hour+7)%24)
		} else {
			r.Label = t.Format("01-02")
		}
		out = append(out, r)
	}
	return out
}
func percentage(success, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(success*1000/total) / 10
}
func throughput(tokens, duration int64) float64 {
	if duration <= 0 {
		return 0
	}
	return float64(tokens) * 1000 / float64(duration)
}

const dashboardSummarySQL = `SELECT (SELECT COUNT(*) FROM usage_logs WHERE created_at >= $1),(SELECT COUNT(*) FROM ops_error_logs WHERE created_at >= $1 AND COALESCE(status_code,0)>=400),(SELECT COALESCE(SUM(input_tokens),0) FROM usage_logs WHERE created_at >= $1),(SELECT COALESCE(SUM(output_tokens),0) FROM usage_logs WHERE created_at >= $1),(SELECT COALESCE(SUM(cache_read_tokens),0) FROM usage_logs WHERE created_at >= $1),(SELECT COALESCE(SUM(actual_cost),0) FROM usage_logs WHERE created_at >= $1),(SELECT COALESCE(SUM(duration_ms),0) FROM usage_logs WHERE created_at >= $1)`
const publicStatsSummarySQL = `SELECT COUNT(*),COALESCE(SUM(COALESCE(input_tokens,0) + COALESCE(output_tokens,0) + COALESCE(cache_read_tokens,0)),0) FROM usage_logs`
const publicStatsModelsSQL = `SELECT COALESCE(requested_model,model,'unknown') model,COUNT(*),COALESCE(SUM(COALESCE(input_tokens,0) + COALESCE(output_tokens,0) + COALESCE(cache_read_tokens,0)),0) tokens FROM usage_logs GROUP BY 1 ORDER BY tokens DESC,model LIMIT $1`
const dashboardModelsSQL = `WITH combined AS (SELECT COALESCE(requested_model,model) model,1 success,0 failed,input_tokens::bigint,output_tokens::bigint,actual_cost::double precision credits,COALESCE(duration_ms,0)::bigint duration FROM usage_logs WHERE ($1::timestamptz IS NULL OR created_at >= $1) UNION ALL SELECT COALESCE(requested_model,model,'unknown'),0,1,0::bigint,0::bigint,0::double precision,COALESCE(duration_ms,0)::bigint FROM ops_error_logs WHERE COALESCE(status_code,0)>=400 AND ($1::timestamptz IS NULL OR created_at >= $1)) SELECT model,SUM(success),SUM(failed),SUM(input_tokens),SUM(output_tokens),SUM(credits),SUM(duration) FROM combined GROUP BY model ORDER BY SUM(input_tokens+output_tokens) DESC,model LIMIT $2`
const dashboardHourlySQL = `WITH buckets AS (SELECT date_trunc('hour',created_at) bucket,COUNT(*) success,0 failed,SUM(input_tokens)::bigint input_tokens,SUM(output_tokens)::bigint output_tokens,SUM(actual_cost)::double precision credits FROM usage_logs WHERE created_at >= $1 GROUP BY 1 UNION ALL SELECT date_trunc('hour',created_at),0,COUNT(*),0,0,0 FROM ops_error_logs WHERE created_at >= $1 AND COALESCE(status_code,0)>=400 GROUP BY 1) SELECT bucket,SUM(success),SUM(failed),SUM(input_tokens),SUM(output_tokens),SUM(credits) FROM buckets GROUP BY bucket ORDER BY bucket`
const dashboardDailySQL = `WITH buckets AS (SELECT date_trunc('day',created_at) bucket,COUNT(*) success,0 failed,SUM(input_tokens)::bigint input_tokens,SUM(output_tokens)::bigint output_tokens,SUM(actual_cost)::double precision credits FROM usage_logs WHERE created_at >= $1 GROUP BY 1 UNION ALL SELECT date_trunc('day',created_at),0,COUNT(*),0,0,0 FROM ops_error_logs WHERE created_at >= $1 AND COALESCE(status_code,0)>=400 GROUP BY 1) SELECT bucket,SUM(success),SUM(failed),SUM(input_tokens),SUM(output_tokens),SUM(credits) FROM buckets GROUP BY bucket ORDER BY bucket`
