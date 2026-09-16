// Package m365 contains fixed Microsoft OAuth constants and pure callback/
// identity helpers used by the M365 provider account flow.
package m365

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const (
	ClientID        = "c0ab8ce9-e9a0-42e7-b064-33d422df41f1"
	Authority       = "https://login.microsoftonline.com/common"
	RedirectURI     = Authority + "/oauth2/nativeclient"
	AuthorizeURL    = Authority + "/oauth2/v2.0/authorize"
	TokenURL        = Authority + "/oauth2/v2.0/token"
	Scope           = "openid profile offline_access https://substrate.office.com/sydney/M365Chat.Read https://substrate.office.com/sydney/sydney.readwrite"
	MailboxClientID = "d3590ed6-52b3-4102-aeff-aad2292ab01c"
	MailboxScope    = "openid profile offline_access https://outlook.office.com/IMAP.AccessAsUser.All"
)

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type Identity struct {
	OID   string
	TID   string
	Email string
	Name  string
}

func GenerateCodeVerifier() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func CodeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func GenerateState() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func GenerateSessionID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func BuildAuthorizationURL(state, challenge string) string {
	return buildAuthorizationURL(ClientID, Scope, state, challenge)
}

func BuildMailboxAuthorizationURL(state, challenge string) string {
	return buildAuthorizationURL(MailboxClientID, MailboxScope, state, challenge)
}

func buildAuthorizationURL(clientID, scope, state, challenge string) string {
	values := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {RedirectURI},
		"response_mode":         {"query"},
		"scope":                 {scope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return AuthorizeURL + "?" + values.Encode()
}

func ParseCallback(raw, expectedState string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "login.microsoftonline.com" || parsed.Path != "/common/oauth2/nativeclient" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("invalid Microsoft OAuth callback origin")
	}
	if callbackError := strings.TrimSpace(parsed.Query().Get("error")); callbackError != "" {
		return "", errors.New("Microsoft login was not completed")
	}
	state := strings.TrimSpace(parsed.Query().Get("state"))
	if state == "" || expectedState == "" || subtle.ConstantTimeCompare([]byte(state), []byte(expectedState)) != 1 {
		return "", errors.New("OAuth state does not match this login session")
	}
	code := strings.TrimSpace(parsed.Query().Get("code"))
	if code == "" {
		return "", errors.New("Microsoft callback does not contain an authorization code")
	}
	return code, nil
}

func DecodeIdentity(accessToken string) (Identity, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("Microsoft access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		padding := parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4)
		payload, err = base64.URLEncoding.DecodeString(padding)
	}
	if err != nil {
		return Identity{}, errors.New("Microsoft access token payload is invalid")
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, errors.New("Microsoft access token claims are invalid")
	}
	identity := Identity{
		OID:   firstString(claims, "oid", "sub"),
		TID:   firstString(claims, "tid", "tenant_id"),
		Email: firstString(claims, "preferred_username", "unique_name", "upn", "email"),
		Name:  firstString(claims, "name"),
	}
	if identity.OID == "" || identity.TID == "" {
		return Identity{}, errors.New("Microsoft token did not include oid and tid")
	}
	return identity, nil
}

func firstString(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func UserID(identity Identity) string {
	return fmt.Sprintf("%s@%s", strings.TrimSpace(identity.OID), strings.TrimSpace(identity.TID))
}
