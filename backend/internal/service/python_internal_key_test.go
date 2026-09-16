package service

import "testing"

func TestPythonRuntimeConstructorsDoNotFallBackToAPIServerSecret(t *testing.T) {
	t.Setenv("API_SECRET", "retired-node-secret")
	t.Setenv("RUNTIME_MANAGER_URL", "http://runtime-manager.test")
	t.Setenv("PYAUTO_INTERNAL_API_KEY", "")
	t.Setenv("RUNTIME_MANAGER_API_KEY", "")

	if got := NewPythonRegisterRuntime().apiKey; got != "" {
		t.Fatalf("register runtime accepted API_SECRET fallback: %q", got)
	}
	if got := NewPythonMailboxRuntime().apiKey; got != "" {
		t.Fatalf("mailbox runtime accepted API_SECRET fallback: %q", got)
	}
	if got := NewPythonRuntimeManager().apiKey; got != "" {
		t.Fatalf("runtime manager accepted API_SECRET fallback: %q", got)
	}
}

func TestPythonRuntimeConstructorsUseTheirDedicatedKeys(t *testing.T) {
	t.Setenv("RUNTIME_MANAGER_URL", "http://runtime-manager.test")
	t.Setenv("PYAUTO_INTERNAL_API_KEY", "worker-key")
	t.Setenv("RUNTIME_MANAGER_API_KEY", "manager-key")

	if got := NewPythonRegisterRuntime().apiKey; got != "worker-key" {
		t.Fatalf("register runtime key = %q", got)
	}
	if got := NewPythonMailboxRuntime().apiKey; got != "worker-key" {
		t.Fatalf("mailbox runtime key = %q", got)
	}
	if got := NewPythonRuntimeManager().apiKey; got != "manager-key" {
		t.Fatalf("runtime manager key = %q", got)
	}
}
