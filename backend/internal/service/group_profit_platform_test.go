package service

import "testing"

func TestNormalizeGroupPlatformDefaultsToAnthropic(t *testing.T) {
	if got := NormalizeGroupPlatform(""); got != PlatformAnthropic {
		t.Fatalf("empty platform should normalize to %s, got %s", PlatformAnthropic, got)
	}
}

func TestProfitControlUnsupportedForEveryNeonixPlatform(t *testing.T) {
	platforms := []string{PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformGrok, PlatformAntigravity}
	for _, platform := range platforms {
		if profitControlPlatformSupported(platform) {
			t.Fatalf("profit control unexpectedly enabled for %s", platform)
		}
	}
}
