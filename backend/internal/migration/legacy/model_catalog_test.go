package legacy

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestImportModelCatalogPreservesAdminAndSoftDeleteState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO neonix_model_catalog").
		WithArgs("oc/free", "Free", "desc", "oc", "oc", false, "MAINTENANCE", sqlmock.AnyArg(), true, true, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	deleted := int64(1710000002000)
	report, err := ImportModelCatalogIntoPostgres(context.Background(), db, []ModelCatalogRow{{
		ModelID: "oc/free", ModelName: "Free", Description: "desc", Provider: "oc", Source: "oc",
		RawData: []byte(`{"status":"MAINTENANCE"}`), UpdatedByAdmin: true, IsDeleted: true,
		CreatedAt: 1710000000000, UpdatedAt: 1710000001000, DeletedAt: &deleted,
	}}, func() time.Time { return time.UnixMilli(1710000000000).UTC() })
	require.NoError(t, err)
	require.Equal(t, ModelCatalogReport{Total: 1, Applied: 1}, report)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestImportModelCatalogRejectsDuplicateIDsAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO neonix_model_catalog").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()
	_, err = ImportModelCatalogIntoPostgres(context.Background(), db, []ModelCatalogRow{{ModelID: "x"}, {ModelID: "x"}}, time.Now)
	require.ErrorIs(t, err, ErrImportDB)
	require.NoError(t, mock.ExpectationsWereMet())
}
