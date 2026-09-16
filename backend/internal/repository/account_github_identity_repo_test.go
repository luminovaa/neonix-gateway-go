package repository

import (
	"bytes"
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/luminovaa/neonix-gateway-go/internal/security/credentials"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

func TestListGitHubPickerAccountsReturnsSanitizedEligibilityMetadata(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	now := time.Now().UnixMilli()
	mock.ExpectQuery(regexp.QuoteMeta(githubIdentityRowsSQL)).WillReturnRows(sqlmock.NewRows([]string{
		"id", "public_id", "email", "username", "status", "enabled", "github_created_at", "github_eligible_at",
		"link_status", "linked_codebuddy_account_id", "last_error_code", "has_secret",
	}).AddRow(int64(7), "github-7", "owner@example.com", "octocat", "active", true, now-86400000, now-1000, nil, nil, nil, true))

	repo := &accountRepository{sql: db}
	accounts, err := repo.ListGitHubPickerAccounts(context.Background())
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, "github-7", accounts[0].ID)
	require.Equal(t, "owner@example.com", accounts[0].Email)
	require.True(t, accounts[0].Eligible)
	require.NotNil(t, accounts[0].AgeDays)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReserveGitHubAccountsDecryptsServerEnvelopeAndCreatesReservation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	codec, err := credentials.New(bytes.Repeat([]byte{6}, 32))
	require.NoError(t, err)
	envelope, err := codec.Seal([]byte(`{"password":"server-password","cookies":[{"name":"session","value":"server-cookie"}],"userAgent":"agent"}`))
	require.NoError(t, err)
	now := time.Now().UnixMilli()

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT.*FROM accounts a.*FOR UPDATE OF a`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "public_id", "email", "username", "status", "enabled", "github_created_at", "github_eligible_at", "link_status", "secret_envelope"}).
			AddRow(int64(7), "github-7", "owner@example.com", "octocat", "active", true, now-86400000, now-1000, nil, envelope))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO github_codebuddy_links (github_account_id, job_id, status) VALUES ($1, $2, 'linking')`)).
		WithArgs(int64(7), "job-7").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	repo := &accountRepository{sql: db, credentialCodec: codec}
	accounts, err := repo.ReserveGitHubAccounts(context.Background(), []string{"github-7"}, "job-7")
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, "server-password", accounts[0].Password)
	require.Equal(t, "server-cookie", accounts[0].Cookies[0]["value"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevealLinkedGitHubIdentityPasswordOpensOnlyActiveLink(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	codec, err := credentials.New(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	envelope, err := codec.Seal([]byte(`{"password":"linked-password","cookies":[{"name":"session","value":"secret-cookie"}]}`))
	require.NoError(t, err)
	mock.ExpectQuery(`(?s)SELECT s.secret_envelope.*FROM github_codebuddy_links`).
		WithArgs(int64(91), "codebuddy").
		WillReturnRows(sqlmock.NewRows([]string{"secret_envelope"}).AddRow(envelope))

	repo := &accountRepository{sql: db, credentialCodec: codec}
	password, err := repo.RevealLinkedGitHubIdentityPassword(context.Background(), 91)
	require.NoError(t, err)
	require.Equal(t, "linked-password", password)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevealLinkedGitHubIdentityPasswordReturnsStableNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	codec, err := credentials.New(bytes.Repeat([]byte{8}, 32))
	require.NoError(t, err)
	mock.ExpectQuery(`(?s)SELECT s.secret_envelope.*FROM github_codebuddy_links`).
		WithArgs(int64(92), "codebuddy").
		WillReturnError(sql.ErrNoRows)

	repo := &accountRepository{sql: db, credentialCodec: codec}
	_, err = repo.RevealLinkedGitHubIdentityPassword(context.Background(), 92)
	require.ErrorIs(t, err, service.ErrLinkedGitHubIdentityNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}
