// Package workbuddy owns the WorkBuddy Global realm boundary.  It is kept
// separate from codebuddy because the two products must never share accounts,
// cooldowns, or routing pools merely because their upstream protocol matches.
package workbuddy

import "strings"

const Domain = "workbuddy.ai"

// IsGlobalDomain accepts the canonical host and subdomains only.  Unknown or
// blank domains are rejected rather than silently defaulting to another realm.
func IsGlobalDomain(value string) bool {
	host := strings.ToLower(strings.TrimSpace(value))
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimSuffix(strings.Split(host, "/")[0], ".")
	return host == Domain || strings.HasSuffix(host, "."+Domain)
}
