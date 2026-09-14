package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/luminovaa/neonix-gateway-go/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNeonixAuthUserMapperKeepsSafeCamelCaseFields(t *testing.T) {
	created := time.UnixMilli(1710000000000).UTC()
	updated := created.Add(time.Hour)
	lastLogin := updated.Add(time.Hour)
	user := neonixAuthUserFromService(&service.User{
		ID:           9,
		Username:     "operator",
		Email:        "operator@example.com",
		Role:         service.RoleAdmin,
		Status:       service.StatusActive,
		CreatedAt:    created,
		UpdatedAt:    updated,
		LastLoginAt:  &lastLogin,
		PasswordHash: "must-not-leak",
	})

	encoded, err := json.Marshal(user)
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"9","username":"operator","email":"operator@example.com","displayName":"operator","role":"admin","status":"active","createdAt":1710000000000,"updatedAt":1710003600000,"lastLoginAt":1710007200000}`, string(encoded))
	require.NotContains(t, string(encoded), "password")
}
