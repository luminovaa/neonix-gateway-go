package service

import (
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddy"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/codebuddychina"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/qoder"
)

const KiroDefaultModelID = "CLAUDE_SONNET_4_20250514_V1_0"

// NativeProviderModelIDs supplies the safe local fallback for providers whose
// catalog is discovered from account-scoped endpoints or curated locally.
func NativeProviderModelIDs(platform string) []string {
	switch platform {
	case PlatformKiro:
		return []string{KiroDefaultModelID}
	case PlatformQoder:
		ids := make([]string, 0, len(qoder.Models))
		for _, model := range qoder.Models {
			ids = append(ids, model.ID)
		}
		return ids
	case PlatformCodeBuddy:
		return codebuddy.ModelIDs()
	case PlatformWorkBuddy:
		// WorkBuddy has no curated fallback: its catalogue and capabilities are
		// account-scoped and must come from the live Global endpoint.
		return nil
	case PlatformCodeBuddyChina:
		return codebuddychina.ModelIDs()
	default:
		return nil
	}
}
