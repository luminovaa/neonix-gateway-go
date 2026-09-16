package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateNeonixInternalServiceSecrets(t *testing.T) {
	t.Setenv("PYAUTO_BASE_URL", "")
	t.Setenv("RUNTIME_MANAGER_URL", "")
	t.Setenv("PYAUTO_INTERNAL_API_KEY", "")
	t.Setenv("RUNTIME_MANAGER_API_KEY", "")
	require.NoError(t, validateNeonixInternalServiceSecrets())

	t.Setenv("PYAUTO_BASE_URL", "http://python-automation:7788")
	require.ErrorContains(t, validateNeonixInternalServiceSecrets(), "PYAUTO_INTERNAL_API_KEY")
	t.Setenv("PYAUTO_INTERNAL_API_KEY", "worker-secret")
	require.NoError(t, validateNeonixInternalServiceSecrets())

	t.Setenv("RUNTIME_MANAGER_URL", "http://python-automation:7790")
	require.ErrorContains(t, validateNeonixInternalServiceSecrets(), "RUNTIME_MANAGER_API_KEY")
	t.Setenv("RUNTIME_MANAGER_API_KEY", "worker-secret")
	require.ErrorContains(t, validateNeonixInternalServiceSecrets(), "must differ")
	t.Setenv("RUNTIME_MANAGER_API_KEY", "manager-secret")
	require.NoError(t, validateNeonixInternalServiceSecrets())
}
