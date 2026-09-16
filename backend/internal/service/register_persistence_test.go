package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type registerWriterStub struct {
	created  *CreateAccountInput
	updated  *UpdateAccountInput
	updateID int64
	updates  int
	account  *Account
}

func (w *registerWriterStub) GetAccount(_ context.Context, id int64) (*Account, error) {
	if w.account != nil {
		copy := *w.account
		return &copy, nil
	}
	return &Account{ID: id, Status: StatusActive, Schedulable: true}, nil
}

func (w *registerWriterStub) CreateAccount(_ context.Context, input *CreateAccountInput) (*Account, error) {
	w.created = input
	return &Account{ID: 41, Name: input.Name, Platform: input.Platform, Type: input.Type, Credentials: input.Credentials, Extra: input.Extra, Status: StatusActive}, nil
}

func (w *registerWriterStub) UpdateAccount(_ context.Context, id int64, input *UpdateAccountInput) (*Account, error) {
	w.updateID, w.updated = id, input
	w.updates++
	return &Account{ID: id, Platform: PlatformAntigravity, Credentials: input.Credentials, Extra: input.Extra, Status: StatusActive}, nil
}

type registerLookupStub struct {
	byLegacy   []Account
	byPlatform []Account
}

type registerMaintenanceStub struct {
	storedID          int64
	storedPassword    string
	restoredID        int64
	storeCalls        int
	restoreCalls      int
	storeFailures     int
	restoreFailures   int
	summary           RegisterReloginCandidateSummary
	injectSummary     RegisterQoderInjectCandidateSummary
	githubSecret      RegisterGitHubIdentitySecret
	githubSecretID    int64
	githubSecretCalls int
	completedGitHubID string
	completedCodeID   int64
	completedJobID    string
	completedSession  *RegisterGitHubSessionUpdate
}

func (s *registerMaintenanceStub) ListGrokReloginCandidates(context.Context) ([]RegisterReloginCandidate, int, error) {
	return nil, 0, nil
}

func (s *registerMaintenanceStub) GrokReloginCandidateSummary(context.Context) (RegisterReloginCandidateSummary, error) {
	return s.summary, nil
}

func (s *registerMaintenanceStub) ListQoderInjectCandidates(context.Context) ([]RegisterQoderInjectCandidate, int, error) {
	return nil, 0, nil
}

func (s *registerMaintenanceStub) QoderInjectCandidateSummary(context.Context) (RegisterQoderInjectCandidateSummary, error) {
	return s.injectSummary, nil
}

func (s *registerMaintenanceStub) StoreRegisterAutomationPassword(_ context.Context, id int64, password string) error {
	s.storeCalls++
	if s.storeFailures > 0 {
		s.storeFailures--
		return errors.New("temporary secret storage failure")
	}
	s.storedID, s.storedPassword = id, password
	return nil
}

func (s *registerMaintenanceStub) RestoreRegisterReloginAccount(_ context.Context, id int64) error {
	s.restoreCalls++
	if s.restoreFailures > 0 {
		s.restoreFailures--
		return errors.New("temporary restore failure")
	}
	s.restoredID = id
	return nil
}

func (s *registerMaintenanceStub) ListGitHubPickerAccounts(context.Context) ([]RegisterGitHubPickerAccount, error) {
	return nil, nil
}

func (s *registerMaintenanceStub) ReserveGitHubAccounts(context.Context, []string, string) ([]PythonRegisterAccount, error) {
	return nil, nil
}

func (s *registerMaintenanceStub) CompleteGitHubLink(_ context.Context, githubID string, codeBuddyID int64, jobID string, session *RegisterGitHubSessionUpdate) error {
	s.completedGitHubID = githubID
	s.completedCodeID = codeBuddyID
	s.completedJobID = jobID
	s.completedSession = session
	return nil
}

func (s *registerMaintenanceStub) ReleaseGitHubReservations(context.Context, string, string, string) error {
	return nil
}

func (s *registerMaintenanceStub) StoreGitHubIdentitySecret(_ context.Context, id int64, secret RegisterGitHubIdentitySecret) error {
	s.githubSecretCalls++
	s.githubSecretID = id
	s.githubSecret = secret
	return nil
}

func (r *registerLookupStub) FindByExtraField(_ context.Context, _ string, _ any) ([]Account, error) {
	return r.byLegacy, nil
}
func (r *registerLookupStub) ListRegisterAccountsByPlatform(_ context.Context, _ string) ([]Account, error) {
	return r.byPlatform, nil
}

func TestRegisterPersistenceCreatesNormalizedEncryptedAccountInput(t *testing.T) {
	writer := &registerWriterStub{}
	persistence := &RegisterPersistence{admin: writer, repo: &registerLookupStub{}}
	result, err := persistence.Persist(context.Background(), json.RawMessage(`{
		"id":"legacy-1","provider":"antigravity","email":"owner@example.com","nickname":"Owner",
		"credentials":{"accessToken":"access-secret","refreshToken":"refresh-secret","projectID":"project-1","password":"must-strip"}
	}`))
	require.NoError(t, err)
	require.True(t, result.Created)
	require.Equal(t, PlatformAntigravity, writer.created.Platform)
	require.Equal(t, AccountTypeOAuth, writer.created.Type)
	require.Equal(t, "access-secret", writer.created.Credentials["access_token"])
	require.Equal(t, "refresh-secret", writer.created.Credentials["refresh_token"])
	require.Equal(t, "project-1", writer.created.Credentials["project_id"])
	require.NotContains(t, writer.created.Credentials, "accessToken")
	require.NotContains(t, writer.created.Credentials, "password")
	require.Equal(t, "legacy-1", writer.created.Extra[registerLegacyAccountIDKey])
	require.Equal(t, "owner@example.com", writer.created.Extra[registerEmailKey])
}

func TestRegisterPersistenceUpdatesExistingByLegacyIDAndPreservesMetadata(t *testing.T) {
	existing := Account{ID: 9, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Extra: map[string]any{registerLegacyAccountIDKey: "legacy-1", "operator_note": "keep"}}
	writer := &registerWriterStub{}
	persistence := &RegisterPersistence{admin: writer, repo: &registerLookupStub{byLegacy: []Account{existing}}}
	result, err := persistence.Persist(context.Background(), json.RawMessage(`{"id":"legacy-1","provider":"antigravity","email":"new@example.com","credentials":{"accessToken":"rotated"}}`))
	require.NoError(t, err)
	require.False(t, result.Created)
	require.Equal(t, int64(9), writer.updateID)
	require.Equal(t, "keep", writer.updated.Extra["operator_note"])
	require.Equal(t, "new@example.com", writer.updated.Extra[registerEmailKey])
	require.Equal(t, "rotated", writer.updated.Credentials["access_token"])
}

func TestRegisterPersistenceGrokReloginPreservesCredentialsAndRestoresRuntimeState(t *testing.T) {
	existing := Account{
		ID: 12, Platform: PlatformGrok, Type: AccountTypeOAuth, Status: StatusError, Schedulable: false,
		Credentials: map[string]any{"refresh_token": "keep-refresh", "team_id": "keep-team"},
		Extra:       map[string]any{registerLegacyAccountIDKey: "legacy-grok", "operator_note": "keep"},
	}
	writer := &registerWriterStub{account: &Account{ID: 12, Platform: PlatformGrok, Status: StatusActive, Schedulable: true}}
	maintenance := &registerMaintenanceStub{}
	persistence := &RegisterPersistence{admin: writer, repo: &registerLookupStub{byLegacy: []Account{existing}}, maintenance: maintenance}
	result, err := persistence.Persist(context.Background(), json.RawMessage(`{
		"id":"legacy-grok","provider":"grok","email":"owner@example.com","password":"  exact password  ",
		"credentials":{"accessToken":"rotated-access","password":"must-strip","relogin_password":"must-strip-too"}
	}`))
	require.NoError(t, err)
	require.False(t, result.Created)
	require.Equal(t, "rotated-access", writer.updated.Credentials["access_token"])
	require.Equal(t, "keep-refresh", writer.updated.Credentials["refresh_token"])
	require.Equal(t, "keep-team", writer.updated.Credentials["team_id"])
	require.NotContains(t, writer.updated.Credentials, "password")
	require.NotContains(t, writer.updated.Credentials, "relogin_password")
	require.Equal(t, "keep", writer.updated.Extra["operator_note"])
	require.Empty(t, writer.updated.Status, "Grok becomes active only at the restore commit point")
	require.Equal(t, int64(12), maintenance.storedID)
	require.Equal(t, "  exact password  ", maintenance.storedPassword)
	require.Equal(t, int64(12), maintenance.restoredID)
	require.True(t, result.Account.Schedulable)
}

func TestRegisterPersistenceGrokReloginRetryCompletesPartialPersistence(t *testing.T) {
	for _, tc := range []struct {
		name             string
		storeFailures    int
		restoreFailures  int
		wantStoreCalls   int
		wantRestoreCalls int
	}{
		{name: "secret storage", storeFailures: 1, wantStoreCalls: 2, wantRestoreCalls: 1},
		{name: "runtime restore", restoreFailures: 1, wantStoreCalls: 2, wantRestoreCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := Account{
				ID: 12, Platform: PlatformGrok, Type: AccountTypeOAuth, Status: StatusError, Schedulable: false,
				Credentials: map[string]any{"refresh_token": "keep-refresh"},
				Extra:       map[string]any{registerLegacyAccountIDKey: "legacy-grok"},
			}
			writer := &registerWriterStub{account: &Account{ID: 12, Platform: PlatformGrok, Status: StatusActive, Schedulable: true}}
			maintenance := &registerMaintenanceStub{storeFailures: tc.storeFailures, restoreFailures: tc.restoreFailures}
			persistence := &RegisterPersistence{admin: writer, repo: &registerLookupStub{byLegacy: []Account{existing}}, maintenance: maintenance}
			payload := json.RawMessage(`{"id":"legacy-grok","provider":"grok","email":"owner@example.com","password":"secret","credentials":{"accessToken":"rotated"}}`)

			_, err := persistence.Persist(context.Background(), payload)
			require.Error(t, err)
			require.Empty(t, writer.updated.Status)

			result, err := persistence.Persist(context.Background(), payload)
			require.NoError(t, err)
			require.False(t, result.Created)
			require.True(t, result.Account.Schedulable)
			require.Equal(t, 2, writer.updates)
			require.Equal(t, tc.wantStoreCalls, maintenance.storeCalls)
			require.Equal(t, tc.wantRestoreCalls, maintenance.restoreCalls)
		})
	}
}

func TestRegisterPersistenceQoderInjectPreservesOperatorStateAndStoresMetadata(t *testing.T) {
	existing := Account{
		ID: 22, Platform: PlatformQoder, Type: AccountTypeAPIKey, Status: StatusDisabled, Schedulable: false,
		Credentials: map[string]any{"token": "pt-keep", "refresh_token": "keep-refresh"},
		Extra: map[string]any{
			registerLegacyAccountIDKey: "legacy-qoder", "operator_note": "keep",
		},
	}
	writer := &registerWriterStub{}
	persistence := &RegisterPersistence{admin: writer, repo: &registerLookupStub{byLegacy: []Account{existing}}, maintenance: &registerMaintenanceStub{}}
	result, err := persistence.PersistForJob(context.Background(), json.RawMessage(`{
		"id":"legacy-qoder","provider":"qoder","email":"owner@example.com","nickname":"Qoder owner",
		"credentials":{"token":"pt-keep","plan":"Pro Trial","userType":"professional_trial"},
		"tags":["existing","pro-trial"],
		"subscription":{"type":"Pro Trial","title":"Trial"},
		"usage":{"current":0,"limit":300}
	}`), "inject")
	require.NoError(t, err)
	require.False(t, result.Created)
	require.Empty(t, writer.updated.Status, "maintenance callback must preserve a manual pause")
	require.Equal(t, "keep-refresh", writer.updated.Credentials["refresh_token"])
	require.Equal(t, "Pro Trial", writer.updated.Credentials["plan"])
	require.Equal(t, "keep", writer.updated.Extra["operator_note"])
	require.Equal(t, []string{"existing", "pro-trial"}, writer.updated.Extra[registerTagsKey])
	require.Equal(t, "Pro Trial", writer.updated.Extra[registerSubscriptionKey].(map[string]any)["type"])
	require.Equal(t, json.Number("300"), writer.updated.Extra[registerUsageKey].(map[string]any)["limit"])
}

func TestRegisterPersistenceOrdinaryQoderCallbackActivatesAccount(t *testing.T) {
	existing := Account{
		ID: 23, Platform: PlatformQoder, Type: AccountTypeAPIKey, Status: StatusDisabled,
		Credentials: map[string]any{"token": "pt-keep"},
		Extra:       map[string]any{registerLegacyAccountIDKey: "legacy-qoder"},
	}
	writer := &registerWriterStub{}
	persistence := &RegisterPersistence{admin: writer, repo: &registerLookupStub{byLegacy: []Account{existing}}}
	_, err := persistence.PersistForJob(context.Background(), json.RawMessage(`{
		"id":"legacy-qoder","provider":"qoder","email":"owner@example.com",
		"credentials":{"token":"pt-rotated"}
	}`), "register")
	require.NoError(t, err)
	require.Equal(t, StatusActive, writer.updated.Status)
}

func TestRegisterPersistenceBuildsCodeBuddyChinaClaimCandidatesWithoutCredentialMap(t *testing.T) {
	persistence := &RegisterPersistence{repo: &registerLookupStub{byPlatform: []Account{
		{ID: 31, Platform: PlatformCodeBuddyChina, Credentials: map[string]any{"access_token": "access-secret", "refresh_token": "refresh-secret", "other": "do-not-send"}, Extra: map[string]any{registerLegacyAccountIDKey: "legacy-cbcn", registerEmailKey: "owner@example.com"}},
		{ID: 32, Platform: PlatformCodeBuddyChina, Credentials: map[string]any{"accessToken": "access-only"}},
	}}}
	accounts, err := persistence.ListCodeBuddyChinaClaimCandidates(context.Background())
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, "legacy-cbcn", accounts[0].ID)
	require.Equal(t, "owner@example.com", accounts[0].Email)
	require.Equal(t, "access-secret", accounts[0].AccessToken)
	require.Equal(t, "refresh-secret", accounts[0].RefreshToken)
	require.Nil(t, accounts[0].Credentials)
}

func TestRegisterPersistenceStoresGitHubIdentitySecretOutsideCredentials(t *testing.T) {
	writer := &registerWriterStub{}
	maintenance := &registerMaintenanceStub{}
	persistence := &RegisterPersistence{admin: writer, repo: &registerLookupStub{}, maintenance: maintenance}
	result, err := persistence.PersistForJobID(context.Background(), json.RawMessage(`{
		"id":"github-legacy-1","provider":"github","email":"owner@example.com","password":"github-password",
		"githubUsername":"octocat","cookies":[{"name":"session","value":"github-cookie"}],"userAgent":"agent",
		"credentials":{"authMethod":"social","password":"must-strip","cookies":{"session":"must-strip"}}
	}`), "register", "github-job")
	require.NoError(t, err)
	require.True(t, result.Created)
	require.Equal(t, "github", writer.created.Platform)
	require.True(t, writer.created.SkipDefaultGroupBind)
	require.NotContains(t, writer.created.Credentials, "password")
	require.NotContains(t, writer.created.Credentials, "cookies")
	require.NotContains(t, writer.created.Credentials, "rawCookies")
	require.Equal(t, int64(41), maintenance.githubSecretID)
	require.Equal(t, "github-password", maintenance.githubSecret.Password)
	require.Equal(t, "github-cookie", maintenance.githubSecret.Cookies[0]["value"])
}

func TestRegisterPersistenceCompletesReservedCodeBuddyGitHubLink(t *testing.T) {
	writer := &registerWriterStub{}
	maintenance := &registerMaintenanceStub{}
	persistence := &RegisterPersistence{admin: writer, repo: &registerLookupStub{}, maintenance: maintenance}
	result, err := persistence.PersistForJobID(context.Background(), json.RawMessage(`{
		"id":"codebuddy-legacy-1","provider":"codebuddy","email":"owner@example.com","idp":"Github",
		"githubAccountId":"github-legacy-1","githubCookies":[{"name":"session","value":"rotated-cookie"}],
		"githubUserAgent":"rotated-agent","credentials":{"apiKey":"codebuddy-key","password":"must-strip"}
	}`), "register", "github-job")
	require.NoError(t, err)
	require.True(t, result.Created)
	require.Equal(t, PlatformCodeBuddy, writer.created.Platform)
	require.Equal(t, "codebuddy-key", writer.created.Credentials["api_key"])
	require.NotContains(t, writer.created.Credentials, "password")
	require.Equal(t, "github-legacy-1", maintenance.completedGitHubID)
	require.Equal(t, int64(41), maintenance.completedCodeID)
	require.Equal(t, "github-job", maintenance.completedJobID)
	require.Equal(t, "rotated-cookie", maintenance.completedSession.Cookies[0]["value"])
	require.Equal(t, "rotated-agent", maintenance.completedSession.UserAgent)
}

func TestRegisterPersistenceRejectsGrokBeforeCreateWhenSecretStoreUnavailable(t *testing.T) {
	writer := &registerWriterStub{}
	persistence := &RegisterPersistence{admin: writer, repo: &registerLookupStub{}}
	_, err := persistence.Persist(context.Background(), json.RawMessage(`{"id":"legacy-grok","provider":"grok","email":"owner@example.com","password":"secret","credentials":{"accessToken":"access"}}`))
	require.Error(t, err)
	require.Equal(t, "REGISTER_AUTOMATION_SECRET_UNAVAILABLE", RegisterPersistenceErrorCode(err))
	require.Nil(t, writer.created)
}

func TestRegisterPersistenceRejectsDeprecatedLinkedOrMalformedIdentityCallbacks(t *testing.T) {
	persistence := &RegisterPersistence{admin: &registerWriterStub{}, repo: &registerLookupStub{}}
	for _, testCase := range []struct {
		payload string
		code    string
	}{
		{payload: `{"id":"1","provider":"codebuff","credentials":{"token":"x"}}`, code: "REGISTER_PROVIDER_INVALID"},
		{payload: `{"id":"1","provider":"codebuddy","linkedIdentity":{"provider":"github"},"credentials":{"apiKey":"x"}}`, code: "REGISTER_CALLBACK_UNSUPPORTED_IDENTITY"},
		{payload: `{"id":"1","provider":"github","credentials":{"cookies":{"session":"x"}}}`, code: "REGISTER_CALLBACK_INVALID"},
	} {
		_, err := persistence.Persist(context.Background(), json.RawMessage(testCase.payload))
		require.Error(t, err)
		require.Equal(t, testCase.code, RegisterPersistenceErrorCode(err))
	}
}
