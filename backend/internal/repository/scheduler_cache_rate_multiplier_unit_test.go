//go:build unit

package repository

import (
	"context"
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

// TestSchedulerCachePreservesRateMultiplier 钉死账号调度快照的两份 payload
// （full + metadata）都必须保留 RateMultiplier。
//
// RateMultiplier participates in upstream billing calculations. The database
// column is non-null with Default(1.0), so nil here indicates that an explicit
// scheduler-cache field list dropped it during serialization.
func TestSchedulerCachePreservesRateMultiplier(t *testing.T) {
	rate := 0.75
	account := service.Account{
		ID:             9001,
		Name:           "scheduler-rate",
		Platform:       service.PlatformOpenAI,
		Type:           service.AccountTypeAPIKey,
		Status:         service.StatusActive,
		Schedulable:    true,
		Concurrency:    2,
		RateMultiplier: &rate,
	}

	t.Run("metadata payload keeps the field", func(t *testing.T) {
		meta := buildSchedulerMetadataAccount(account)
		require.NotNil(t, meta.RateMultiplier, "metadata 快照不得漏掉 rate_multiplier")
		require.Equal(t, rate, *meta.RateMultiplier)
	})

	t.Run("both payloads survive a decode round-trip", func(t *testing.T) {
		full, meta, err := marshalSchedulerCacheAccount(account)
		require.NoError(t, err)

		for name, payload := range map[string][]byte{"full": full, "metadata": meta} {
			decoded, decodeErr := decodeCachedAccount(payload)
			require.NoError(t, decodeErr, name)
			require.NotNil(t, decoded.RateMultiplier, "%s payload 反序列化后 rate_multiplier 不得为 nil", name)
			require.Equal(t, rate, *decoded.RateMultiplier, name)
		}
	})

	t.Run("zero rate survives as zero rather than nil", func(t *testing.T) {
		// 0 是合法值，绝不能在缓存序列化时被当成缺字段丢掉。
		zero := 0.0
		zeroAccount := account
		zeroAccount.ID = 9002
		zeroAccount.RateMultiplier = &zero

		_, meta, err := marshalSchedulerCacheAccount(zeroAccount)
		require.NoError(t, err)
		decoded, err := decodeCachedAccount(meta)
		require.NoError(t, err)
		require.NotNil(t, decoded.RateMultiplier)
		require.Equal(t, 0.0, *decoded.RateMultiplier)
	})

	t.Run("SetAccount then GetAccount preserves the field", func(t *testing.T) {
		cache := newSchedulerCacheUnit(t)
		ctx := context.Background()
		require.NoError(t, cache.SetAccount(ctx, &account))

		got, err := cache.GetAccount(ctx, account.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.NotNil(t, got.RateMultiplier, "端到端缓存读写后 rate_multiplier 不得丢失")
		require.Equal(t, rate, *got.RateMultiplier)
	})
}
