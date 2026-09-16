package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestFilterRegisteredAccountsUsesLegacyProviderAndEmailMetadata(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT lower\(extra->>'neonix_legacy_email'\)`).
		WithArgs("kiro", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("known@example.com"))

	router := gin.New()
	router.POST("/filter", filterRegisteredAccounts(db))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/filter", strings.NewReader(`{"provider":"kiro","emails":["KNOWN@example.com","new@example.com","known@example.com"]}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"registered":["known@example.com"],"unregistered":["new@example.com"]}`, recorder.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFilterRegisteredAccountsRejectsInvalidPayloadBeforeQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	router := gin.New()
	router.POST("/filter", filterRegisteredAccounts(db))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/filter", strings.NewReader(`{"provider":"","emails":["user@example.com"]}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "ACCOUNT_PROVIDER_REQUIRED")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNormalizedFilterEmailsIsBoundedAndStable(t *testing.T) {
	require.Equal(t, []string{"one@example.com", "two@example.com"}, normalizedFilterEmails([]string{
		" One@Example.com ", "invalid", "one@example.com", "two@example.com",
	}))
}
