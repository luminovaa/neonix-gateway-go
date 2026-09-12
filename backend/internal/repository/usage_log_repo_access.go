package repository

import (
	"context"
	"fmt"

	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

// GetAPIKeyAccessStats implements the optional Neonix compatibility query.
// Usage logs already retain the client IP and user-agent, so no second access
// log table is needed during the migration.
func (r *usageLogRepository) GetAPIKeyAccessStats(ctx context.Context, apiKeyID int64) (*service.APIKeyAccessStats, error) {
	if r == nil || r.sql == nil {
		return nil, fmt.Errorf("usage SQL executor is not configured")
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT COUNT(*),
		       COUNT(DISTINCT NULLIF(ip_address, '')),
		       COUNT(DISTINCT NULLIF(user_agent, ''))
		FROM usage_logs
		WHERE api_key_id = $1`, apiKeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return &service.APIKeyAccessStats{}, nil
	}
	result := &service.APIKeyAccessStats{}
	if err := rows.Scan(&result.TotalRequests, &result.UniqueIPs, &result.UniqueUserAgents); err != nil {
		return nil, err
	}
	return result, rows.Err()
}
