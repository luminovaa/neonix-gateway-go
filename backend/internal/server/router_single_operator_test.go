package server

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionRouterExposesOnlySingleOperatorControlPlane(t *testing.T) {
	source, err := os.ReadFile("router.go")
	require.NoError(t, err)

	routerSource := string(source)
	require.NotContains(t, routerSource, "routes.RegisterAuthRoutes(", "the inherited multi-user auth surface must stay off the production router")
	require.NotContains(t, routerSource, "handler.RegisterPageRoutes(", "the inherited page system must stay off the production router")
	require.Contains(t, routerSource, "routes.RegisterNeonixCompatibilityRoutes(")
	require.Contains(t, routerSource, "routes.RegisterGatewayRoutes(")
	require.False(t, strings.Contains(routerSource, "routes.RegisterUserRoutes("), "member routes must stay off the production router")
	require.False(t, strings.Contains(routerSource, "routes.RegisterAdminRoutes("), "the inherited Sub2API admin surface must stay off the production router")
	require.False(t, strings.Contains(routerSource, "subscriptionService *service.SubscriptionService"), "the production router must not start subscription enforcement")
	require.False(t, strings.Contains(routerSource, "jwtAuth middleware2.JWTAuthMiddleware"), "the production router must use only the operator and gateway auth boundaries")
}
