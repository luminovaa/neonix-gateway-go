package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
	"github.com/luminovaa/neonix-gateway-go/internal/provider"
)

const (
	registerLegacyAccountIDKey = "neonix_legacy_account_id"
	registerSourceProviderKey  = "neonix_legacy_provider"
	registerEmailKey           = "neonix_legacy_email"
	registerIdentityKey        = "neonix_legacy_idp"
	registerTagsKey            = "neonix_legacy_tags"
	registerSubscriptionKey    = "neonix_legacy_subscription"
	registerUsageKey           = "neonix_legacy_usage"
)

type RegisterPersistence struct {
	admin       registerAccountWriter
	repo        registerAccountLookup
	maintenance registerMaintenanceLookup
}

type registerAccountWriter interface {
	GetAccount(context.Context, int64) (*Account, error)
	CreateAccount(context.Context, *CreateAccountInput) (*Account, error)
	UpdateAccount(context.Context, int64, *UpdateAccountInput) (*Account, error)
}

type registerAccountLookup interface {
	FindByExtraField(context.Context, string, any) ([]Account, error)
	ListRegisterAccountsByPlatform(context.Context, string) ([]Account, error)
}

type registerMaintenanceLookup interface {
	ListGrokReloginCandidates(context.Context) ([]RegisterReloginCandidate, int, error)
	GrokReloginCandidateSummary(context.Context) (RegisterReloginCandidateSummary, error)
	ListQoderInjectCandidates(context.Context) ([]RegisterQoderInjectCandidate, int, error)
	QoderInjectCandidateSummary(context.Context) (RegisterQoderInjectCandidateSummary, error)
	StoreRegisterAutomationPassword(context.Context, int64, string) error
	RestoreRegisterReloginAccount(context.Context, int64) error
	ListGitHubPickerAccounts(context.Context) ([]RegisterGitHubPickerAccount, error)
	ReserveGitHubAccounts(context.Context, []string, string) ([]PythonRegisterAccount, error)
	CompleteGitHubLink(context.Context, string, int64, string, *RegisterGitHubSessionUpdate) error
	ReleaseGitHubReservations(context.Context, string, string, string) error
	StoreGitHubIdentitySecret(context.Context, int64, RegisterGitHubIdentitySecret) error
}

type RegisterReloginCandidate struct {
	ID          string
	Email       string
	Password    string
	Provider    string
	Credentials map[string]any
}

type RegisterReloginCandidateSummary struct {
	Count         int
	TotalProvider int
}

type RegisterQoderInjectCandidate struct {
	ID          string
	Email       string
	Provider    string
	Credentials map[string]any
	IDP         string
	Nickname    string
	Tags        []string
	CreatedAt   int64
}

type RegisterQoderInjectCandidateSummary struct {
	Count         int
	TotalProvider int
}

type RegisterGitHubPickerAccount struct {
	ID                       string  `json:"id"`
	Email                    string  `json:"email"`
	Username                 string  `json:"username"`
	Status                   string  `json:"status"`
	Enabled                  bool    `json:"enabled"`
	GitHubCreatedAt          *int64  `json:"githubCreatedAt"`
	GitHubEligibleAt         *int64  `json:"githubEligibleAt"`
	AgeDays                  *int64  `json:"ageDays"`
	Eligible                 bool    `json:"eligible"`
	LinkStatus               *string `json:"linkStatus"`
	LinkedCodeBuddyAccountID *string `json:"linkedCodebuddyAccountId"`
	LastError                string  `json:"lastError,omitempty"`
}

type RegisterGitHubIdentitySecret struct {
	Password  string           `json:"password"`
	Cookies   []map[string]any `json:"cookies"`
	UserAgent string           `json:"userAgent,omitempty"`
	Proxy     string           `json:"proxy,omitempty"`
}

type RegisterGitHubSessionUpdate struct {
	Cookies   []map[string]any
	UserAgent string
}

type RegisterPersistResult struct {
	Account *Account
	Created bool
}

func NewRegisterPersistence(admin AdminService, repo AccountRepository) *RegisterPersistence {
	lookup, _ := repo.(registerAccountLookup)
	maintenance, _ := repo.(registerMaintenanceLookup)
	return &RegisterPersistence{admin: admin, repo: lookup, maintenance: maintenance}
}

// ListGrokReloginCandidates keeps replayable credentials inside the
// operator-to-worker boundary. The browser only receives the aggregate from
// GrokReloginCandidateSummary; passwords are never returned by an HTTP route.
func (s *RegisterPersistence) ListGrokReloginCandidates(ctx context.Context) ([]PythonRegisterAccount, error) {
	if s == nil || s.maintenance == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
	}
	candidates, _, err := s.maintenance.ListGrokReloginCandidates(ctx)
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_ACCOUNT_LOAD_FAILED", "failed to load Grok re-login accounts").WithCause(err)
	}
	result := make([]PythonRegisterAccount, 0, len(candidates))
	for i := range candidates {
		result = append(result, PythonRegisterAccount{
			ID: candidates[i].ID, Email: candidates[i].Email, Password: candidates[i].Password,
			Provider: candidates[i].Provider, Credentials: cloneRegisterMap(candidates[i].Credentials),
		})
	}
	return result, nil
}

func (s *RegisterPersistence) GrokReloginCandidateSummary(ctx context.Context) (RegisterReloginCandidateSummary, error) {
	if s == nil || s.maintenance == nil {
		return RegisterReloginCandidateSummary{}, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
	}
	summary, err := s.maintenance.GrokReloginCandidateSummary(ctx)
	if err != nil {
		return RegisterReloginCandidateSummary{}, infraerrors.New(http.StatusInternalServerError, "REGISTER_ACCOUNT_LOAD_FAILED", "failed to load Grok re-login accounts").WithCause(err)
	}
	return summary, nil
}

// ListQoderInjectCandidates returns only the PAT and non-secret identity
// metadata required by the internal Python worker. Other account credentials
// stay in Go and are merged back when the callback completes.
func (s *RegisterPersistence) ListQoderInjectCandidates(ctx context.Context) ([]PythonRegisterAccount, error) {
	if s == nil || s.maintenance == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
	}
	candidates, _, err := s.maintenance.ListQoderInjectCandidates(ctx)
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_ACCOUNT_LOAD_FAILED", "failed to load Qoder inject accounts").WithCause(err)
	}
	result := make([]PythonRegisterAccount, 0, len(candidates))
	for i := range candidates {
		result = append(result, PythonRegisterAccount{
			ID: candidates[i].ID, Email: candidates[i].Email, Provider: candidates[i].Provider,
			Credentials: cloneRegisterMap(candidates[i].Credentials), IDP: candidates[i].IDP,
			Nickname: candidates[i].Nickname, Tags: append([]string(nil), candidates[i].Tags...), CreatedAt: candidates[i].CreatedAt,
		})
	}
	return result, nil
}

func (s *RegisterPersistence) QoderInjectCandidateSummary(ctx context.Context) (RegisterQoderInjectCandidateSummary, error) {
	if s == nil || s.maintenance == nil {
		return RegisterQoderInjectCandidateSummary{}, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
	}
	summary, err := s.maintenance.QoderInjectCandidateSummary(ctx)
	if err != nil {
		return RegisterQoderInjectCandidateSummary{}, infraerrors.New(http.StatusInternalServerError, "REGISTER_ACCOUNT_LOAD_FAILED", "failed to load Qoder inject accounts").WithCause(err)
	}
	return summary, nil
}

// ListCodeBuddyChinaClaimCandidates keeps refresh credentials inside the
// operator-to-worker boundary. The browser selects claim mode but never
// supplies or receives an account token.
func (s *RegisterPersistence) ListCodeBuddyChinaClaimCandidates(ctx context.Context) ([]PythonRegisterAccount, error) {
	if s == nil || s.repo == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
	}
	accounts, err := s.repo.ListRegisterAccountsByPlatform(ctx, PlatformCodeBuddyChina)
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_ACCOUNT_LOAD_FAILED", "failed to load CodeBuddy China claim accounts").WithCause(err)
	}
	result := make([]PythonRegisterAccount, 0, len(accounts))
	for i := range accounts {
		refreshToken := firstRegisterCredentialString(accounts[i].Credentials, "refreshToken", "refresh_token")
		if refreshToken == "" {
			continue
		}
		legacyID, _ := accounts[i].Extra[registerLegacyAccountIDKey].(string)
		if strings.TrimSpace(legacyID) == "" {
			legacyID = strconv.FormatInt(accounts[i].ID, 10)
		}
		email, _ := accounts[i].Extra[registerEmailKey].(string)
		if strings.TrimSpace(email) == "" {
			email = firstRegisterCredentialString(accounts[i].Credentials, "email")
		}
		result = append(result, PythonRegisterAccount{
			ID: legacyID, Email: strings.TrimSpace(email), Provider: PlatformCodeBuddyChina,
			AccessToken:  firstRegisterCredentialString(accounts[i].Credentials, "accessToken", "access_token"),
			RefreshToken: refreshToken,
		})
	}
	return result, nil
}

func firstRegisterCredentialString(credentials map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := credentials[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *RegisterPersistence) ListGitHubPickerAccounts(ctx context.Context) ([]RegisterGitHubPickerAccount, error) {
	if s == nil || s.maintenance == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
	}
	accounts, err := s.maintenance.ListGitHubPickerAccounts(ctx)
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_ACCOUNT_LOAD_FAILED", "failed to load GitHub identities").WithCause(err)
	}
	return accounts, nil
}

func (s *RegisterPersistence) ReserveGitHubAccounts(ctx context.Context, ids []string, jobID string) ([]PythonRegisterAccount, error) {
	if s == nil || s.maintenance == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_ACCOUNT_LOAD_FAILED", "register account persistence is unavailable")
	}
	accounts, err := s.maintenance.ReserveGitHubAccounts(ctx, ids, jobID)
	if err != nil {
		return nil, infraerrors.Conflict("REGISTER_GITHUB_ACCOUNTS_UNAVAILABLE", "one or more GitHub identities are unavailable, linked, or not yet eligible").WithCause(err)
	}
	return accounts, nil
}

func (s *RegisterPersistence) ReleaseGitHubReservations(ctx context.Context, jobID, email, reasonCode string) error {
	if s == nil || s.maintenance == nil || strings.TrimSpace(jobID) == "" {
		return nil
	}
	if err := s.maintenance.ReleaseGitHubReservations(ctx, jobID, email, reasonCode); err != nil {
		return infraerrors.New(http.StatusInternalServerError, "REGISTER_GITHUB_RELEASE_FAILED", "failed to release GitHub identity reservation").WithCause(err)
	}
	return nil
}

func (s *RegisterPersistence) Persist(ctx context.Context, raw json.RawMessage) (*RegisterPersistResult, error) {
	return s.PersistForJob(ctx, raw, "")
}

// PersistForJob applies callback data with the server-owned automation job
// context. The worker account payload is deliberately not trusted to choose
// maintenance semantics.
func (s *RegisterPersistence) PersistForJob(ctx context.Context, raw json.RawMessage, jobType string) (*RegisterPersistResult, error) {
	return s.PersistForJobID(ctx, raw, jobType, "")
}

func (s *RegisterPersistence) PersistForJobID(ctx context.Context, raw json.RawMessage, jobType, jobID string) (*RegisterPersistResult, error) {
	if s == nil || s.admin == nil || s.repo == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_CALLBACK_PERSIST_FAILED", "register account persistence is unavailable")
	}
	account, err := decodeRegisterAccount(raw)
	if err != nil {
		return nil, err
	}
	existing, err := s.findExisting(ctx, account)
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_CALLBACK_PERSIST_FAILED", "failed to find the register account").WithCause(err)
	}
	if (account.Provider == "grok" || account.Provider == "github" || account.GitHubAccountID != "") && s.maintenance == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "REGISTER_AUTOMATION_SECRET_UNAVAILABLE", "register maintenance persistence is unavailable")
	}
	if existing != nil {
		extra := cloneRegisterMap(existing.Extra)
		applyRegisterMetadata(extra, account)
		credentials := cloneRegisterMap(existing.Credentials)
		for key, value := range account.Credentials {
			credentials[key] = value
		}
		credentials = SanitizeStoredCredentials(account.Platform, credentials)
		status := StatusActive
		if account.Provider == "grok" || (account.Provider == "qoder" && strings.EqualFold(strings.TrimSpace(jobType), "inject")) {
			// RestoreRegisterReloginAccount is the commit point that makes a
			// failed Grok account schedulable again. Keeping Status empty here
			// prevents a partial callback from exposing an active-but-still-
			// blocked account when secret storage or runtime cleanup fails. A
			// Qoder inject callback also preserves the operator's current status;
			// ordinary Qoder registration still activates the resulting account.
			status = ""
		}
		updated, err := s.admin.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{
			Type: account.Type, Credentials: credentials, Extra: extra, Status: status,
		})
		if err != nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_CALLBACK_PERSIST_FAILED", "failed to update the register account").WithCause(err)
		}
		if account.Provider == "grok" {
			if account.AutomationPassword != "" {
				if err := s.maintenance.StoreRegisterAutomationPassword(ctx, existing.ID, account.AutomationPassword); err != nil {
					return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_AUTOMATION_SECRET_SAVE_FAILED", "failed to save the register automation credential").WithCause(err)
				}
			}
			if err := s.maintenance.RestoreRegisterReloginAccount(ctx, existing.ID); err != nil {
				return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_CALLBACK_PERSIST_FAILED", "failed to restore the Grok account state").WithCause(err)
			}
			updated, err = s.admin.GetAccount(ctx, existing.ID)
			if err != nil {
				return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_CALLBACK_PERSIST_FAILED", "failed to reload the Grok account").WithCause(err)
			}
		}
		if account.Provider == "github" {
			if account.GitHubSecret == nil {
				return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "GitHub identity callback is missing its automation credential")
			}
			if err := s.maintenance.StoreGitHubIdentitySecret(ctx, existing.ID, *account.GitHubSecret); err != nil {
				return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_AUTOMATION_SECRET_SAVE_FAILED", "failed to save the GitHub identity credential").WithCause(err)
			}
		}
		if account.GitHubAccountID != "" {
			if strings.TrimSpace(jobID) == "" {
				return nil, infraerrors.Conflict("REGISTER_GITHUB_RESERVATION_MISSING", "GitHub identity reservation is missing")
			}
			if err := s.maintenance.CompleteGitHubLink(ctx, account.GitHubAccountID, existing.ID, jobID, account.GitHubSession); err != nil {
				return nil, infraerrors.Conflict("REGISTER_GITHUB_RESERVATION_MISSING", "GitHub identity reservation is missing or already used").WithCause(err)
			}
		}
		return &RegisterPersistResult{Account: updated}, nil
	}

	extra := make(map[string]any, 5)
	applyRegisterMetadata(extra, account)
	created, err := s.admin.CreateAccount(ctx, &CreateAccountInput{
		Name: account.Name, Platform: account.Platform, Type: account.Type, Credentials: account.Credentials, Extra: extra,
		Concurrency: 3, Priority: 50, SkipDefaultGroupBind: account.Provider == "github", SkipMixedChannelCheck: true,
	})
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_CALLBACK_PERSIST_FAILED", "failed to create the register account").WithCause(err)
	}
	if account.Provider == "grok" && account.AutomationPassword != "" {
		if err := s.maintenance.StoreRegisterAutomationPassword(ctx, created.ID, account.AutomationPassword); err != nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_AUTOMATION_SECRET_SAVE_FAILED", "failed to save the register automation credential").WithCause(err)
		}
	}
	if account.Provider == "github" {
		if account.GitHubSecret == nil {
			return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "GitHub identity callback is missing its automation credential")
		}
		if err := s.maintenance.StoreGitHubIdentitySecret(ctx, created.ID, *account.GitHubSecret); err != nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_AUTOMATION_SECRET_SAVE_FAILED", "failed to save the GitHub identity credential").WithCause(err)
		}
	}
	if account.GitHubAccountID != "" {
		if strings.TrimSpace(jobID) == "" {
			return nil, infraerrors.Conflict("REGISTER_GITHUB_RESERVATION_MISSING", "GitHub identity reservation is missing")
		}
		if err := s.maintenance.CompleteGitHubLink(ctx, account.GitHubAccountID, created.ID, jobID, account.GitHubSession); err != nil {
			return nil, infraerrors.Conflict("REGISTER_GITHUB_RESERVATION_MISSING", "GitHub identity reservation is missing or already used").WithCause(err)
		}
	}
	return &RegisterPersistResult{Account: created, Created: true}, nil
}

type normalizedRegisterAccount struct {
	LegacyID           string
	Provider           string
	Platform           string
	Type               string
	Email              string
	Name               string
	Identity           string
	Credentials        map[string]any
	AutomationPassword string
	GitHubSecret       *RegisterGitHubIdentitySecret
	GitHubAccountID    string
	GitHubSession      *RegisterGitHubSessionUpdate
	GitHubCreatedAt    int64
	GitHubEligibleAt   int64
	Tags               []string
	Subscription       map[string]any
	Usage              map[string]any
}

func decodeRegisterAccount(raw json.RawMessage) (*normalizedRegisterAccount, error) {
	if len(raw) == 0 || len(raw) > registerRuntimeBodyLimit {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "invalid register account payload")
	}
	var value map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "invalid register account payload")
	}
	if value["linkedIdentity"] != nil {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_UNSUPPORTED_IDENTITY", "linked identity registration is not available in the Go backend yet")
	}
	sourceProvider := strings.ToLower(registerString(value, "provider"))
	definition, ok := provider.Lookup(sourceProvider)
	if !ok || definition.Deprecated || sourceProvider == "byok" {
		return nil, infraerrors.BadRequest("REGISTER_PROVIDER_INVALID", "unsupported register provider")
	}
	targetPlatform := definition.TargetPlatform
	if targetPlatform == "" {
		targetPlatform = sourceProvider
	}
	githubAccountID := boundedRegisterString(registerString(value, "githubAccountId"), 256)
	credentials, _ := value["credentials"].(map[string]any)
	credentials = cloneRegisterMap(credentials)
	var githubSecret *RegisterGitHubIdentitySecret
	var githubSession *RegisterGitHubSessionUpdate
	automationPassword := rawRegisterString(value, "password")
	if strings.TrimSpace(automationPassword) == "" {
		automationPassword = firstRawStringValue(credentials, "relogin_password", "password")
	}
	if sourceProvider != "grok" {
		automationPassword = ""
	} else if strings.TrimSpace(automationPassword) == "" {
		automationPassword = ""
	} else if len(automationPassword) > 4096 {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "register automation credential is too long")
	}
	if sourceProvider == "github" {
		secret, err := decodeRegisterGitHubSecret(value, credentials)
		if err != nil {
			return nil, err
		}
		githubSecret = secret
		if username := boundedRegisterString(firstRawStringValue(value, "githubUsername"), 256); username != "" {
			credentials["github_username"] = username
		}
	}
	if githubAccountID != "" {
		if sourceProvider != "codebuddy" || !strings.EqualFold(registerString(value, "idp"), "Github") {
			return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "invalid GitHub CodeBuddy link")
		}
		if session, err := decodeRegisterGitHubSession(value); err != nil {
			return nil, err
		} else {
			githubSession = session
		}
	}
	for source, target := range map[string]string{
		"accessToken": "access_token", "refreshToken": "refresh_token", "idToken": "id_token",
		"clientId": "client_id", "clientSecret": "client_secret", "expiresAt": "expires_at",
		"projectId": "project_id", "projectID": "project_id", "accountId": "account_id",
		"apiKey": "api_key", "authMethod": "auth_method", "machineId": "machine_id",
		"userId": "user_id", "sessionToken": "session_token", "serviceToken": "service_token", "phToken": "ph_token",
	} {
		if candidate, exists := credentials[source]; exists && candidate != nil {
			if _, hasCanonical := credentials[target]; !hasCanonical {
				credentials[target] = candidate
			}
			delete(credentials, source)
		}
	}
	for source, target := range map[string]string{
		"accessToken": "access_token", "refreshToken": "refresh_token", "idToken": "id_token",
		"clientId": "client_id", "clientSecret": "client_secret", "expiresAt": "expires_at",
		"projectId": "project_id", "projectID": "project_id", "accountId": "account_id",
		"apiKey": "api_key", "authMethod": "auth_method", "machineId": "machine_id",
		"userId": "user_id", "sessionToken": "session_token", "serviceToken": "service_token", "phToken": "ph_token",
	} {
		if candidate, exists := value[source]; exists && candidate != nil {
			credentials[target] = candidate
		}
	}
	if token := registerString(value, "token"); token != "" {
		credentials["token"] = token
	}
	if len(credentials) == 0 || !validRegisterCredentialValue(credentials, 0) {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "register account credentials are missing or invalid")
	}
	credentials = SanitizeStoredCredentials(targetPlatform, credentials)
	for _, key := range []string{"githubSecret", "github_secret", "password", "userAgent", "user_agent", "proxy"} {
		delete(credentials, key)
	}
	delete(credentials, "rawCookies")
	delete(credentials, "cookies")
	email := boundedRegisterString(registerString(value, "email"), 320)
	if email == "" {
		email = boundedRegisterString(registerString(value, "username"), 320)
	}
	legacyID := boundedRegisterString(registerString(value, "id"), 256)
	if email == "" && legacyID == "" {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "register account identity is missing")
	}
	name := boundedRegisterString(registerString(value, "nickname"), maxAccountNameRunes)
	if name == "" {
		name = boundedRegisterString(email, maxAccountNameRunes)
	}
	if name == "" {
		name = sourceProvider + "-registered"
	}
	accountType := definition.AccountType
	if rawType := strings.TrimSpace(registerString(value, "type")); rawType != "" {
		switch rawType {
		case AccountTypeOAuth, AccountTypeAPIKey, AccountTypeSetupToken:
			accountType = rawType
		default:
			return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "invalid register account type")
		}
	}
	tags, err := registerStringSlice(value, "tags", 128, 160)
	if err != nil {
		return nil, err
	}
	subscription, err := registerMetadataMap(value, "subscription")
	if err != nil {
		return nil, err
	}
	usage, err := registerMetadataMap(value, "usage")
	if err != nil {
		return nil, err
	}
	return &normalizedRegisterAccount{
		LegacyID: legacyID, Provider: sourceProvider, Platform: targetPlatform, Type: accountType, Email: email, Name: name,
		Identity: boundedRegisterString(registerString(value, "idp"), 80), Credentials: credentials,
		AutomationPassword: automationPassword, GitHubSecret: githubSecret, GitHubAccountID: githubAccountID, GitHubSession: githubSession,
		GitHubCreatedAt: registerInt64(value, "githubCreatedAt"), GitHubEligibleAt: registerInt64(value, "githubEligibleAt"),
		Tags: tags, Subscription: subscription, Usage: usage,
	}, nil
}

func (s *RegisterPersistence) findExisting(ctx context.Context, account *normalizedRegisterAccount) (*Account, error) {
	if account.LegacyID != "" {
		matches, err := s.repo.FindByExtraField(ctx, registerLegacyAccountIDKey, account.LegacyID)
		if err != nil {
			return nil, err
		}
		if len(matches) > 0 {
			return &matches[0], nil
		}
	}
	if account.Email == "" {
		return nil, nil
	}
	items, err := s.repo.ListRegisterAccountsByPlatform(ctx, account.Platform)
	if err != nil {
		return nil, err
	}
	for i := range items {
		source := strings.ToLower(strings.TrimSpace(registerMapString(items[i].Extra, registerSourceProviderKey)))
		if source == "" {
			source = strings.ToLower(items[i].Platform)
		}
		if source == account.Provider && strings.EqualFold(strings.TrimSpace(registerMapString(items[i].Extra, registerEmailKey)), account.Email) {
			return &items[i], nil
		}
	}
	return nil, nil
}

func applyRegisterMetadata(extra map[string]any, account *normalizedRegisterAccount) {
	if account.LegacyID != "" {
		extra[registerLegacyAccountIDKey] = account.LegacyID
	}
	extra[registerSourceProviderKey] = account.Provider
	if account.Email != "" {
		extra[registerEmailKey] = account.Email
	}
	if account.Identity != "" {
		extra[registerIdentityKey] = account.Identity
	}
	if account.Tags != nil {
		extra[registerTagsKey] = append([]string(nil), account.Tags...)
	}
	if account.Subscription != nil {
		extra[registerSubscriptionKey] = cloneRegisterMap(account.Subscription)
	}
	if account.Usage != nil {
		extra[registerUsageKey] = cloneRegisterMap(account.Usage)
	}
	extra["register_source"] = "python-automation"
	if account.GitHubCreatedAt > 0 {
		extra["github_created_at_ms"] = account.GitHubCreatedAt
	}
	if account.GitHubEligibleAt > 0 {
		extra["github_eligible_at_ms"] = account.GitHubEligibleAt
	}
}

func registerStringSlice(value map[string]any, key string, maxItems, maxRunes int) ([]string, error) {
	raw, exists := value[key]
	if !exists || raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok || len(items) > maxItems {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "register account metadata is invalid")
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || utf8.RuneCountInString(text) > maxRunes {
			return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "register account metadata is invalid")
		}
		result = append(result, text)
	}
	return result, nil
}

func registerMetadataMap(value map[string]any, key string) (map[string]any, error) {
	raw, exists := value[key]
	if !exists || raw == nil {
		return nil, nil
	}
	metadata, ok := raw.(map[string]any)
	if !ok || !validRegisterCredentialValue(metadata, 0) {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "register account metadata is invalid")
	}
	return cloneRegisterMap(metadata), nil
}

func registerString(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return strings.TrimSpace(result)
}

func rawRegisterString(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func firstRawStringValue(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result, ok := value[key].(string); ok && strings.TrimSpace(result) != "" {
			return result
		}
	}
	return ""
}

func registerMapString(value map[string]any, key string) string {
	if value == nil {
		return ""
	}
	result, _ := value[key].(string)
	return result
}

func cloneRegisterMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func boundedRegisterString(value string, max int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}

func validRegisterCredentialValue(value any, depth int) bool {
	if depth > 8 {
		return false
	}
	switch typed := value.(type) {
	case nil, bool, json.Number:
		return true
	case string:
		return len(typed) <= registerRuntimeBodyLimit
	case []any:
		if len(typed) > 2048 {
			return false
		}
		for _, item := range typed {
			if !validRegisterCredentialValue(item, depth+1) {
				return false
			}
		}
		return true
	case map[string]any:
		if len(typed) > 512 {
			return false
		}
		for key, item := range typed {
			if len(key) > 256 || !validRegisterCredentialValue(item, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func RegisterPersistenceErrorCode(err error) string {
	var appErr *infraerrors.ApplicationError
	if errors.As(err, &appErr) && appErr.Reason != "" {
		return appErr.Reason
	}
	return "REGISTER_CALLBACK_PERSIST_FAILED"
}
