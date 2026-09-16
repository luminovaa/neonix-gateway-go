package provider

import "testing"

func TestRegistryCoversNeonixProviderGroups(t *testing.T) {
	for _, id := range []string{"antigravity", "codex", "grok", "m365", "oc", "kiro", "qoder", "codebuddy", "workbuddy", "codebuddy-china"} {
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

func TestRegistryKeepsUnmigratedProviderPlatformsStable(t *testing.T) {
	for _, id := range []string{"kiro", "qoder", "codebuddy", "workbuddy", "codebuddy-china"} {
		definition, ok := Lookup(id)
		if !ok {
			t.Fatalf("missing provider %s", id)
		}
		if definition.TargetPlatform != id {
			t.Fatalf("provider %s must not be routed through an unrelated adapter: %+v", id, definition)
		}
	}
}

func TestRegistryCodeBuddySupportsDeviceOAuthAndLegacyManualCredentials(t *testing.T) {
	definition, ok := Lookup("codebuddy")
	if !ok || !definition.OAuth || !definition.Manual || definition.AccountType != "oauth" {
		t.Fatalf("unexpected CodeBuddy registry definition: %+v, %v", definition, ok)
	}
}

func TestRegistryKiroSupportsGoogleOAuthAndManualImport(t *testing.T) {
	definition, ok := Lookup("kiro")
	if !ok || !definition.OAuth || !definition.Manual || definition.AccountType != "oauth" {
		t.Fatalf("unexpected Kiro registry definition: %+v, %v", definition, ok)
	}
}

func TestBuildSummaryIncludesCoverageAndHistoricalProviders(t *testing.T) {
	items := BuildSummary([]AccountSnapshot{
		{SourceProvider: "antigravity", Platform: "antigravity", Status: "active", Schedulable: true},
		{SourceProvider: "antigravity", Platform: "antigravity", Status: "error", Schedulable: false},
		{Platform: "openai", Status: "active", Schedulable: true},
		{Platform: "retired-provider", Status: "disabled", Schedulable: false},
	})

	byID := make(map[string]SummaryItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	if got := byID["antigravity"]; got.TotalAccounts != 2 || got.ActiveAccounts != 1 || got.ErrorAccounts != 1 {
		t.Fatalf("unexpected antigravity coverage: %+v", got)
	}
	if got := byID["openai"]; got.TotalAccounts != 1 || got.Category != CategoryLegacy {
		t.Fatalf("unexpected legacy openai coverage: %+v", got)
	}
	if got := byID["retired-provider"]; got.TotalAccounts != 1 || got.BannedAccounts != 1 {
		t.Fatalf("unexpected historical coverage: %+v", got)
	}
}
