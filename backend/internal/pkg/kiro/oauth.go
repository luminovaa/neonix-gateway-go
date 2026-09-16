package kiro

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
)

const (
	OIDCBaseURL  = "https://oidc.us-east-1.amazonaws.com"
	RegisterURL  = OIDCBaseURL + "/client/register"
	AuthorizeURL = OIDCBaseURL + "/authorize"
	TokenURL     = OIDCBaseURL + "/token"
	SignInURL    = "https://app.kiro.dev/signin"
	RedirectURI  = "http://127.0.0.1:3128"
	IssuerURL    = "https://view.awsapps.com/start/"
)

var OAuthScopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
	"codewhisperer:transformations",
	"codewhisperer:taskassist",
}

func GenerateOAuthSecret(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func GenerateOAuthSessionID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func OAuthCodeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func BuildSignInURL(state, challenge string) string {
	query := url.Values{
		"state": {state}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "redirect_uri": {RedirectURI},
		"redirect_from": {"KiroIDE"},
	}
	return SignInURL + "?" + query.Encode()
}

func BuildAuthorizeURL(clientID, state, challenge string) string {
	query := url.Values{
		"response_type": {"code"}, "client_id": {clientID},
		"redirect_uri": {RedirectURI}, "scopes": {strings.Join(OAuthScopes, ",")},
		"state": {state}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"},
	}
	return AuthorizeURL + "?" + query.Encode()
}

type CallbackKind int

const (
	CallbackIdentity CallbackKind = iota + 1
	CallbackAuthorization
)

func ParseOAuthCallback(raw, expectedState string) (CallbackKind, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User != nil || parsed.Fragment != "" || parsed.Scheme != "http" {
		return 0, "", errors.New("invalid Kiro callback URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if (host != "127.0.0.1" && host != "localhost") || parsed.Port() != "3128" {
		return 0, "", errors.New("invalid Kiro callback origin")
	}
	query := parsed.Query()
	if rejected := strings.TrimSpace(query.Get("error")); rejected != "" {
		return 0, "", errors.New("Kiro login was rejected")
	}
	if strings.TrimSpace(query.Get("state")) != expectedState {
		return 0, "", errors.New("Kiro OAuth state mismatch")
	}
	if code := strings.TrimSpace(query.Get("code")); code != "" {
		return CallbackAuthorization, code, nil
	}
	if strings.TrimSpace(query.Get("login_option")) != "" || strings.Contains(strings.ToLower(parsed.Path), "signin/callback") {
		return CallbackIdentity, "", nil
	}
	return 0, "", errors.New("Kiro callback is missing login information")
}
