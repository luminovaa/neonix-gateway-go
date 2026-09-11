// Package legacy contains the deterministic, one-time conversion logic for
// Neonix's legacy Node account export. Database access stays in the command
// layer so the converter can be tested with redacted fixtures.
package legacy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/luminovaa/neonix-gateway-go/internal/provider"
	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
)

var (
	ErrMissingID          = errors.New("account id is required")
	ErrMissingProvider    = errors.New("account provider is required")
	ErrMissingCredentials = errors.New("active account credentials are missing")
	ErrInvalidCredentials = errors.New("account credentials are not valid JSON")
	ErrDuplicateID        = errors.New("duplicate account id")
)

// Account is the redacted-compatible shape emitted by the Node migration
// reader. Credentials are kept as raw bytes so migration never reserializes
// provider-specific JSON before encryption.
type Account struct {
	ID            string          `json:"id"`
	Provider      string          `json:"provider"`
	Type          string          `json:"type,omitempty"`
	Email         string          `json:"email,omitempty"`
	Nickname      string          `json:"nickname,omitempty"`
	IDP           string          `json:"idp,omitempty"`
	UserID        string          `json:"userId,omitempty"`
	GroupID       string          `json:"groupId,omitempty"`
	Tags          []string        `json:"tags,omitempty"`
	Status        string          `json:"status,omitempty"`
	LastError     string          `json:"lastError,omitempty"`
	IsActive      bool            `json:"isActive"`
	Enabled       *bool           `json:"enabled,omitempty"`
	CreatedAt     int64           `json:"createdAt,omitempty"`
	LastUsedAt    int64           `json:"lastUsedAt,omitempty"`
	LastCheckedAt *int64          `json:"lastCheckedAt,omitempty"`
	Credentials   json.RawMessage `json:"credentials"`
	Subscription  json.RawMessage `json:"subscription,omitempty"`
	Usage         json.RawMessage `json:"usage,omitempty"`
}

// NormalizedAccount is the target representation passed to the Go persistence
// writer. It deliberately contains an encrypted credential envelope only.
type NormalizedAccount struct {
	ID             string          `json:"id"`
	Provider       string          `json:"provider"`
	SourceProvider string          `json:"sourceProvider,omitempty"`
	Type           string          `json:"type"`
	Email          string          `json:"email,omitempty"`
	Nickname       string          `json:"nickname,omitempty"`
	IDP            string          `json:"idp,omitempty"`
	UserID         string          `json:"userId,omitempty"`
	GroupID        string          `json:"groupId,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	Status         string          `json:"status,omitempty"`
	LastError      string          `json:"lastError,omitempty"`
	IsActive       bool            `json:"isActive"`
	Enabled        *bool           `json:"enabled,omitempty"`
	CreatedAt      int64           `json:"createdAt,omitempty"`
	LastUsedAt     int64           `json:"lastUsedAt,omitempty"`
	LastCheckedAt  *int64          `json:"lastCheckedAt,omitempty"`
	Credential     string          `json:"credentialEnvelope"`
	Subscription   json.RawMessage `json:"subscription,omitempty"`
	Usage          json.RawMessage `json:"usage,omitempty"`
}

type Issue struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
	Severity string `json:"severity"`
	Code     string `json:"code"`
}

type Report struct {
	Total    int     `json:"total"`
	Migrated int     `json:"migrated"`
	Skipped  int     `json:"skipped"`
	Blocked  int     `json:"blocked"`
	Issues   []Issue `json:"issues,omitempty"`
}

type Options struct {
	DeprecatedProviders map[string]bool
}

func (a Account) normalized(codec *credentials.Envelope) (NormalizedAccount, error) {
	if a.ID == "" {
		return NormalizedAccount{}, ErrMissingID
	}
	if a.Provider == "" {
		return NormalizedAccount{}, ErrMissingProvider
	}
	if len(a.Credentials) == 0 || string(a.Credentials) == "null" {
		return NormalizedAccount{}, ErrMissingCredentials
	}
	if !json.Valid(a.Credentials) {
		return NormalizedAccount{}, ErrInvalidCredentials
	}
	ciphertext, err := codec.Seal(a.Credentials)
	if err != nil {
		return NormalizedAccount{}, fmt.Errorf("seal credentials: %w", err)
	}
	targetPlatform := provider.TargetPlatform(a.Provider)
	targetType := a.Type
	if targetType == "" {
		targetType = provider.TargetAccountType(a.Provider)
	}
	return NormalizedAccount{
		ID: a.ID, Provider: targetPlatform, SourceProvider: a.Provider, Type: targetType, Email: a.Email, Nickname: a.Nickname,
		IDP: a.IDP, UserID: a.UserID, GroupID: a.GroupID, Tags: append([]string(nil), a.Tags...),
		Status: a.Status, LastError: a.LastError, IsActive: a.IsActive, Enabled: a.Enabled,
		CreatedAt: a.CreatedAt, LastUsedAt: a.LastUsedAt, LastCheckedAt: a.LastCheckedAt,
		Credential: ciphertext, Subscription: append(json.RawMessage(nil), a.Subscription...),
		Usage: append(json.RawMessage(nil), a.Usage...),
	}, nil
}

// Convert validates and encrypts accounts without logging or returning raw
// credentials. Results are sorted by ID so dry-run reports are reproducible.
func Convert(accounts []Account, codec *credentials.Envelope, opts Options) ([]NormalizedAccount, Report) {
	result := make([]NormalizedAccount, 0, len(accounts))
	report := Report{Total: len(accounts)}
	seen := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		if _, exists := seen[account.ID]; exists && account.ID != "" {
			report.Blocked++
			report.Issues = append(report.Issues, Issue{ID: account.ID, Provider: account.Provider, Severity: "blocked", Code: "DUPLICATE_ACCOUNT_ID"})
			continue
		}
		if account.ID != "" {
			seen[account.ID] = struct{}{}
		}
		// Deprecated providers remain in the legacy archive but are not copied
		// into the active Go account store. This keeps historical data available
		// without allowing an old provider to re-enter routing accidentally.
		if opts.DeprecatedProviders[account.Provider] {
			report.Skipped++
			continue
		}
		converted, err := account.normalized(codec)
		if err != nil {
			if opts.DeprecatedProviders[account.Provider] {
				report.Skipped++
				continue
			}
			report.Blocked++
			report.Issues = append(report.Issues, Issue{ID: account.ID, Provider: account.Provider, Severity: "blocked", Code: issueCode(err)})
			continue
		}
		result = append(result, converted)
		report.Migrated++
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	sort.Slice(report.Issues, func(i, j int) bool {
		if report.Issues[i].ID == report.Issues[j].ID {
			return report.Issues[i].Code < report.Issues[j].Code
		}
		return report.Issues[i].ID < report.Issues[j].ID
	})
	return result, report
}

func issueCode(err error) string {
	switch {
	case errors.Is(err, ErrMissingID):
		return "ACCOUNT_ID_MISSING"
	case errors.Is(err, ErrMissingProvider):
		return "ACCOUNT_PROVIDER_MISSING"
	case errors.Is(err, ErrMissingCredentials):
		return "ACTIVE_CREDENTIAL_MISSING"
	case errors.Is(err, ErrInvalidCredentials):
		return "CREDENTIALS_INVALID_JSON"
	default:
		return "CREDENTIAL_ENCRYPTION_FAILED"
	}
}
