package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
)

type RegisterPersistence struct {
	admin registerAccountWriter
	repo  registerAccountLookup
}

type registerAccountWriter interface {
	CreateAccount(context.Context, *CreateAccountInput) (*Account, error)
	UpdateAccount(context.Context, int64, *UpdateAccountInput) (*Account, error)
}

type registerAccountLookup interface {
	FindByExtraField(context.Context, string, any) ([]Account, error)
	ListByPlatform(context.Context, string) ([]Account, error)
}

type RegisterPersistResult struct {
	Account *Account
	Created bool
}

func NewRegisterPersistence(admin AdminService, repo AccountRepository) *RegisterPersistence {
	return &RegisterPersistence{admin: admin, repo: repo}
}

func (s *RegisterPersistence) Persist(ctx context.Context, raw json.RawMessage) (*RegisterPersistResult, error) {
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
	if existing != nil {
		extra := cloneRegisterMap(existing.Extra)
		applyRegisterMetadata(extra, account)
		updated, err := s.admin.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{
			Type: account.Type, Credentials: account.Credentials, Extra: extra, Status: StatusActive,
		})
		if err != nil {
			return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_CALLBACK_PERSIST_FAILED", "failed to update the register account").WithCause(err)
		}
		return &RegisterPersistResult{Account: updated}, nil
	}

	extra := make(map[string]any, 5)
	applyRegisterMetadata(extra, account)
	created, err := s.admin.CreateAccount(ctx, &CreateAccountInput{
		Name: account.Name, Platform: account.Platform, Type: account.Type, Credentials: account.Credentials, Extra: extra,
		Concurrency: 3, Priority: 50, SkipMixedChannelCheck: true,
	})
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "REGISTER_CALLBACK_PERSIST_FAILED", "failed to create the register account").WithCause(err)
	}
	return &RegisterPersistResult{Account: created, Created: true}, nil
}

type normalizedRegisterAccount struct {
	LegacyID    string
	Provider    string
	Platform    string
	Type        string
	Email       string
	Name        string
	Identity    string
	Credentials map[string]any
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
	if value["linkedIdentity"] != nil || registerString(value, "githubAccountId") != "" {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_UNSUPPORTED_IDENTITY", "linked identity registration is not available in the Go backend yet")
	}
	sourceProvider := strings.ToLower(registerString(value, "provider"))
	definition, ok := provider.Lookup(sourceProvider)
	if !ok || definition.Deprecated || sourceProvider == "byok" {
		return nil, infraerrors.BadRequest("REGISTER_PROVIDER_INVALID", "unsupported register provider")
	}
	if sourceProvider == "github" {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_UNSUPPORTED_IDENTITY", "GitHub identity persistence is not available in the Go backend yet")
	}
	credentials, _ := value["credentials"].(map[string]any)
	credentials = cloneRegisterMap(credentials)
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
	credentials = SanitizeStoredCredentials(definition.TargetPlatform, credentials)
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
	return &normalizedRegisterAccount{
		LegacyID: legacyID, Provider: sourceProvider, Platform: definition.TargetPlatform, Type: accountType, Email: email, Name: name,
		Identity: boundedRegisterString(registerString(value, "idp"), 80), Credentials: credentials,
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
	items, err := s.repo.ListByPlatform(ctx, account.Platform)
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
	extra["register_source"] = "python-automation"
}

func registerString(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return strings.TrimSpace(result)
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
