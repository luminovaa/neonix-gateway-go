package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/usagestats"
)

// GetAPIKeyModelStatsCompat combines native Go usage with the one-time
// imported Neonix history. It is deliberately API-key scoped: legacy rows do
// not contain trustworthy Go account/group dimensions.
func (r *usageLogRepository) GetAPIKeyModelStatsCompat(ctx context.Context, apiKeyID int64, startTime, endTime time.Time) (results []usagestats.ModelStat, err error) {
	if r == nil || r.sql == nil {
		return nil, fmt.Errorf("usage SQL executor is not configured")
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT model, SUM(requests), SUM(input_tokens), SUM(output_tokens),
		       SUM(cache_creation_tokens), SUM(cache_read_tokens), SUM(total_tokens),
		       SUM(cost), SUM(actual_cost), SUM(account_cost)
		FROM (
		  SELECT model, COUNT(*) AS requests,
		         COALESCE(SUM(input_tokens), 0) AS input_tokens,
		         COALESCE(SUM(output_tokens), 0) AS output_tokens,
		         COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
		         COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
		         COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens), 0) AS total_tokens,
		         COALESCE(SUM(total_cost), 0) AS cost, COALESCE(SUM(actual_cost), 0) AS actual_cost,
		         COALESCE(SUM(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)), 0) AS account_cost
		  FROM usage_logs WHERE api_key_id = $1 AND created_at >= $2 AND created_at < $3 GROUP BY model
		  UNION ALL
		  SELECT model, COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		         0, 0, COALESCE(SUM(input_tokens + output_tokens), 0),
		         COALESCE(SUM(credits), 0), COALESCE(SUM(credits), 0), COALESCE(SUM(credits), 0)
		  FROM neonix_legacy_api_key_usage
		  WHERE api_key_id = $1 AND occurred_at >= $2 AND occurred_at < $3 GROUP BY model
		) model_rows
		GROUP BY model ORDER BY SUM(total_tokens) DESC`, apiKeyID, startTime, endTime)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
			results = nil
		}
	}()
	results = make([]usagestats.ModelStat, 0)
	for rows.Next() {
		var item usagestats.ModelStat
		if err := rows.Scan(&item.Model, &item.Requests, &item.InputTokens, &item.OutputTokens,
			&item.CacheCreationTokens, &item.CacheReadTokens, &item.TotalTokens, &item.Cost,
			&item.ActualCost, &item.AccountCost); err != nil {
			return nil, err
		}
		results = append(results, item)
	}
	return results, rows.Err()
}
