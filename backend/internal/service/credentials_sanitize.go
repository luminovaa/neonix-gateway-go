package service

// SanitizeStoredCredentials strips ephemeral secrets that must never be
// persisted on the provider credential document after OAuth conversion.
// Grok re-login passwords live in a separate encrypted automation-secret
// table and therefore must also be removed from this provider document.
// Call from admin create/update/import/apply-oauth paths.
//
// Cookie is always stripped: bulk paths may pass an empty platform label, and
// session-jar residue must never sit next to OAuth tokens on any platform.
// The platform argument is retained for call-site clarity / future scrubbing.
func SanitizeStoredCredentials(platform string, creds map[string]any) map[string]any {
	if creds == nil {
		return nil
	}
	_ = platform
	for _, key := range []string{
		"password", "relogin_password", "reloginPassword", "sso_token", "sso", "sso-rw",
		"clearTextPassword", "clear_text_password", "cookie",
	} {
		delete(creds, key)
	}
	return creds
}
