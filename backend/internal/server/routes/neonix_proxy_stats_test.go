package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNeonixProxyStatsSubtractsPersistentBaseline(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT.+usage_logs").WillReturnRows(sqlmock.NewRows([]string{"success", "failed", "input", "output", "credits"}).AddRow(12, 3, 150, 40, 2.5))
	mock.ExpectQuery("SELECT success_requests").WillReturnRows(sqlmock.NewRows([]string{"success", "failed", "input", "output", "credits"}).AddRow(2, 1, 50, 10, 0.5))
	router := gin.New()
	router.GET("/stats", (&neonixProxyStats{db: db}).get)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/stats", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"totalRequests":12,"successRequests":10,"failedRequests":2,"inputTokens":100,"outputTokens":30,"totalCredits":2,"activeRequests":0}`, recorder.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNeonixProxyStatsNeverReturnsNegativeAfterRetention(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT.+usage_logs").WillReturnRows(sqlmock.NewRows([]string{"success", "failed", "input", "output", "credits"}).AddRow(1, 0, 5, 2, 0.1))
	mock.ExpectQuery("SELECT success_requests").WillReturnRows(sqlmock.NewRows([]string{"success", "failed", "input", "output", "credits"}).AddRow(10, 10, 50, 20, 5))
	router := gin.New()
	router.GET("/stats", (&neonixProxyStats{db: db}).get)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/stats", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"totalRequests":0`)
	require.Contains(t, recorder.Body.String(), `"totalCredits":0`)
}

func TestNeonixProxyStatsResetStoresCurrentTotalsWithoutDeletingUsage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT.+usage_logs").WillReturnRows(sqlmock.NewRows([]string{"success", "failed", "input", "output", "credits"}).AddRow(12, 3, 150, 40, 2.5))
	mock.ExpectExec("INSERT INTO neonix_proxy_stats_baseline").WithArgs(int64(12), int64(3), int64(150), int64(40), 2.5).WillReturnResult(sqlmock.NewResult(1, 1))
	router := gin.New()
	router.POST("/stats/reset", (&neonixProxyStats{db: db}).reset)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/stats/reset", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"ok":true}`, recorder.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}
