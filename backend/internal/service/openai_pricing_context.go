package service

import (
	"context"
	"time"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/timezone"
)

// openAIPricingAtCtxKey keeps a stable request timestamp for peak-rate billing.
// It is intentionally independent from account admission and scheduling.
type openAIPricingAtCtxKey struct{}

func (s *OpenAIGatewayService) WithOpenAIRequestPricingContext(ctx context.Context, _ *int64) (context.Context, time.Time) {
	pricingAt := timezone.Now()
	return context.WithValue(ctx, openAIPricingAtCtxKey{}, pricingAt), pricingAt
}

func (s *OpenAIGatewayService) WithOpenAITurnPricingContext(ctx context.Context, _ *int64) (context.Context, time.Time) {
	pricingAt := timezone.Now()
	return context.WithValue(ctx, openAIPricingAtCtxKey{}, pricingAt), pricingAt
}

func openAIPricingAtFromContext(ctx context.Context) (time.Time, bool) {
	pricingAt, ok := ctx.Value(openAIPricingAtCtxKey{}).(time.Time)
	return pricingAt, ok && !pricingAt.IsZero()
}

func OpenAIPricingAtFromContext(ctx context.Context) time.Time {
	pricingAt, _ := openAIPricingAtFromContext(ctx)
	return pricingAt
}

// BindStickySessionForRequest records a selected account while preserving a
// guardian parent's existing binding when the current request uses the same
// session hash.
func (s *OpenAIGatewayService) BindStickySessionForRequest(ctx context.Context, groupID *int64, sessionHash string, accountID int64) error {
	if preserveOpenAIGuardianParentBinding(ctx, sessionHash) {
		return nil
	}
	return s.BindStickySession(ctx, groupID, sessionHash, accountID)
}

func (s *OpenAIGatewayService) bindOpenAIStickySessionDuringSelection(ctx context.Context, groupID *int64, sessionHash string, accountID int64) error {
	return s.BindStickySessionForRequest(ctx, groupID, sessionHash, accountID)
}
