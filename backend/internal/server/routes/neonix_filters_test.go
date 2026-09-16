package routes

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/filterrule"
	"github.com/stretchr/testify/require"
)

func filterTestDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func filterContext(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

func TestNeonixFiltersListReturnsCountsAndOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, mock := filterTestDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	mock.ExpectQuery("SELECT id,rule_id,pattern,replacement,is_active,is_regex,sort_order,created_at,updated_at FROM filter_rules ORDER BY sort_order,id").WillReturnRows(
		sqlmock.NewRows([]string{"id", "rule_id", "pattern", "replacement", "is_active", "is_regex", "sort_order", "created_at", "updated_at"}).
			AddRow(2, "first", "one", "", true, false, 0, now, nil).
			AddRow(1, "second", "two", "", false, false, 1, now, nil),
	)
	c, w := filterContext(http.MethodGet, "/api/filters", "")
	(&neonixFilters{db: db}).list(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"count":2`)
	require.Contains(t, w.Body.String(), `"activeCount":1`)
	require.Less(t, strings.Index(w.Body.String(), `"first"`), strings.Index(w.Body.String(), `"second"`))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNeonixFiltersRejectsInvalidInputs(t *testing.T) {
	db, _ := filterTestDB(t)
	s := &neonixFilters{db: db, runtime: filterrule.New(db)}
	for _, test := range []struct{ body, code string }{{`{}`, "FILTER_PATTERN_REQUIRED"}, {`{"pattern":"[","isRegex":true}`, "FILTER_PATTERN_INVALID"}} {
		c, w := filterContext(http.MethodPost, "/api/filters", test.body)
		s.create(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Contains(t, w.Body.String(), test.code)
	}
}

func TestNeonixFiltersDeleteMissing(t *testing.T) {
	db, mock := filterTestDB(t)
	mock.ExpectExec(`DELETE FROM filter_rules WHERE id=\$1`).WithArgs(int64(42)).WillReturnResult(sqlmock.NewResult(0, 0))
	c, w := filterContext(http.MethodDelete, "/api/filters/42", "")
	c.Params = gin.Params{{Key: "id", Value: "42"}}
	(&neonixFilters{db: db, runtime: filterrule.New(db)}).delete(c)
	require.Equal(t, http.StatusNotFound, w.Code)
	require.Contains(t, w.Body.String(), "FILTER_RULE_NOT_FOUND")
	require.NoError(t, mock.ExpectationsWereMet())
}
