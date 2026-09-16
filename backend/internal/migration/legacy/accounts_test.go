package legacy

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
)

func sealNodeGitHubSecretForTest(t *testing.T, key, plaintext []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{7}, gcm.NonceSize())
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	ciphertext, tag := sealed[:len(sealed)-gcm.Overhead()], sealed[len(sealed)-gcm.Overhead():]
	legacy := append(append(append([]byte(nil), nonce...), tag...), ciphertext...)
	return base64.StdEncoding.EncodeToString(legacy)
}

func TestConvertPreservesCredentialBytesAndMetadata(t *testing.T) {
	codec, _ := credentials.New([]byte("01234567890123456789012345678901"))
	raw := json.RawMessage("{ \"refreshToken\": \"opaque\", \"expiresAt\": 123 }")
	accounts, report := Convert([]Account{{
		ID: "a1", Provider: "antigravity", Email: "user@example.com", GroupID: "default",
		Tags: []string{"primary"}, Status: "active", Credentials: raw,
	}}, codec, Options{})
	if report.Migrated != 1 || report.Blocked != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if accounts[0].Provider != "antigravity" || accounts[0].SourceProvider != "antigravity" || accounts[0].Type != "oauth" {
		t.Fatalf("provider target metadata was not normalized: %+v", accounts[0])
	}
	if accounts[0].GroupID != "default" || len(accounts[0].Tags) != 1 {
		t.Fatalf("metadata was not preserved: %+v", accounts[0])
	}
	if string(accounts[0].Credentials) != string(raw) {
		t.Fatalf("credential bytes changed: got %q want %q", accounts[0].Credentials, raw)
	}
}

func TestNormalizedAccountKeepsCredentialsOnlyInMemoryForRuntimeImport(t *testing.T) {
	codec, err := credentials.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	normalized, report := Convert([]Account{{
		ID: "a1", Provider: "antigravity", Credentials: json.RawMessage(`{"access_token":"secret","project_id":"p"}`),
	}}, codec, Options{})
	if report.Migrated != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	decoded, err := normalized[0].CredentialsMap()
	if err != nil {
		t.Fatal(err)
	}
	if decoded["access_token"] != "secret" || decoded["project_id"] != "p" {
		t.Fatalf("unexpected decoded credentials: %#v", decoded)
	}
}

func TestConvertSeparatesGrokReloginPasswordFromProviderCredentials(t *testing.T) {
	codec, err := credentials.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	password := "  exact password\t"
	normalized, report := Convert([]Account{{
		ID: "grok-1", Provider: "grok", Password: password,
		Credentials: json.RawMessage(`{"access_token":"access-secret","refresh_token":"refresh-secret","password":"stale","relogin_password":"stale-two"}`),
	}}, codec, Options{})
	if report.Migrated != 1 || len(normalized) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	decoded, err := normalized[0].CredentialsMap()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["password"]; ok {
		t.Fatalf("password leaked into provider credentials: %#v", decoded)
	}
	if _, ok := decoded["relogin_password"]; ok {
		t.Fatalf("relogin password leaked into provider credentials: %#v", decoded)
	}
	secret, err := codec.Open(normalized[0].AutomationSecret)
	if err != nil {
		t.Fatal(err)
	}
	if string(secret) != password {
		t.Fatalf("password bytes changed: got %q want %q", secret, password)
	}
	encoded, _ := json.Marshal(normalized[0])
	if bytes.Contains(encoded, []byte(password)) || bytes.Contains(encoded, []byte("access-secret")) {
		t.Fatalf("normalized artifact contains plaintext secret: %s", encoded)
	}
}

func TestConvertReencryptsNodeGitHubSecretAndStripsIdentitySecrets(t *testing.T) {
	codec, err := credentials.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	legacyKey := bytes.Repeat([]byte{4}, 32)
	plaintext := []byte(`{"password":"github-password","cookies":[{"name":"session","value":"github-cookie"}],"userAgent":"agent"}`)
	encrypted := sealNodeGitHubSecretForTest(t, legacyKey, plaintext)
	credentialsJSON, err := json.Marshal(map[string]any{"authMethod": "social", "githubSecret": encrypted, "password": "must-strip"})
	if err != nil {
		t.Fatal(err)
	}
	accounts, report := Convert([]Account{{
		ID: "github-1", Provider: "github", Email: "owner@example.com", Credentials: credentialsJSON,
		GitHubCreatedAt: 1735689600000, GitHubEligibleAt: 1735732800000,
	}}, codec, Options{LegacyBYOKKey: legacyKey})
	if report.Migrated != 1 || report.Blocked != 0 || len(accounts) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	providerCredentials, err := accounts[0].CredentialsMap()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"githubSecret", "password", "cookies", "rawCookies", "userAgent"} {
		if _, exists := providerCredentials[key]; exists {
			t.Fatalf("identity secret %q leaked into provider credentials: %#v", key, providerCredentials)
		}
	}
	secret, err := codec.Open(accounts[0].GitHubSecret)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secret, plaintext) {
		t.Fatalf("GitHub identity secret changed: got %s want %s", secret, plaintext)
	}
	if accounts[0].GitHubCreatedAt != 1735689600000 || accounts[0].GitHubEligibleAt != 1735732800000 {
		t.Fatalf("GitHub timestamps changed: %+v", accounts[0])
	}
}

func TestConvertGitHubPlaintextFallbackAndMissingLegacyKey(t *testing.T) {
	codec, _ := credentials.New(bytes.Repeat([]byte{8}, 32))
	plaintextAccounts, report := Convert([]Account{{
		ID: "github-plain", Provider: "github", Password: "github-password",
		Cookies: json.RawMessage(`{"session":"github-cookie"}`), Credentials: json.RawMessage(`{"authMethod":"social"}`),
	}}, codec, Options{})
	if report.Migrated != 1 || len(plaintextAccounts) != 1 || plaintextAccounts[0].GitHubSecret == "" {
		t.Fatalf("plaintext GitHub secret was not migrated: %+v", report)
	}

	legacyKey := bytes.Repeat([]byte{5}, 32)
	encrypted := sealNodeGitHubSecretForTest(t, legacyKey, []byte(`{"password":"p","cookies":[{"name":"s","value":"c"}]}`))
	credentialsJSON, _ := json.Marshal(map[string]any{"githubSecret": encrypted})
	_, blocked := Convert([]Account{{ID: "github-encrypted", Provider: "github", Credentials: credentialsJSON}}, codec, Options{})
	if blocked.Blocked != 1 || len(blocked.Issues) != 1 || blocked.Issues[0].Code != "GITHUB_IDENTITY_SECRET_INVALID" {
		t.Fatalf("missing legacy key did not block encrypted secret: %+v", blocked)
	}
}

func TestConvertBlocksInvalidActiveAndSkipsDeprecated(t *testing.T) {
	codec, _ := credentials.New([]byte("01234567890123456789012345678901"))
	accounts, report := Convert([]Account{
		{ID: "active", Provider: "codex", Status: "active", Credentials: json.RawMessage("not-json")},
		{ID: "deprecated", Provider: "bb", Status: "expired", Credentials: json.RawMessage("not-json")},
	}, codec, Options{DeprecatedProviders: map[string]bool{"bb": true}})
	if len(accounts) != 0 || report.Blocked != 1 || report.Skipped != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.Issues) != 1 || report.Issues[0].Code != "CREDENTIALS_INVALID_JSON" {
		t.Fatalf("unexpected issue: %+v", report.Issues)
	}
	encoded, _ := json.Marshal(report)
	if string(encoded) == "" || string(encoded) == "not-json" {
		t.Fatalf("report contains credential material: %s", encoded)
	}
}

func TestConvertBlocksDuplicateIDs(t *testing.T) {
	codec, _ := credentials.New([]byte("01234567890123456789012345678901"))
	_, report := Convert([]Account{
		{ID: "same", Provider: "kiro", Status: "active", Credentials: json.RawMessage("{}")},
		{ID: "same", Provider: "kiro", Status: "active", Credentials: json.RawMessage("{}")},
	}, codec, Options{})
	if report.Migrated != 1 || report.Blocked != 1 || report.Issues[0].Code != "DUPLICATE_ACCOUNT_ID" {
		t.Fatalf("unexpected duplicate report: %+v", report)
	}
}

func TestConvertMapsCodexAndOpenCodeToOpenAIStoragePlatform(t *testing.T) {
	codec, _ := credentials.New([]byte("01234567890123456789012345678901"))
	accounts, report := Convert([]Account{
		{ID: "codex", Provider: "codex", Credentials: json.RawMessage(`{"accessToken":"x"}`)},
		{ID: "oc", Provider: "oc", Credentials: json.RawMessage(`{"api_key":"x","base_url":"https://opencode.ai/zen/v1"}`)},
	}, codec, Options{})
	if report.Migrated != 2 || report.Blocked != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if accounts[0].Provider != "openai" || accounts[0].Type != "oauth" || accounts[0].SourceProvider != "codex" {
		t.Fatalf("codex mapping lost: %+v", accounts[0])
	}
	if accounts[1].Provider != "openai" || accounts[1].Type != "apikey" || accounts[1].SourceProvider != "oc" {
		t.Fatalf("opencode mapping lost: %+v", accounts[1])
	}
}
