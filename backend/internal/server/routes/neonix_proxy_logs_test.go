package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNeonixProxyLogsUnifiesSuccessAndFailureShape(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	columns := []string{"id", "timestamp", "path", "model", "account_id", "account_email", "account_name", "account_provider", "input", "output", "cache_read", "cache_write", "credits", "response_time", "ttft", "status", "success", "error"}
	mock.ExpectQuery("WITH view_state").WithArgs(int64(1710000000000), 50).WillReturnRows(sqlmock.NewRows(columns).
		AddRow(3, int64(1710000001000), "/v1/responses", "gpt-test", "8", "", "Account", "openai", 10, 2, 4, 1, 0.25, 500, 120, 200, true, "").
		AddRow(-4, int64(1710000000000), "/v1/messages", "claude-test", "9", "", "Claude", "anthropic", 0, 0, 0, 0, 0, 300, 0, 429, false, "Rate limited"))
	router := gin.New()
	router.GET("/logs", (&neonixProxyLogs{db: db}).list)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/logs?limit=50&since=1710000000000", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"success":true`)
	require.Contains(t, recorder.Body.String(), `"cacheReadTokens":4`)
	require.Contains(t, recorder.Body.String(), `"error":"Rate limited"`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNeonixProxyLogsBoundsLimitAndNormalizesSeconds(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/?limit=999999&since=1710000000", nil)
	limit, since := parseProxyLogQuery(ctx)
	require.Equal(t, 10000, limit)
	require.Equal(t, int64(1710000000000), since)
}

func TestNeonixProxyLogsClearAdvancesViewBoundary(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectExec("INSERT INTO neonix_proxy_log_view_state").WillReturnResult(sqlmock.NewResult(1, 1))
	router := gin.New()
	router.DELETE("/logs", (&neonixProxyLogs{db: db}).clear)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/logs", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"ok":true}`, recorder.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}
