package m365

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildAuthorizationURLUsesPKCEAndMicrosoftCallback(t *testing.T) {
	urlValue := BuildAuthorizationURL("state-1", "challenge-1")
	parsed, err := url.Parse(urlValue)
	require.NoError(t, err)
	require.Equal(t, AuthorizeURL, parsed.Scheme+"://"+parsed.Host+parsed.Path)
	query := parsed.Query()
	require.Equal(t, ClientID, query.Get("client_id"))
	require.Equal(t, RedirectURI, query.Get("redirect_uri"))
	require.Equal(t, "state-1", query.Get("state"))
	require.Equal(t, "challenge-1", query.Get("code_challenge"))
	require.Equal(t, "S256", query.Get("code_challenge_method"))
	require.Contains(t, query.Get("scope"), "offline_access")
}

func TestParseCallbackValidatesOriginStateAndCode(t *testing.T) {
	code, err := ParseCallback(RedirectURI+"?code=abc%2B123&state=state-1", "state-1")
	require.NoError(t, err)
	require.Equal(t, "abc+123", code)
	for _, raw := range []string{
		"http://login.microsoftonline.com/common/oauth2/nativeclient?code=abc&state=state-1",
		"https://evil.example/common/oauth2/nativeclient?code=abc&state=state-1",
		RedirectURI + "?code=abc&state=wrong",
		RedirectURI + "?state=state-1",
		RedirectURI + "?error=access_denied&state=state-1",
		RedirectURI + "?code=abc&state=state-1#fragment",
	} {
		_, err := ParseCallback(raw, "state-1")
		require.Error(t, err, raw)
	}
}

func TestDecodeIdentityRequiresOIDAndTID(t *testing.T) {
	header, _ := json.Marshal(map[string]string{"alg": "none"})
	payload, _ := json.Marshal(map[string]any{
		"oid": "oid-1", "tid": "tenant-1", "preferred_username": "owner@example.com", "name": "Owner",
	})
	token := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	identity, err := DecodeIdentity(token)
	require.NoError(t, err)
	require.Equal(t, Identity{OID: "oid-1", TID: "tenant-1", Email: "owner@example.com", Name: "Owner"}, identity)
	require.Equal(t, "oid-1@tenant-1", UserID(identity))

	missingTenantPayload, _ := json.Marshal(map[string]any{"oid": "oid-1"})
	missingTenant := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(missingTenantPayload) + ".sig"
	_, err = DecodeIdentity(missingTenant)
	require.Error(t, err)
}
