package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClearGrokAccountModelQuotaBlocksAfterRelogin(t *testing.T) {
	accountID := time.Now().UnixNano()
	now := time.Now()
	markGrokModelQuotaBlock(accountID, "grok-4.5", now.Add(time.Hour))
	markGrokModelQuotaBlock(accountID+1, "grok-4.5", now.Add(time.Hour))
	require.True(t, isGrokModelQuotaBlocked(accountID, "grok-4.5", now))

	ClearGrokAccountModelQuotaBlocks(accountID)

	require.False(t, isGrokModelQuotaBlocked(accountID, "grok-4.5", now))
	require.True(t, isGrokModelQuotaBlocked(accountID+1, "grok-4.5", now))
}

func TestClearGrokAccountTeamModelRateLimitsAfterRelogin(t *testing.T) {
	now := time.Now()
	account := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{"team_id": "relogin-team-a"}}
	sibling := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{"team_id": "relogin-team-a"}}
	other := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{"team_id": "relogin-team-b"}}
	markGrokTeamModelRateLimit(account, "grok-4.5", now.Add(time.Hour))
	markGrokTeamModelRateLimit(other, "grok-4.5", now.Add(time.Hour))
	require.True(t, isGrokTeamModelRateLimited(sibling, "grok-4.5", now))

	ClearGrokAccountTeamModelRateLimits(account)

	require.False(t, isGrokTeamModelRateLimited(sibling, "grok-4.5", now))
	require.True(t, isGrokTeamModelRateLimited(other, "grok-4.5", now))
}
