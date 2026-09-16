package service

import (
	"encoding/json"
	"strconv"
	"strings"

	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
)

const (
	registerGitHubCookieLimit     = 512
	registerGitHubCookieNameMax   = 256
	registerGitHubCookieValueMax  = 16384
	registerGitHubUserAgentMax    = 2048
	registerGitHubProxyMax        = 2048
	registerLatestReasonableEpoch = int64(32503680000000) // 3000-01-01 UTC
)

func decodeRegisterGitHubSecret(value, credentials map[string]any) (*RegisterGitHubIdentitySecret, error) {
	password := rawRegisterString(value, "password")
	if strings.TrimSpace(password) == "" {
		password = firstRawStringValue(credentials, "password")
	}
	if strings.TrimSpace(password) == "" || len(password) > 4096 {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "GitHub identity callback is missing a valid password")
	}

	rawCookies, ok := value["cookies"]
	if !ok || rawCookies == nil {
		rawCookies, ok = credentials["cookies"]
	}
	if (!ok || rawCookies == nil) && credentials["rawCookies"] != nil {
		rawCookies, ok = credentials["rawCookies"]
	}
	cookies, err := normalizeRegisterGitHubCookies(rawCookies)
	if err != nil || !ok {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "GitHub identity callback is missing valid cookies")
	}

	userAgent := firstRawStringValue(value, "userAgent", "user_agent")
	if userAgent == "" {
		userAgent = firstRawStringValue(credentials, "userAgent", "user_agent")
	}
	proxy := firstRawStringValue(value, "proxy")
	if proxy == "" {
		proxy = firstRawStringValue(credentials, "proxy")
	}
	if len(userAgent) > registerGitHubUserAgentMax || len(proxy) > registerGitHubProxyMax {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "GitHub identity callback metadata is invalid")
	}
	return &RegisterGitHubIdentitySecret{Password: password, Cookies: cookies, UserAgent: userAgent, Proxy: proxy}, nil
}

func decodeRegisterGitHubSession(value map[string]any) (*RegisterGitHubSessionUpdate, error) {
	raw, exists := value["githubCookies"]
	userAgent := firstRawStringValue(value, "githubUserAgent")
	if !exists || raw == nil {
		if len(userAgent) > registerGitHubUserAgentMax {
			return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "GitHub session metadata is invalid")
		}
		if userAgent == "" {
			return nil, nil
		}
		return &RegisterGitHubSessionUpdate{UserAgent: userAgent}, nil
	}
	cookies, err := normalizeRegisterGitHubCookies(raw)
	if err != nil || len(userAgent) > registerGitHubUserAgentMax {
		return nil, infraerrors.BadRequest("REGISTER_CALLBACK_INVALID", "GitHub session metadata is invalid")
	}
	return &RegisterGitHubSessionUpdate{Cookies: cookies, UserAgent: userAgent}, nil
}

func normalizeRegisterGitHubCookies(value any) ([]map[string]any, error) {
	if text, ok := value.(string); ok {
		var decoded any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, err
		}
		value = decoded
	}

	items := make([]any, 0)
	switch typed := value.(type) {
	case []any:
		items = typed
	case []map[string]any:
		items = make([]any, len(typed))
		for i := range typed {
			items[i] = typed[i]
		}
	case map[string]any:
		for name, cookieValue := range typed {
			items = append(items, map[string]any{"name": name, "value": cookieValue, "domain": ".github.com", "path": "/"})
		}
	default:
		return nil, strconv.ErrSyntax
	}
	if len(items) == 0 || len(items) > registerGitHubCookieLimit {
		return nil, strconv.ErrRange
	}

	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		cookie, ok := item.(map[string]any)
		if !ok {
			return nil, strconv.ErrSyntax
		}
		name, nameOK := cookie["name"].(string)
		cookieValue, valueOK := cookie["value"].(string)
		if !nameOK || !valueOK || strings.TrimSpace(name) == "" || len(name) > registerGitHubCookieNameMax || len(cookieValue) > registerGitHubCookieValueMax {
			return nil, strconv.ErrSyntax
		}
		copy := cloneRegisterMap(cookie)
		if !validRegisterCredentialValue(copy, 0) {
			return nil, strconv.ErrSyntax
		}
		result = append(result, copy)
	}
	return result, nil
}

func registerInt64(value map[string]any, key string) int64 {
	raw, exists := value[key]
	if !exists || raw == nil {
		return 0
	}
	var parsed int64
	switch typed := raw.(type) {
	case json.Number:
		parsed, _ = typed.Int64()
	case int64:
		parsed = typed
	case int:
		parsed = int64(typed)
	case float64:
		parsed = int64(typed)
	case string:
		parsed, _ = strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
	}
	if parsed <= 0 || parsed > registerLatestReasonableEpoch {
		return 0
	}
	return parsed
}
