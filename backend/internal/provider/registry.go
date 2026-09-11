// Package provider is the canonical provider catalog for the Neonix Go
// backend. It is independent from HTTP handlers so routing, account
// migration, and the future admin API share the same metadata.
package provider

type Category string

const (
	CategoryProvider Category = "provider"
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

var definitions = []Definition{
	{ID: "antigravity", Category: CategoryProvider, Routable: true, OAuth: true, Manual: true, TargetPlatform: "antigravity", AccountType: "oauth"},
	{ID: "codex", Category: CategoryProvider, Routable: true, OAuth: true, Manual: true, TargetPlatform: "openai", AccountType: "oauth"},
	{ID: "grok", Category: CategoryProvider, Routable: true, OAuth: true, Manual: true, TargetPlatform: "grok", AccountType: "oauth"},
	{ID: "m365", Category: CategoryProvider, Routable: true, OAuth: true, Manual: true, TargetPlatform: "m365", AccountType: "oauth"},
	{ID: "oc", Category: CategoryProvider, Routable: true, OAuth: false, Manual: true, TargetPlatform: "openai", AccountType: "apikey"},
	{ID: "kiro", Category: CategoryProvider, Routable: true, OAuth: false, Manual: true, TargetPlatform: "openai", AccountType: "apikey"},
	{ID: "qoder", Category: CategoryProvider, Routable: true, OAuth: false, Manual: true, TargetPlatform: "openai", AccountType: "apikey"},
	{ID: "codebuddy", Category: CategoryProvider, Routable: true, OAuth: false, Manual: true, TargetPlatform: "openai", AccountType: "apikey"},
	{ID: "codebuddy-china", Category: CategoryProvider, Routable: true, OAuth: false, Manual: true, TargetPlatform: "openai", AccountType: "apikey"},
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
