package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
	"github.com/stretchr/testify/require"
)

func TestAccountCredentialEnvelopeSealAndOpen(t *testing.T) {
	codec, err := credentials.New([]byte("01234567890123456789012345678901"))
	require.NoError(t, err)
	payload := map[string]any{"access_token": "secret", "project_id": "project"}
	envelope, err := sealAccountCredentials(codec, payload)
	require.NoError(t, err)
	require.NotContains(t, envelope, "secret")
	decoded, err := openAccountCredentialEnvelope(codec, envelope)
	require.NoError(t, err)
	require.Equal(t, payload, decoded)
}

func TestCredentialEnvelopeFromEnvironmentFailsClosedWhenConfiguredKeyIsInvalid(t *testing.T) {
	t.Setenv("NEONIX_CREDENTIAL_KEY", "not-a-key")
	codec, err := credentialEnvelopeFromEnvironment()
	require.Nil(t, codec)
	require.Error(t, err)
}

func TestLoadAccountCredentialEnvelopesReadsOnlyRequestedIDs(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(accountCredentialEnvelopeSelectSQL)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "envelope"}).
			AddRow(int64(7), "neonix-credential-v1.payload").
			AddRow(int64(8), ""))
	got, err := loadAccountCredentialEnvelopes(context.Background(), db, []int64{7, 7, 8})
	require.NoError(t, err)
	require.Equal(t, map[int64]string{7: "neonix-credential-v1.payload"}, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAccountCredentialEnvelopeRejectsMalformedPayload(t *testing.T) {
	codec, err := credentials.New([]byte("01234567890123456789012345678901"))
	require.NoError(t, err)
	_, err = openAccountCredentialEnvelope(codec, "neonix-credential-v1.bad")
	require.Error(t, err)
	_, err = openAccountCredentialEnvelope(codec, "")
	require.Error(t, err)

	// An envelope containing a JSON scalar is not a credentials map.
	envelope, err := codec.Seal([]byte(`"token"`))
	require.NoError(t, err)
	_, err = openAccountCredentialEnvelope(codec, envelope)
	require.Error(t, err)
	_, err = loadAccountCredentialEnvelopes(context.Background(), nil, []int64{1})
	require.NoError(t, err)
}

func TestSyncAccountCredentialEnvelopesSealsCurrentJSONBValues(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	codec, err := credentials.New([]byte("01234567890123456789012345678901"))
	require.NoError(t, err)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, credentials FROM accounts WHERE id = ANY($1) AND deleted_at IS NULL")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "credentials"}).AddRow(int64(4), []byte(`{"access_token":"secret"}`)))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO account_credential_envelopes (account_id, envelope, key_version)")).
		WithArgs(int64(4), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, syncAccountCredentialEnvelopes(context.Background(), db, codec, []int64{4}))
	require.NoError(t, mock.ExpectationsWereMet())
}
