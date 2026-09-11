// Package provider is the canonical provider catalog for the Neonix Go
// backend. It is independent from HTTP handlers so routing, account
// migration, and the future admin API share the same metadata.
package provider

import "sort"

type Category string

const (
	// routable is the wire value used by the existing Accounts UI. Keep the
	// internal name Provider so callers can group it semantically without
	// duplicating the legacy API vocabulary.
	CategoryProvider Category = "routable"
	CategoryIdentity Category = "identity"
	CategoryBYOK     Category = "byok"
	CategoryLegacy   Category = "legacy"
)

// Definition describes account capabilities. Names are stable storage
// identifiers; labels belong to the UI locale layer.
type Definition struct {
	ID             string   `json:"id"`
	Category       Category `json:"category"`
	Routable       bool     `json:"routable"`
	OAuth          bool     `json:"oauth"`
	Manual         bool     `json:"manual"`
	Deprecated     bool     `json:"deprecated"`
	TargetPlatform string   `json:"targetPlatform"`
	AccountType    string   `json:"accountType"`
}

// AccountSnapshot is the non-secret subset needed to build the operator
// provider dashboard. SourceProvider is preferred when it was written by the
// Neonix migration importer; Platform remains the compatibility fallback for
// legacy rows.
type AccountSnapshot struct {
	SourceProvider string
	Platform       string
	Status         string
	Schedulable    bool
}

type SummaryItem struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Enabled           bool     `json:"enabled"`
	TotalAccounts     int      `json:"totalAccounts"`
	ActiveAccounts    int      `json:"activeAccounts"`
	ErrorAccounts     int      `json:"errorAccounts"`
	ExhaustedAccounts int      `json:"exhaustedAccounts"`
	BannedAccounts    int      `json:"bannedAccounts"`
	Category          Category `json:"category"`
}

var definitions = []Definition{
	{ID: "antigravity", Category: CategoryProvider, Routable: true, OAuth: true, Manual: true, TargetPlatform: "antigravity", AccountType: "oauth"},
	{ID: "codex", Category: CategoryProvider, Routable: true, OAuth: true, Manual: true, TargetPlatform: "openai", AccountType: "oauth"},
	{ID: "grok", Category: CategoryProvider, Routable: true, OAuth: true, Manual: true, TargetPlatform: "grok", AccountType: "oauth"},
	{ID: "m365", Category: CategoryProvider, Routable: true, OAuth: true, Manual: true, TargetPlatform: "m365", AccountType: "oauth"},
	{ID: "oc", Category: CategoryProvider, Routable: true, OAuth: false, Manual: true, TargetPlatform: "openai", AccountType: "apikey"},
	// These providers have provider-specific adapters in the Neonix contract.
	// Keep their storage platform stable until each adapter is migrated; mapping
	// them to openai would silently route credentials through the wrong upstream.
	{ID: "kiro", Category: CategoryProvider, Routable: true, OAuth: false, Manual: true, TargetPlatform: "kiro", AccountType: "apikey"},
	{ID: "qoder", Category: CategoryProvider, Routable: true, OAuth: false, Manual: true, TargetPlatform: "qoder", AccountType: "apikey"},
	{ID: "codebuddy", Category: CategoryProvider, Routable: true, OAuth: false, Manual: true, TargetPlatform: "codebuddy", AccountType: "apikey"},
	{ID: "codebuddy-china", Category: CategoryProvider, Routable: true, OAuth: false, Manual: true, TargetPlatform: "codebuddy-china", AccountType: "apikey"},
	{ID: "github", Category: CategoryIdentity, Manual: true, AccountType: "oauth"},
	{ID: "outlook", Category: CategoryIdentity, OAuth: true, AccountType: "oauth"},
	{ID: "byok", Category: CategoryBYOK, Manual: true, TargetPlatform: "openai", AccountType: "apikey"},
	{ID: "bai", Category: CategoryLegacy, Deprecated: true},
	{ID: "bb", Category: CategoryLegacy, Deprecated: true},
	{ID: "codebuff", Category: CategoryLegacy, Deprecated: true},
}

// All returns a copy, keeping callers from mutating the global catalog.
func All() []Definition {
	out := make([]Definition, len(definitions))
	copy(out, definitions)
	return out
}

func Lookup(id string) (Definition, bool) {
	for _, definition := range definitions {
		if definition.ID == id {
			return definition, true
		}
	}
	return Definition{}, false
}

func IsDeprecated(id string) bool {
	definition, ok := Lookup(id)
	return ok && definition.Deprecated
}

func TargetPlatform(id string) string {
	definition, ok := Lookup(id)
	if !ok || definition.TargetPlatform == "" {
		return id
	}
	return definition.TargetPlatform
}

func TargetAccountType(id string) string {
	definition, ok := Lookup(id)
	if !ok || definition.AccountType == "" {
		return "apikey"
	}
	return definition.AccountType
}

// DeprecatedIDs returns stable identifiers no longer eligible for new routing.
func DeprecatedIDs() []string {
	ids := make([]string, 0, 3)
	for _, definition := range definitions {
		if definition.Deprecated {
			ids = append(ids, definition.ID)
		}
	}
	return ids
}

// BuildSummary keeps provider coverage derived from durable account rows and
// the canonical registry. It never copies credentials into the response.
func BuildSummary(accounts []AccountSnapshot) []SummaryItem {
	items := make([]SummaryItem, 0, len(definitions))
	index := make(map[string]int, len(definitions))
	for _, definition := range definitions {
		index[definition.ID] = len(items)
		items = append(items, SummaryItem{
			ID: definition.ID, Name: definition.ID, Enabled: !definition.Deprecated,
			Category: definition.Category,
		})
	}
	for _, account := range accounts {
		id := account.SourceProvider
		if id == "" {
			id = account.Platform
		}
		if id == "" {
			continue
		}
		itemIndex, ok := index[id]
		if !ok {
			// Legacy rows can carry a platform that is no longer in the registry.
			// Keep it visible so migration never hides an existing account.
			itemIndex = len(items)
			index[id] = itemIndex
			items = append(items, SummaryItem{ID: id, Name: id, Enabled: true, Category: CategoryLegacy})
		}
		item := &items[itemIndex]
		item.TotalAccounts++
		if account.Status == "active" && account.Schedulable {
			item.ActiveAccounts++
		}
		if account.Status == "error" {
			item.ErrorAccounts++
		}
		if !account.Schedulable || account.Status == "disabled" || account.Status == "expired" {
			item.BannedAccounts++
		}
	}
	// Keep registry order stable and append historical providers in a
	// deterministic order for cache keys and UI deep links.
	if len(items) > len(definitions) {
		sort.SliceStable(items[len(definitions):], func(i, j int) bool {
			return items[len(definitions)+i].ID < items[len(definitions)+j].ID
		})
	}
	return items
}
