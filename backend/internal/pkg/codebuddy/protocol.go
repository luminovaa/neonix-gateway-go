package codebuddy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	CLIUserAgent = "CLI/2.63.2 CodeBuddy/2.63.2"
	CLIAuthMark  = "cli"
)

// Hosts is the fixed endpoint family for one CodeBuddy realm. A token must
// continue using the realm that issued it.
type Hosts struct {
	Chat    string
	Billing string
	Origin  string
}

var (
	AIHosts        = Hosts{Chat: "https://www.codebuddy.ai", Billing: "https://www.codebuddy.ai", Origin: "https://www.codebuddy.ai"}
	WorkBuddyHosts = Hosts{Chat: "https://www.workbuddy.ai", Billing: "https://www.workbuddy.ai", Origin: "https://www.workbuddy.ai"}
	CNHosts        = Hosts{Chat: "https://copilot.tencent.com", Billing: "https://www.codebuddy.cn", Origin: "https://www.codebuddy.cn"}
)

func ResolveHosts(domain string) Hosts {
	domain = strings.ToLower(strings.TrimSpace(domain))
	switch {
	case strings.Contains(domain, "workbuddy."):
		return WorkBuddyHosts
	case strings.HasSuffix(domain, ".cn"), strings.Contains(domain, "copilot.tencent.com"), strings.Contains(domain, "codebuddy.net"):
		return CNHosts
	default:
		// Existing Neonix CodeBuddy accounts were issued by codebuddy.ai. An
		// absent realm must keep them there rather than silently moving to CN.
		return AIHosts
	}
}

// IsChinaRealm keeps the separate codebuddy-china provider boundary explicit.
// The .ai adapter must never inherit the regional adapter merely because a
// credential contains a CN domain.
func IsChinaRealm(domain string) bool {
	return ResolveHosts(domain) == CNHosts
}

func CommonHeaders(hosts Hosts) http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("Origin", hosts.Origin)
	h.Set("Referer", strings.TrimRight(hosts.Origin, "/")+"/")
	h.Set("User-Agent", CLIUserAgent)
	return h
}

type Identity struct {
	AccessToken  string
	RefreshToken string
	UserID       string
	EnterpriseID string
	Domain       string
	AuthMark     string
}

func (i Identity) IsCLI() bool {
	return i.AuthMark == CLIAuthMark
}

func CLIHeaders(identity Identity) http.Header {
	hosts := ResolveHosts(identity.Domain)
	h := CommonHeaders(hosts)
	h.Set("Accept", "text/event-stream, application/json, */*")
	if identity.AccessToken != "" {
		h.Set("Authorization", "Bearer "+identity.AccessToken)
	} else {
		h.Set("X-No-Authorization", "1")
	}
	if identity.UserID != "" {
		h.Set("X-User-Id", identity.UserID)
	} else {
		h.Set("X-No-User-Id", "1")
	}
	if identity.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", identity.EnterpriseID)
	} else {
		h.Set("X-No-Enterprise-Id", "1")
	}
	if identity.Domain != "" {
		h.Set("X-Domain", identity.Domain)
	} else {
		h.Set("X-No-Department-Info", "1")
	}
	h.Set("X-Product", "SaaS")
	return h
}

type Envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func ParseEnvelope(body []byte) (Envelope, error) {
	var envelope Envelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Envelope{}, errors.New("CodeBuddy returned invalid JSON")
	}
	if envelope.Code != 0 {
		return envelope, fmt.Errorf("CodeBuddy request was rejected (code %d)", envelope.Code)
	}
	return envelope, nil
}

type Tokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

func ParseTokens(data json.RawMessage) (Tokens, error) {
	var tokens Tokens
	if len(data) == 0 || string(data) == "null" {
		return tokens, errors.New("CodeBuddy token response was empty")
	}
	if err := json.Unmarshal(data, &tokens); err != nil {
		return tokens, errors.New("CodeBuddy token response was invalid")
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return tokens, errors.New("CodeBuddy token response did not contain an access token")
	}
	return tokens, nil
}

type AccountInfo struct {
	UserID       string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

func ParseAccountInfo(data json.RawMessage) AccountInfo {
	var info AccountInfo
	_ = json.Unmarshal(data, &info)
	return info
}

func DeviceStartURL(hosts Hosts) string {
	return strings.TrimRight(hosts.Chat, "/") + "/v2/plugin/auth/state?platform=CLI"
}

func DevicePollURL(hosts Hosts, state string) string {
	return strings.TrimRight(hosts.Chat, "/") + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
}

func DeviceAccountURL(hosts Hosts, state string) string {
	return strings.TrimRight(hosts.Chat, "/") + "/v2/plugin/login/account?state=" + url.QueryEscape(state)
}

func RefreshURL(hosts Hosts) string {
	return strings.TrimRight(hosts.Chat, "/") + "/v2/plugin/auth/token/refresh"
}

func ChatURL(hosts Hosts) string {
	return strings.TrimRight(hosts.Chat, "/") + "/v2/chat/completions"
}

func Expiry(now time.Time, expiresIn int64) time.Time {
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return now.Add(time.Duration(expiresIn) * time.Second)
}
