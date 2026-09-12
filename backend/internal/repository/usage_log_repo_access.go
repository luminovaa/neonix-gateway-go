package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

// GetAPIKeyAccessStats implements the optional Neonix compatibility query.
// Usage logs already retain the client IP and user-agent, so no second access
// log table is needed during the migration.
func (r *usageLogRepository) GetAPIKeyAccessStatsSince(ctx context.Context, apiKeyID int64, since time.Time) (*service.APIKeyAccessStats, error) {
	if r == nil || r.sql == nil {
		return nil, fmt.Errorf("usage SQL executor is not configured")
	}
	if since.IsZero() {
		since = time.Unix(0, 0).UTC()
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT COUNT(*),
		       COUNT(DISTINCT NULLIF(ip_address, '')),
		       COUNT(DISTINCT NULLIF(user_agent, '')),
		       MAX(created_at)
		FROM usage_logs
		WHERE api_key_id = $1 AND created_at >= $2`, apiKeyID, since)
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
	if err := rows.Scan(&result.TotalRequests, &result.UniqueIPs, &result.UniqueUserAgents, &result.LastAccessedAt); err != nil {
		return nil, err
	}
	return result, rows.Err()
}

// GetAPIKeyRequestStats returns all request rows and the successful billed
// subset for a bounded range. Usage logs intentionally use actual_cost > 0 as
// their success marker, matching the repository's aggregate conventions.
func (r *usageLogRepository) GetAPIKeyRequestStats(ctx context.Context, apiKeyID int64, startTime, endTime time.Time) (*service.APIKeyRequestStats, error) {
	if r == nil || r.sql == nil {
		return nil, fmt.Errorf("usage SQL executor is not configured")
	}
	if startTime.IsZero() {
		startTime = time.Unix(0, 0).UTC()
	}
	if endTime.IsZero() {
		endTime = time.Now().UTC().Add(time.Nanosecond)
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE actual_cost > 0)
		FROM usage_logs
		WHERE api_key_id = $1 AND created_at >= $2 AND created_at < $3`, apiKeyID, startTime, endTime)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return &service.APIKeyRequestStats{}, nil
	}
	result := &service.APIKeyRequestStats{}
	if err := rows.Scan(&result.TotalRequests, &result.SuccessRequests); err != nil {
		return nil, err
	}
	return result, rows.Err()
}
