package repository

import (
	"encoding/hex"
	"errors"
	"os"
	"strings"

	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
)

// credentialEnvelopeFromEnvironment loads the key used exclusively for
// automation-only secrets (for example a Grok registration password). Provider
// credentials deliberately stay in accounts.credentials and are never copied
// into account_credential_envelopes.
func credentialEnvelopeFromEnvironment() (*credentials.Envelope, error) {
	raw := strings.TrimSpace(os.Getenv("NEONIX_CREDENTIAL_KEY"))
	if raw == "" {
		return nil, nil
	}
	if len(raw) != 64 {
		return nil, errors.New("credential envelope key must be a 64-character hex value")
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, errors.New("credential envelope key is not valid hex")
	}
	codec, err := credentials.New(key)
	if err != nil {
		return nil, errors.New("credential envelope key is invalid")
	}
	return codec, nil
}

func (r *accountRepository) credentialEnvelopeConfigError() error {
	if r == nil || r.credentialCodecErr == nil {
		return nil
	}
	return errors.New("automation-secret envelope configuration is invalid")
}
