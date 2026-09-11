package credentials

import (
	"bytes"
	"errors"
	"testing"
)

func testKey(seed byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

func TestEnvelopeRoundTripPreservesCredentialBytes(t *testing.T) {
	codec, err := New(testKey(7))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"accessToken":"opaque","refreshToken":"refresh","expiresAt":123}`)
	ciphertext, err := codec.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix([]byte(ciphertext), []byte(prefix)) {
		t.Fatalf("missing version prefix: %q", ciphertext)
	}
	got, err := codec.Open(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("credential bytes changed: got %q want %q", got, plaintext)
	}
}

func TestEnvelopeUsesFreshNonce(t *testing.T) {
	codec, _ := New(testKey(1))
	one, err := codec.Seal([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := codec.Seal([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Fatal("same plaintext produced the same ciphertext")
	}
}

func TestEnvelopeRejectsInvalidKeyAndEnvelope(t *testing.T) {
	if _, err := New(make([]byte, 31)); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("wrong key error = %v", err)
	}
	codec, _ := New(testKey(2))
	if _, err := codec.Open("neonix-credential-v2.payload"); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version error = %v", err)
	}
	if _, err := codec.Open("neonix-credential-v1.not-base64"); !errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("malformed error = %v", err)
	}
	other, _ := New(testKey(3))
	ciphertext, _ := codec.Seal([]byte("secret"))
	if _, err := other.Open(ciphertext); !errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("wrong key error = %v", err)
	}
}
