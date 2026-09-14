package service

import (
	"github.com/luminovaa/neonix-gateway-go/internal/config"
	"github.com/luminovaa/neonix-gateway-go/internal/util/responseheaders"
)

func compileResponseHeaderFilter(cfg *config.Config) *responseheaders.CompiledHeaderFilter {
	if cfg == nil {
		return nil
	}
	return responseheaders.CompileHeaderFilter(cfg.Security.ResponseHeaders)
}
