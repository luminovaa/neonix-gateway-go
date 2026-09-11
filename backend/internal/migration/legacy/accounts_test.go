package legacy

import (
	"encoding/json"
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
)

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
	plaintext, err := codec.Open(accounts[0].Credential)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != string(raw) {
		t.Fatalf("credential bytes changed: got %q want %q", plaintext, raw)
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
