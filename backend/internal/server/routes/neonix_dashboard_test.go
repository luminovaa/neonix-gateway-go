package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDashboardPeriodAndSeriesAreDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 12, 9, 30, 0, 0, time.UTC)
	period, start, slots := dashboardPeriod("7d", now)
	require.Equal(t, "7d", period)
	require.Equal(t, 7, slots)
	require.Equal(t, time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), start)
	rows := []dashboardSeries{{Date: "2026-09-07T00:00:00Z", dashboardAggregate: dashboardAggregate{SuccessRequests: 2, TotalRequests: 2}}}
	filled := fillDashboardSeries(period, start, slots, rows)
	require.Len(t, filled, 7)
	require.Equal(t, "09-06", filled[0].Label)
	require.Equal(t, int64(2), filled[1].TotalRequests)
}

func TestNeonixLeaderboardAggregatesModelsOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("WITH combined AS .*GROUP BY model").WithArgs(sqlmock.AnyArg(), 5).WillReturnRows(sqlmock.NewRows([]string{"model", "success", "failed", "input", "output", "credits", "duration"}).AddRow("gemini-test", 8, 2, 100, 50, 1.25, 10000))
	router := gin.New()
	router.GET("/leaderboard", (&neonixDashboard{db: db}).leaderboard)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/leaderboard?period=7d&limit=5", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"period":"7d"`)
	require.Contains(t, recorder.Body.String(), `"model":"gemini-test"`)
	require.Contains(t, recorder.Body.String(), `"successRate":80`)
	require.NotContains(t, recorder.Body.String(), `"users"`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLeaderboardBoundsLimitAndRejectsDatabaseFailureSafely(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("WITH combined AS .*GROUP BY model").WithArgs(nil, 100).WillReturnError(sqlmock.ErrCancelled)
	router := gin.New()
	router.GET("/leaderboard", (&neonixDashboard{db: db}).leaderboard)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/leaderboard?period=invalid&limit=5000", nil))
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Contains(t, recorder.Body.String(), "LEADERBOARD_LOAD_FAILED")
	require.NotContains(t, recorder.Body.String(), "canceling query")
}

func TestPublicStatsReturnsOnlyAggregateUsageAndCachesBriefly(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT COUNT.*FROM usage_logs").WillReturnRows(sqlmock.NewRows([]string{"requests", "tokens"}).AddRow(12, 3456))
	mock.ExpectQuery("SELECT COALESCE.*GROUP BY 1").WithArgs(5).WillReturnRows(
		sqlmock.NewRows([]string{"model", "requests", "tokens"}).
			AddRow("codex/gpt-5", 8, 3000).
			AddRow("antigravity/gemini", 4, 456),
	)
	router := gin.New()
	router.GET("/api/public/stats", (&neonixDashboard{db: db}).publicStats)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/public/stats", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "public, max-age=60", recorder.Header().Get("Cache-Control"))
	require.Contains(t, recorder.Body.String(), `"totalTokens":3456`)
	require.Contains(t, recorder.Body.String(), `"totalRequests":12`)
	require.Contains(t, recorder.Body.String(), `"model":"codex/gpt-5"`)
	require.NotContains(t, recorder.Body.String(), "account")
	require.NoError(t, mock.ExpectationsWereMet())
}
