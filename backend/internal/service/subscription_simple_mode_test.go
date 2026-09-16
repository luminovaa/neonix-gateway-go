package service

import (
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionServiceSimpleModeDoesNotStartSaaSRuntime(t *testing.T) {
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.SubscriptionCache.L1Size = 100
	cfg.SubscriptionCache.L1TTLSeconds = 60
	cfg.SubscriptionMaintenance.WorkerCount = 2
	cfg.SubscriptionMaintenance.QueueSize = 8

	svc := NewSubscriptionService(nil, nil, nil, nil, cfg)
	t.Cleanup(svc.Stop)

	require.Nil(t, svc.subCacheL1)
	require.Nil(t, svc.maintenanceQueue)
}
