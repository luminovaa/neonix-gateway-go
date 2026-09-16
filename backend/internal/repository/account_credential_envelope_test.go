package repository

import "testing"

func TestCredentialEnvelopeFromEnvironmentAcceptsUnsetOrValidAutomationKey(t *testing.T) {
	t.Setenv("NEONIX_CREDENTIAL_KEY", "")
	codec, err := credentialEnvelopeFromEnvironment()
	if err != nil || codec != nil {
		t.Fatalf("unset key: codec=%v err=%v", codec, err)
	}

	t.Setenv("NEONIX_CREDENTIAL_KEY", "3031323334353637383930313233343536373839303132333435363738393031")
	codec, err = credentialEnvelopeFromEnvironment()
	if err != nil || codec == nil {
		t.Fatalf("valid key: codec=%v err=%v", codec, err)
	}
}

func TestCredentialEnvelopeFromEnvironmentRejectsMalformedAutomationKey(t *testing.T) {
	t.Setenv("NEONIX_CREDENTIAL_KEY", "not-a-key")
	if _, err := credentialEnvelopeFromEnvironment(); err == nil {
		t.Fatal("expected malformed key rejection")
	}
}
