package provider

import "testing"

func TestRegistryCoversNeonixProviderGroups(t *testing.T) {
	for _, id := range []string{"antigravity", "codex", "grok", "m365", "oc", "kiro", "qoder", "codebuddy", "codebuddy-china"} {
		definition, ok := Lookup(id)
		if !ok || definition.Category != CategoryProvider || !definition.Routable {
			t.Fatalf("%s is not a routable provider: %+v, %v", id, definition, ok)
		}
	}
	for _, id := range []string{"github", "outlook"} {
		definition, ok := Lookup(id)
		if !ok || definition.Category != CategoryIdentity || definition.Routable {
			t.Fatalf("%s must remain identity-only: %+v, %v", id, definition, ok)
		}
	}
}

func TestRegistryMarksDeprecatedProviders(t *testing.T) {
	for _, id := range []string{"bai", "bb", "codebuff"} {
		if !IsDeprecated(id) {
			t.Fatalf("%s should be deprecated", id)
		}
	}
	copyOfAll := All()
	copyOfAll[0].ID = "mutated"
	if got, _ := Lookup("mutated"); got.ID != "" {
		t.Fatal("All returned mutable global catalog")
	}
}
