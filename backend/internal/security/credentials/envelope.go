// Package credentials contains the only wire format used for provider
// credentials stored by the Neonix Go backend.
package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	currentVersion = 1
	prefix         = "neonix-credential-v1."
)

var (
	ErrInvalidKey         = errors.New("credential envelope key must be 32 bytes")
	ErrInvalidEnvelope    = errors.New("invalid credential envelope")
	ErrUnsupportedVersion = errors.New("unsupported credential envelope version")
)

// Envelope encrypts provider credentials with AES-256-GCM. The encoded value
// contains a version prefix followed by nonce+ciphertext+authentication tag.
// A new random nonce is generated for every call to Seal.
type Envelope struct {
	key []byte
}

// New creates an envelope codec. The key is copied so callers can rotate or
// wipe their input without changing an already-created codec.
func New(key []byte) (*Envelope, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	copyKey := make([]byte, len(key))
	copy(copyKey, key)
	return &Envelope{key: copyKey}, nil
}

// Seal encrypts plaintext and returns a versioned, database-safe string.
func (e *Envelope) Seal(plaintext []byte) (string, error) {
	if e == nil || len(e.key) != 32 {
		return "", ErrInvalidKey
	}
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("create credential cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create credential gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate credential nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, []byte(prefix))
	return prefix + base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

// Open decrypts a versioned envelope. It returns a copy of the plaintext and
// never logs or includes secret material in returned errors.
func (e *Envelope) Open(encoded string) ([]byte, error) {
	if e == nil || len(e.key) != 32 {
		return nil, ErrInvalidKey
	}
	if !strings.HasPrefix(encoded, prefix) {
		if strings.HasPrefix(encoded, "neonix-credential-v") {
			return nil, ErrUnsupportedVersion
		}
		return nil, ErrInvalidEnvelope
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encoded, prefix))
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(raw) < gcm.NonceSize() {
		return nil, ErrInvalidEnvelope
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(prefix))
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	return plaintext, nil
}
