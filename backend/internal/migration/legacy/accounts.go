// Package legacy contains the deterministic, one-time conversion logic for
// Neonix's legacy Node account export. Database access stays in the command
// layer so the converter can be tested with redacted fixtures.
package legacy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/luminovaa/neonix-gateway-go/internal/provider"
	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
)

var (
	ErrMissingID               = errors.New("account id is required")
	ErrMissingProvider         = errors.New("account provider is required")
	ErrMissingCredentials      = errors.New("active account credentials are missing")
	ErrInvalidCredentials      = errors.New("account credentials are not valid JSON")
	ErrInvalidAutomationSecret = errors.New("register automation secret is invalid")
	ErrInvalidGitHubSecret     = errors.New("GitHub identity automation secret is invalid")
	ErrDuplicateID             = errors.New("duplicate account id")
)

// Account is the redacted-compatible shape emitted by the Node migration
// reader. Credentials are kept as raw bytes so migration never reserializes
// provider-specific JSON before encryption.
type Account struct {
	ID               string          `json:"id"`
	Provider         string          `json:"provider"`
	Password         string          `json:"password,omitempty"`
	Type             string          `json:"type,omitempty"`
	Email            string          `json:"email,omitempty"`
	Nickname         string          `json:"nickname,omitempty"`
	IDP              string          `json:"idp,omitempty"`
	UserID           string          `json:"userId,omitempty"`
	GroupID          string          `json:"groupId,omitempty"`
	Tags             []string        `json:"tags,omitempty"`
	Status           string          `json:"status,omitempty"`
	LastError        string          `json:"lastError,omitempty"`
	IsActive         bool            `json:"isActive"`
	Enabled          *bool           `json:"enabled,omitempty"`
	CreatedAt        int64           `json:"createdAt,omitempty"`
	LastUsedAt       int64           `json:"lastUsedAt,omitempty"`
	LastCheckedAt    *int64          `json:"lastCheckedAt,omitempty"`
	GitHubCreatedAt  int64           `json:"githubCreatedAt,omitempty"`
	GitHubEligibleAt int64           `json:"githubEligibleAt,omitempty"`
	Cookies          json.RawMessage `json:"cookies,omitempty"`
	RawCookies       json.RawMessage `json:"rawCookies,omitempty"`
	UserAgent        string          `json:"userAgent,omitempty"`
	Proxy            string          `json:"proxy,omitempty"`
	Credentials      json.RawMessage `json:"credentials"`
	Subscription     json.RawMessage `json:"subscription,omitempty"`
	Usage            json.RawMessage `json:"usage,omitempty"`
}

// NormalizedAccount is the target representation passed to the Go persistence
// writer. Provider credentials remain in the normalized account only while the
// one-shot import runs, then are written directly to accounts.credentials.
// Automation-only secrets use separate encrypted envelopes so they never enter
// the credential document consumed by gateway adapters or a JSON artifact.
type NormalizedAccount struct {
	ID               string          `json:"id"`
	Provider         string          `json:"provider"`
	SourceProvider   string          `json:"sourceProvider,omitempty"`
	Type             string          `json:"type"`
	Email            string          `json:"email,omitempty"`
	Nickname         string          `json:"nickname,omitempty"`
	IDP              string          `json:"idp,omitempty"`
	UserID           string          `json:"userId,omitempty"`
	GroupID          string          `json:"groupId,omitempty"`
	Tags             []string        `json:"tags,omitempty"`
	Status           string          `json:"status,omitempty"`
	LastError        string          `json:"lastError,omitempty"`
	IsActive         bool            `json:"isActive"`
	Enabled          *bool           `json:"enabled,omitempty"`
	CreatedAt        int64           `json:"createdAt,omitempty"`
	LastUsedAt       int64           `json:"lastUsedAt,omitempty"`
	LastCheckedAt    *int64          `json:"lastCheckedAt,omitempty"`
	Credentials      json.RawMessage `json:"-"`
	AutomationSecret string          `json:"automationSecretEnvelope,omitempty"`
	GitHubSecret     string          `json:"githubSecretEnvelope,omitempty"`
	GitHubCreatedAt  int64           `json:"githubCreatedAt,omitempty"`
	GitHubEligibleAt int64           `json:"githubEligibleAt,omitempty"`
	Subscription     json.RawMessage `json:"subscription,omitempty"`
	Usage            json.RawMessage `json:"usage,omitempty"`
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
	// LegacyBYOKKey opens the Node AES-GCM githubSecret during the one-time
	// migration. It is never retained in the normalized artifact.
	LegacyBYOKKey []byte
}

func (a Account) normalized(codec *credentials.Envelope, opts Options) (NormalizedAccount, error) {
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
	credentialBytes := append(json.RawMessage(nil), a.Credentials...)
	automationPassword := ""
	githubSecretEnvelope := ""
	if strings.EqualFold(a.Provider, "grok") {
		automationPassword = a.Password
		var values map[string]any
		if err := json.Unmarshal(credentialBytes, &values); err != nil || values == nil {
			return NormalizedAccount{}, ErrInvalidCredentials
		}
		if strings.TrimSpace(automationPassword) == "" {
			for _, key := range []string{"relogin_password", "reloginPassword", "password", "clearTextPassword", "clear_text_password"} {
				if candidate, ok := values[key].(string); ok && strings.TrimSpace(candidate) != "" {
					automationPassword = candidate
					break
				}
			}
		}
		for _, key := range []string{"password", "relogin_password", "reloginPassword", "clearTextPassword", "clear_text_password"} {
			delete(values, key)
		}
		var err error
		credentialBytes, err = json.Marshal(values)
		if err != nil {
			return NormalizedAccount{}, ErrInvalidCredentials
		}
	}
	if strings.EqualFold(a.Provider, "github") {
		var values map[string]any
		if err := json.Unmarshal(credentialBytes, &values); err != nil || values == nil {
			return NormalizedAccount{}, ErrInvalidCredentials
		}
		secret, present, err := extractLegacyGitHubSecret(a, values, opts.LegacyBYOKKey)
		if err != nil {
			return NormalizedAccount{}, err
		}
		for _, key := range []string{"githubSecret", "github_secret", "password", "cookies", "rawCookies", "raw_cookies", "userAgent", "user_agent", "proxy"} {
			delete(values, key)
		}
		credentialBytes, err = json.Marshal(values)
		if err != nil {
			return NormalizedAccount{}, ErrInvalidCredentials
		}
		if present {
			if codec == nil {
				return NormalizedAccount{}, errors.New("credential codec is required for GitHub identity secrets")
			}
			secretBytes, err := json.Marshal(secret)
			if err != nil {
				return NormalizedAccount{}, ErrInvalidGitHubSecret
			}
			githubSecretEnvelope, err = codec.Seal(secretBytes)
			if err != nil {
				return NormalizedAccount{}, fmt.Errorf("seal GitHub identity secret: %w", err)
			}
		}
	}
	automationSecret := ""
	var err error
	if strings.TrimSpace(automationPassword) != "" {
		if codec == nil {
			return NormalizedAccount{}, errors.New("credential codec is required for automation secrets")
		}
		if len(automationPassword) > 4096 {
			return NormalizedAccount{}, ErrInvalidAutomationSecret
		}
		automationSecret, err = codec.Seal([]byte(automationPassword))
		if err != nil {
			return NormalizedAccount{}, fmt.Errorf("seal register automation secret: %w", err)
		}
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
		Credentials: append(json.RawMessage(nil), credentialBytes...), AutomationSecret: automationSecret, GitHubSecret: githubSecretEnvelope,
		GitHubCreatedAt: a.GitHubCreatedAt, GitHubEligibleAt: a.GitHubEligibleAt, Subscription: append(json.RawMessage(nil), a.Subscription...),
		Usage: append(json.RawMessage(nil), a.Usage...),
	}, nil
}

// CredentialsMap validates the in-memory provider JSON immediately before it
// is persisted. It deliberately accepts no envelope codec: provider
// credentials are not encrypted or written to a migration handoff artifact.
func (a NormalizedAccount) CredentialsMap() (map[string]any, error) {
	var credentials map[string]any
	if err := json.Unmarshal(a.Credentials, &credentials); err != nil || credentials == nil {
		return nil, ErrInvalidCredentials
	}
	return credentials, nil
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
		converted, err := account.normalized(codec, opts)
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
	case errors.Is(err, ErrInvalidAutomationSecret):
		return "AUTOMATION_SECRET_INVALID"
	case errors.Is(err, ErrInvalidGitHubSecret):
		return "GITHUB_IDENTITY_SECRET_INVALID"
	default:
		return "CREDENTIAL_ENCRYPTION_FAILED"
	}
}
