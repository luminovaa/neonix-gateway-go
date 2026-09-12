package routes

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type neonixProxyLogs struct{ db *sql.DB }

type neonixProxyRequestLog struct {
	ID               int64   `json:"id"`
	Timestamp        int64   `json:"timestamp"`
	Path             string  `json:"path"`
	Model            string  `json:"model"`
	AccountID        string  `json:"accountId"`
	AccountEmail     string  `json:"accountEmail,omitempty"`
	AccountNickname  string  `json:"accountNickname,omitempty"`
	AccountProvider  string  `json:"accountProvider,omitempty"`
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	Credits          float64 `json:"credits,omitempty"`
	ResponseTime     int64   `json:"responseTime"`
	TTFT             int64   `json:"ttft"`
	Status           int     `json:"status"`
	Success          bool    `json:"success"`
	Error            string  `json:"error,omitempty"`
}

func parseProxyLogQuery(c *gin.Context) (int, int64) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "1000"))
	if limit < 1 {
		limit = 1
	}
	if limit > 10000 {
		limit = 10000
	}
	since, _ := strconv.ParseInt(strings.TrimSpace(c.Query("since")), 10, 64)
	if since > 0 && since < 10_000_000_000 {
		since *= 1000
	}
	return limit, since
}

func (s *neonixProxyLogs) list(c *gin.Context) {
	if s == nil || s.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Proxy request logs are unavailable", "errorCode": "PROXY_LOGS_UNAVAILABLE"})
		return
	}
	limit, since := parseProxyLogQuery(c)
	logs, err := s.query(c.Request.Context(), limit, since)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load proxy request logs", "errorCode": "PROXY_LOGS_LOAD_FAILED"})
		return
	}
	c.JSON(http.StatusOK, logs)
}

func (s *neonixProxyLogs) query(ctx context.Context, limit int, since int64) ([]neonixProxyRequestLog, error) {
	rows, err := s.db.QueryContext(ctx, neonixProxyLogsSQL, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := make([]neonixProxyRequestLog, 0)
	for rows.Next() {
		var item neonixProxyRequestLog
		if err := rows.Scan(&item.ID, &item.Timestamp, &item.Path, &item.Model, &item.AccountID, &item.AccountEmail, &item.AccountNickname, &item.AccountProvider, &item.InputTokens, &item.OutputTokens, &item.CacheReadTokens, &item.CacheWriteTokens, &item.Credits, &item.ResponseTime, &item.TTFT, &item.Status, &item.Success, &item.Error); err != nil {
			return nil, err
		}
		logs = append(logs, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return logs, nil
}

// Success and error logs are append-only audit records. Clearing the UI creates
// a per-operator view boundary instead of destroying billing or incident data.
func (s *neonixProxyLogs) clear(c *gin.Context) {
	if s == nil || s.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Proxy request logs are unavailable", "errorCode": "PROXY_LOGS_UNAVAILABLE"})
		return
	}
	_, err := s.db.ExecContext(c.Request.Context(), `INSERT INTO neonix_proxy_log_view_state (id, cleared_at) VALUES (1,NOW()) ON CONFLICT (id) DO UPDATE SET cleared_at=EXCLUDED.cleared_at`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear proxy request log view", "errorCode": "PROXY_LOGS_CLEAR_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

const neonixProxyLogsSQL = `WITH view_state AS (
  SELECT COALESCE((SELECT cleared_at FROM neonix_proxy_log_view_state WHERE id=1), '-infinity'::timestamptz) AS cleared_at
), combined AS (
  SELECT ul.id, ul.created_at, COALESCE(ul.inbound_endpoint, '/v1') AS path, COALESCE(ul.requested_model, ul.model) AS model,
         ul.account_id, COALESCE(a.name, '') AS account_name, COALESCE(a.platform, '') AS account_provider,
         ul.input_tokens::bigint, ul.output_tokens::bigint, ul.cache_read_tokens::bigint, ul.cache_creation_tokens::bigint,
         ul.actual_cost::double precision, COALESCE(ul.duration_ms,0)::bigint, COALESCE(ul.first_token_ms,ul.duration_ms,0)::bigint,
         200 AS status, TRUE AS success, '' AS error
  FROM usage_logs ul LEFT JOIN accounts a ON a.id=ul.account_id CROSS JOIN view_state v
  WHERE ul.created_at >= v.cleared_at AND ($1::bigint <= 0 OR ul.created_at >= to_timestamp($1::double precision / 1000.0))
  UNION ALL
  SELECT -o.id, o.created_at, COALESCE(o.inbound_endpoint,o.request_path,'/v1') AS path, COALESCE(o.requested_model,o.model,'') AS model,
         COALESCE(o.account_id,0), COALESCE(a.name,''), COALESCE(NULLIF(o.platform,''),a.platform,''),
         0::bigint,0::bigint,0::bigint,0::bigint,0::double precision,COALESCE(o.duration_ms,0)::bigint,COALESCE(o.time_to_first_token_ms,o.duration_ms,0)::bigint,
         COALESCE(o.status_code,500),FALSE,COALESCE(NULLIF(o.error_message,''),'Request failed')
  FROM ops_error_logs o LEFT JOIN accounts a ON a.id=o.account_id CROSS JOIN view_state v
  WHERE o.created_at >= v.cleared_at AND COALESCE(o.status_code,0) >= 400 AND ($1::bigint <= 0 OR o.created_at >= to_timestamp($1::double precision / 1000.0))
)
SELECT id, (EXTRACT(EPOCH FROM created_at)*1000)::bigint, path, model, account_id::text, '', account_name, account_provider, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, credits, response_time, ttft, status, success, error
FROM combined ORDER BY created_at DESC, id DESC LIMIT $2`
