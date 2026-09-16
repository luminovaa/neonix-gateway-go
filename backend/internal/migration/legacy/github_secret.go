package legacy

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type legacyGitHubSecret struct {
	Password  string           `json:"password"`
	Cookies   []map[string]any `json:"cookies"`
	UserAgent string           `json:"userAgent,omitempty"`
	Proxy     string           `json:"proxy,omitempty"`
}

// DecodeLegacyBYOKKey implements the strict key formats accepted by the Node
// backend. An empty value is allowed for exports without an encrypted secret.
func DecodeLegacyBYOKKey(raw string) ([]byte, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	if len(value) == 64 {
		decoded, err := hex.DecodeString(value)
		if err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) < 32 {
		return nil, errors.New("BYOK_ENCRYPTION_KEY must be 64 hex characters or base64 encoding at least 32 bytes")
	}
	return append([]byte(nil), decoded[:32]...), nil
}

func extractLegacyGitHubSecret(account Account, values map[string]any, legacyKey []byte) (legacyGitHubSecret, bool, error) {
	if encoded := firstLegacyString(values, "githubSecret", "github_secret"); encoded != "" {
		raw := []byte(encoded)
		if !strings.HasPrefix(strings.TrimSpace(encoded), "{") {
			var err error
			raw, err = openNodeGitHubSecret(encoded, legacyKey)
			if err != nil {
				return legacyGitHubSecret{}, false, ErrInvalidGitHubSecret
			}
		}
		var secret legacyGitHubSecret
		if json.Unmarshal(raw, &secret) != nil || !validLegacyGitHubSecret(secret) {
			return legacyGitHubSecret{}, false, ErrInvalidGitHubSecret
		}
		return secret, true, nil
	}

	password := account.Password
	if strings.TrimSpace(password) == "" {
		password = firstLegacyString(values, "password")
	}
	cookieValue, cookiePresent := firstLegacyCookieValue(account, values)
	if strings.TrimSpace(password) == "" && !cookiePresent {
		return legacyGitHubSecret{}, false, nil
	}
	cookies, err := normalizeLegacyGitHubCookies(cookieValue)
	if err != nil {
		return legacyGitHubSecret{}, false, ErrInvalidGitHubSecret
	}
	secret := legacyGitHubSecret{
		Password: password, Cookies: cookies,
		UserAgent: firstNonEmpty(account.UserAgent, firstLegacyString(values, "userAgent", "user_agent")),
		Proxy:     firstNonEmpty(account.Proxy, firstLegacyString(values, "proxy")),
	}
	if !validLegacyGitHubSecret(secret) {
		return legacyGitHubSecret{}, false, ErrInvalidGitHubSecret
	}
	return secret, true, nil
}

func openNodeGitHubSecret(encoded string, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalidGitHubSecret
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) <= 28 {
		return nil, ErrInvalidGitHubSecret
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrInvalidGitHubSecret
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrInvalidGitHubSecret
	}
	nonce, tag, ciphertext := raw[:12], raw[12:28], raw[28:]
	combined := append(append([]byte(nil), ciphertext...), tag...)
	plaintext, err := gcm.Open(nil, nonce, combined, nil)
	if err != nil {
		return nil, ErrInvalidGitHubSecret
	}
	return plaintext, nil
}

func firstLegacyCookieValue(account Account, values map[string]any) (any, bool) {
	for _, raw := range []json.RawMessage{account.Cookies, account.RawCookies} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var value any
		if json.Unmarshal(raw, &value) == nil {
			return value, true
		}
	}
	for _, key := range []string{"cookies", "rawCookies", "raw_cookies"} {
		if value, ok := values[key]; ok && value != nil {
			return value, true
		}
	}
	return nil, false
}

func normalizeLegacyGitHubCookies(value any) ([]map[string]any, error) {
	if text, ok := value.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) != nil {
			return nil, ErrInvalidGitHubSecret
		}
		value = decoded
	}
	var raw []any
	switch typed := value.(type) {
	case []any:
		raw = typed
	case map[string]any:
		for name, item := range typed {
			raw = append(raw, map[string]any{"name": name, "value": item, "domain": ".github.com", "path": "/"})
		}
	default:
		return nil, ErrInvalidGitHubSecret
	}
	if len(raw) == 0 || len(raw) > 512 {
		return nil, ErrInvalidGitHubSecret
	}
	result := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		cookie, ok := item.(map[string]any)
		if !ok {
			return nil, ErrInvalidGitHubSecret
		}
		name, nameOK := cookie["name"].(string)
		value, valueOK := cookie["value"].(string)
		if !nameOK || !valueOK || strings.TrimSpace(name) == "" || len(name) > 256 || len(value) > 16384 {
			return nil, ErrInvalidGitHubSecret
		}
		result = append(result, cookie)
	}
	return result, nil
}

func validLegacyGitHubSecret(secret legacyGitHubSecret) bool {
	return strings.TrimSpace(secret.Password) != "" && len(secret.Password) <= 4096 && len(secret.Cookies) > 0 && len(secret.UserAgent) <= 2048 && len(secret.Proxy) <= 2048
}

func firstLegacyString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
