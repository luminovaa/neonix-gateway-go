// Command migrate performs the one-time legacy Node account and gateway-key
// conversion.
// It is intentionally file-based first: the production wrapper can export a
// consistent PostgreSQL snapshot, run this command in dry-run mode, and then
// apply the verified normalized rows in one transaction.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/luminovaa/neonix-gateway-go/internal/migration/legacy"
	"github.com/luminovaa/neonix-gateway-go/internal/provider"
	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
)

type sourceEnvelope struct {
	Accounts []legacy.Account           `json:"accounts"`
	APIKeys  []legacy.APIKey            `json:"apiKeys"`
	Settings map[string]json.RawMessage `json:"settings"`
	// api_keys is accepted for exports produced by SQL tooling that preserves
	// the database column naming convention.
	LegacyAPIKeys []legacy.APIKey `json:"api_keys"`
}

func main() {
	sourcePath := flag.String("source", "", "path to an account array or {\"accounts\": [...], \"apiKeys\": [...], \"settings\": {...}} export")
	outPath := flag.String("output", "", "write normalized encrypted accounts (requires --apply; optional with --dsn)")
	dsn := flag.String("dsn", "", "PostgreSQL DSN for direct transactional import (or NEONIX_MIGRATION_DSN)")
	apply := flag.Bool("apply", false, "write normalized rows instead of dry-run report")
	deprecated := flag.String("deprecated", strings.Join(provider.DeprecatedIDs(), ","), "comma-separated providers that may be archived")
	flag.Parse()

	if strings.TrimSpace(*sourcePath) == "" {
		fatal("--source is required")
	}
	keyHex := strings.TrimSpace(os.Getenv("NEONIX_CREDENTIAL_KEY"))
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		fatal("NEONIX_CREDENTIAL_KEY must be a 64-character hex key")
	}
	codec, err := credentials.New(key)
	if err != nil {
		fatal("credential codec unavailable: %v", err)
	}
	accounts, apiKeys, settings, err := readMigrationSource(*sourcePath)
	if err != nil {
		fatal("read migration source: %v", err)
	}
	deprecatedProviders := make(map[string]bool)
	for _, provider := range strings.Split(*deprecated, ",") {
		if provider = strings.TrimSpace(provider); provider != "" {
			deprecatedProviders[provider] = true
		}
	}
	normalized, report := legacy.Convert(accounts, codec, legacy.Options{DeprecatedProviders: deprecatedProviders})
	normalizedAPIKeys, apiKeyReport := legacy.NormalizeAPIKeys(apiKeys, time.Now)
	normalizedSettings, settingsReport := legacy.NormalizeSettings(settings)
	if report.Blocked > 0 {
		writeReport(report)
		os.Exit(2)
	}
	if apiKeyReport.Failed > 0 {
		writeReport(apiKeyReport)
		os.Exit(2)
	}
	if settingsReport.Failed > 0 {
		writeReport(settingsReport)
		os.Exit(2)
	}
	if !*apply {
		writeReport(report)
		if len(apiKeys) > 0 {
			writeReport(apiKeyReport)
		}
		if len(settings) > 0 {
			writeReport(settingsReport)
		}
		return
	}
	effectiveDSN := strings.TrimSpace(*dsn)
	if effectiveDSN == "" {
		effectiveDSN = strings.TrimSpace(os.Getenv("NEONIX_MIGRATION_DSN"))
	}
	if strings.TrimSpace(*outPath) == "" && effectiveDSN == "" {
		fatal("--output or --dsn is required with --apply")
	}
	if strings.TrimSpace(*outPath) != "" {
		data, err := json.MarshalIndent(struct {
			Version  int                        `json:"version"`
			Accounts []legacy.NormalizedAccount `json:"accounts"`
		}{Version: 1, Accounts: normalized}, "", "  ")
		if err != nil {
			fatal("encode normalized accounts: %v", err)
		}
		if err := os.WriteFile(*outPath, append(data, '\n'), 0o600); err != nil {
			fatal("write normalized accounts: %v", err)
		}
		sum := sha256.Sum256(data)
		fmt.Printf("normalized_output_sha256=%s\n", hex.EncodeToString(sum[:]))
	}
	if effectiveDSN == "" {
		writeReport(report)
		if len(apiKeys) > 0 {
			writeReport(apiKeyReport)
		}
		if len(settings) > 0 {
			writeReport(settingsReport)
		}
		return
	}
	importCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db, err := sql.Open("postgres", effectiveDSN)
	if err != nil {
		fatal("open migration database: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(importCtx); err != nil {
		fatal("connect migration database")
	}
	importReport, err := legacy.ImportIntoPostgres(importCtx, db, normalized, codec, legacy.ImportOptions{})
	if err != nil {
		if errors.Is(err, legacy.ErrImportBlocked) {
			writeImportReport(importReport)
			os.Exit(2)
		}
		fatal("import normalized accounts: %v", err)
	}
	writeReport(report)
	data, _ := json.Marshal(importReport)
	fmt.Printf("database_import=%s\n", data)
	if len(normalizedAPIKeys) > 0 {
		keyImportReport, err := legacy.ImportAPIKeysIntoPostgres(importCtx, db, normalizedAPIKeys, time.Now)
		if err != nil {
			fatal("import API keys: %v", err)
		}
		keyData, _ := json.Marshal(keyImportReport)
		fmt.Printf("api_key_database_import=%s\n", keyData)
	}
	if len(normalizedSettings) > 0 {
		settingsImportReport, err := legacy.ImportSettingsIntoPostgres(importCtx, db, normalizedSettings, time.Now)
		if err != nil {
			fatal("import settings: %v", err)
		}
		settingsData, _ := json.Marshal(settingsImportReport)
		fmt.Printf("settings_database_import=%s\n", settingsData)
	}
}

func readMigrationSource(path string) ([]legacy.Account, []legacy.APIKey, map[string]json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, nil, err
	}
	var accounts []legacy.Account
	if err := json.Unmarshal(data, &accounts); err == nil {
		return accounts, nil, nil, nil
	}
	var envelope sourceEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, nil, nil, errors.New("source must be a JSON account array or object with accounts")
	}
	if len(envelope.APIKeys) == 0 {
		envelope.APIKeys = envelope.LegacyAPIKeys
	}
	return envelope.Accounts, envelope.APIKeys, envelope.Settings, nil
}

func writeReport(report any) {
	writeJSONReport(report)
}

func writeImportReport(report legacy.ImportReport) {
	writeJSONReport(report)
}

func writeJSONReport(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		fatal("encode migration report: %v", err)
	}
	fmt.Println(string(data))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "migration error: "+format+"\n", args...)
	os.Exit(1)
}
