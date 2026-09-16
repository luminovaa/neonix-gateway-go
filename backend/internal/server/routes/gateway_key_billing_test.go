package routes

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayRoutesKeyBillingInfoPathIsNotRegistered(t *testing.T) {
	router := newGatewayRoutesTestRouter()

	for _, route := range router.Routes() {
		require.False(t, route.Method == http.MethodGet && route.Path == "/v1/sub2api/billing")
	}
}
