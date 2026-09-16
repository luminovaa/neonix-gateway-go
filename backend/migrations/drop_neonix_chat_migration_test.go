package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDropNeonixChatMigration(t *testing.T) {
	raw, err := FS.ReadFile("249_drop_neonix_chat.sql")
	require.NoError(t, err)

	sql := strings.ToLower(string(raw))
	summary := strings.Index(sql, "drop table if exists chat_context_summaries")
	messages := strings.Index(sql, "drop table if exists chat_messages")
	sessions := strings.Index(sql, "drop table if exists chat_sessions")
	require.GreaterOrEqual(t, summary, 0)
	require.Greater(t, messages, summary)
	require.Greater(t, sessions, messages)
}
