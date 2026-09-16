package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/luminovaa/neonix-gateway-go/internal/pkg/m365"
	"github.com/stretchr/testify/require"
)

type mailboxOAuthRoundTripper struct {
	status int
	body   string
}

func (r mailboxOAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: r.status, Body: io.NopCloser(strings.NewReader(r.body)), Header: make(http.Header), Request: req}, nil
}

func mailboxTestJWT() string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{"oid": "mailbox-oid", "tid": "mailbox-tenant", "preferred_username": "mailbox@example.com", "name": "Mailbox Owner"})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestMailboxOAuthPKCEExchangeAndReplay(t *testing.T) {
	service := NewMailboxOAuthService()
	service.SetHTTPClient(&http.Client{Transport: mailboxOAuthRoundTripper{status: http.StatusOK, body: `{"access_token":"` + mailboxTestJWT() + `","refresh_token":"mail-refresh","expires_in":3600}`}})
	started, err := service.Start(context.Background())
	require.NoError(t, err)
	parsed, err := url.Parse(started.AuthorizationURL)
	require.NoError(t, err)
	require.Equal(t, m365.MailboxClientID, parsed.Query().Get("client_id"))
	require.Contains(t, parsed.Query().Get("scope"), "IMAP.AccessAsUser.All")
	state := parsed.Query().Get("state")

	_, err = service.Complete(context.Background(), started.LoginID, m365.RedirectURI+"?code=auth&state=wrong")
	require.Error(t, err)
	info, err := service.Complete(context.Background(), started.LoginID, m365.RedirectURI+"?code=auth&state="+url.QueryEscape(state))
	require.NoError(t, err)
	require.Equal(t, "mailbox-oid", info.OID)
	require.Equal(t, "mailbox-tenant", info.TID)
	require.Equal(t, "mailbox@example.com", info.Email)
	require.Equal(t, "mail-refresh", info.RefreshToken)
	replay, err := service.Complete(context.Background(), started.LoginID, m365.RedirectURI+"?code=auth&state="+url.QueryEscape(state))
	require.NoError(t, err)
	require.Equal(t, info, replay)
	service.Consume(started.LoginID)
	_, err = service.Complete(context.Background(), started.LoginID, m365.RedirectURI+"?code=auth&state="+url.QueryEscape(state))
	require.Error(t, err)
}

func TestMailboxOAuthRejectsInvalidCallbackAndIdentity(t *testing.T) {
	service := NewMailboxOAuthService()
	service.SetHTTPClient(&http.Client{Transport: mailboxOAuthRoundTripper{status: http.StatusOK, body: `{"access_token":"opaque","refresh_token":"refresh"}`}})
	started, err := service.Start(context.Background())
	require.NoError(t, err)
	parsed, err := url.Parse(started.AuthorizationURL)
	require.NoError(t, err)
	_, err = service.Complete(context.Background(), started.LoginID, "https://evil.example/callback?code=x&state="+url.QueryEscape(parsed.Query().Get("state")))
	require.Error(t, err)
	_, err = service.Complete(context.Background(), started.LoginID, m365.RedirectURI+"?code=x&state="+url.QueryEscape(parsed.Query().Get("state")))
	require.Error(t, err)
	require.Contains(t, err.Error(), "MAILBOX_OAUTH_IDENTITY_MISSING")
}
