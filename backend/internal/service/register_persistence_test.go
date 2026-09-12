package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

type registerWriterStub struct {
	created  *CreateAccountInput
	updated  *UpdateAccountInput
	updateID int64
}

func (w *registerWriterStub) CreateAccount(_ context.Context, input *CreateAccountInput) (*Account, error) {
	w.created = input
	return &Account{ID: 41, Name: input.Name, Platform: input.Platform, Type: input.Type, Credentials: input.Credentials, Extra: input.Extra, Status: StatusActive}, nil
}

func (w *registerWriterStub) UpdateAccount(_ context.Context, id int64, input *UpdateAccountInput) (*Account, error) {
	w.updateID, w.updated = id, input
	return &Account{ID: id, Platform: PlatformAntigravity, Credentials: input.Credentials, Extra: input.Extra, Status: StatusActive}, nil
}

type registerLookupStub struct {
	byLegacy   []Account
	byPlatform []Account
}

func (r *registerLookupStub) FindByExtraField(_ context.Context, _ string, _ any) ([]Account, error) {
	return r.byLegacy, nil
}
func (r *registerLookupStub) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
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

func TestRegisterPersistenceRejectsDeprecatedOrLinkedIdentityCallbacks(t *testing.T) {
	persistence := &RegisterPersistence{admin: &registerWriterStub{}, repo: &registerLookupStub{}}
	for _, payload := range []string{
		`{"id":"1","provider":"codebuff","credentials":{"token":"x"}}`,
		`{"id":"1","provider":"codebuddy","linkedIdentity":{"provider":"github"},"credentials":{"apiKey":"x"}}`,
		`{"id":"1","provider":"github","credentials":{"cookies":{"session":"x"}}}`,
	} {
		_, err := persistence.Persist(context.Background(), json.RawMessage(payload))
		require.Error(t, err)
		require.Contains(t, []string{"REGISTER_PROVIDER_INVALID", "REGISTER_CALLBACK_UNSUPPORTED_IDENTITY"}, RegisterPersistenceErrorCode(err))
	}
}
