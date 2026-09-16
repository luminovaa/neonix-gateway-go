package workbuddy

import "testing"

func TestIsGlobalDomain(t *testing.T) {
	for _, value := range []string{"workbuddy.ai", "www.workbuddy.ai", "https://www.workbuddy.ai/"} {
		if !IsGlobalDomain(value) {
			t.Fatalf("expected %q to be accepted", value)
		}
	}
	for _, value := range []string{"", "codebuddy.ai", "workbuddy.ai.invalid", "copilot.tencent.com"} {
		if IsGlobalDomain(value) {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}
