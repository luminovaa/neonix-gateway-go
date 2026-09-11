package admin

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAntigravityCallbackURLValidatesOriginAndFields(t *testing.T) {
	code, state, err := parseAntigravityCallbackURL("http://localhost:8085/callback?code=abc%2B123&state=state-1")
	require.NoError(t, err)
	require.Equal(t, "abc+123", code)
	require.Equal(t, "state-1", state)

	for _, raw := range []string{
		"https://localhost:8085/callback?code=abc&state=s",
		"http://localhost:8080/callback?code=abc&state=s",
		"http://127.0.0.1:8085/callback?code=abc&state=s",
		"http://localhost:8085/callback?code=abc",
		"javascript:alert(1)",
		"http://localhost:8085/callback?code=abc&state=s#token",
	} {
		_, _, err := parseAntigravityCallbackURL(raw)
		require.Error(t, err, raw)
	}
}
