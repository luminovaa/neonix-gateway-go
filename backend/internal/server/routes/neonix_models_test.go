package routes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/provider/opencode"
	"github.com/stretchr/testify/require"
)

type modelCatalogRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn modelCatalogRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestNeonixModelCatalogListReturnsDirectLegacyShape(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT model_id, model_name").WillReturnRows(sqlmock.NewRows([]string{
		"model_id", "model_name", "description", "provider", "source", "status", "is_deleted", "updated_by_admin", "raw_data", "updated_at",
	}).AddRow("oc/free", "Free", "", "oc", "oc", "AVAILABLE", false, false, []byte(`{"tokenLimits":{"maxInputTokens":64000,"maxOutputTokens":8192},"supportsToolCalling":true}`), time.Unix(1710000000, 0).UTC()))

	router := gin.New()
	router.GET("/api/proxy/models", (&neonixModelCatalog{db: db}).list)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/proxy/models", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `[{"id":"oc/free","name":"Free","provider":"oc","source":"oc","status":"AVAILABLE","isDeleted":false,"updatedByAdmin":false,"updatedAt":1710000000000,"tokenLimits":{"maxInputTokens":64000,"maxOutputTokens":8192},"maxInputTokens":64000,"maxOutputTokens":8192,"supportsToolCalling":true}]`, recorder.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNeonixModelCatalogUpdateDoesNotCreateMissingModel(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT EXISTS").WithArgs("missing").WillReturnRows(sqlmock.NewRows([]string{"exists", "name", "description", "provider", "source", "status", "raw_data"}).AddRow(false, "", "", "", "", "", []byte(`{}`)))

	router := gin.New()
	router.PATCH("/api/proxy/models/*id", (&neonixModelCatalog{db: db}).update)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/proxy/models/missing", strings.NewReader(`{"name":"Missing"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Body.String(), "MODEL_NOT_FOUND")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNeonixModelCatalogUpdateMergesExistingRawMetadata(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT EXISTS").WithArgs("oc/free").WillReturnRows(sqlmock.NewRows([]string{"exists", "name", "description", "provider", "source", "status", "raw_data"}).AddRow(true, "Free", "old description", "oc", "oc", "AVAILABLE", []byte(`{"actualModelId":"free-upstream","sortOrder":3}`)))
	mock.ExpectExec("INSERT INTO neonix_model_catalog").WithArgs("oc/free", "Free renamed", "old description", "oc", "oc", "AVAILABLE", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT model_id, model_name").WithArgs("oc/free").WillReturnRows(sqlmock.NewRows([]string{"model_id", "model_name", "description", "provider", "source", "status", "is_deleted", "updated_by_admin", "raw_data", "updated_at"}).AddRow("oc/free", "Free renamed", "old description", "oc", "oc", "AVAILABLE", false, true, []byte(`{"actualModelId":"free-upstream","sortOrder":3,"modelProvider":"oc","status":"AVAILABLE"}`), time.Unix(1710000000, 0).UTC()))

	router := gin.New()
	router.PATCH("/api/proxy/models/*id", (&neonixModelCatalog{db: db}).update)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/proxy/models/oc%2Ffree", strings.NewReader(`{"name":"Free renamed","provider":"oc"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"actualModelId":"free-upstream"`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNormalizeModelInputRejectsInvalidStatusAndKeepsTechnicalMetadata(t *testing.T) {
	input := neonixModelInput{ID: " oc/free ", Provider: " oc ", Status: "available", MaxInputTokens: ptrInt64(64_000)}
	require.NoError(t, normalizeModelInput(&input))
	require.Equal(t, "oc/free", input.ID)
	require.Equal(t, "AVAILABLE", input.Status)
	require.Equal(t, "oc", input.RawData["modelProvider"])

	invalid := neonixModelInput{ID: "x", Status: "retired"}
	require.ErrorIs(t, normalizeModelInput(&invalid), errModelStatusInvalid)
}

func ptrInt64(value int64) *int64 { return &value }

func TestPersistOpenCodeModelsSoftDeletesRetiredAndPreservesAdminRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO neonix_model_catalog").
		WithArgs("oc/current", "Current", "free", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT model_id, updated_by_admin").WillReturnRows(sqlmock.NewRows([]string{"model_id", "updated_by_admin"}).
		AddRow("oc/current", false).
		AddRow("oc/retired", false).
		AddRow("oc/custom", true))
	mock.ExpectExec("UPDATE neonix_model_catalog SET is_deleted=TRUE").WithArgs("oc/retired").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := (&neonixModelCatalog{db: db}).persistOpenCodeModels(context.Background(), []opencode.FreeModel{{ID: "current", Name: "Current", Description: "free", OwnedBy: "opencode"}}, false)
	require.NoError(t, err)
	require.Equal(t, openCodeSyncResult{Provider: "oc", Upserted: 1, Removed: 1, Preserved: 1}, result)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPersistOpenCodeModelsKeepsAdminOwnedConflictUntouched(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO neonix_model_catalog").WithArgs("oc/custom", "Remote", "remote", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT model_id, updated_by_admin").WillReturnRows(sqlmock.NewRows([]string{"model_id", "updated_by_admin"}).AddRow("oc/custom", true))
	mock.ExpectCommit()

	result, err := (&neonixModelCatalog{db: db}).persistOpenCodeModels(context.Background(), []opencode.FreeModel{{ID: "custom", Name: "Remote", Description: "remote"}}, false)
	require.NoError(t, err)
	require.Equal(t, 1, result.Upserted)
	require.Zero(t, result.Removed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSyncOpenCodeFailurePreservesExistingCatalog(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	client := &http.Client{Transport: modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})}
	router := gin.New()
	router.POST("/sync", (&neonixModelCatalog{db: db, httpClient: client}).syncOpenCode)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sync", nil))
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.Contains(t, recorder.Body.String(), "MODEL_SYNC_UPSTREAM_UNAVAILABLE")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSyncOpenCodeBootstrapsFallbackOnlyWhenCatalogEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectBegin()
	for range opencode.FallbackModels() {
		mock.ExpectExec("INSERT INTO neonix_model_catalog").WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectQuery("SELECT model_id, updated_by_admin").WillReturnRows(sqlmock.NewRows([]string{"model_id", "updated_by_admin"}))
	mock.ExpectCommit()
	client := &http.Client{Transport: modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})}
	router := gin.New()
	router.POST("/sync", (&neonixModelCatalog{db: db, httpClient: client}).syncOpenCode)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sync", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"bootstrap":true`)
	require.Contains(t, recorder.Body.String(), `"upserted":4`)
	require.NoError(t, mock.ExpectationsWereMet())
}
