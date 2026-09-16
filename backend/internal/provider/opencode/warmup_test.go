package opencode

import (
	"strings"
	"testing"
)

func TestResolveWarmupCandidatesUsesAvailableCachedRows(t *testing.T) {
	order := 20
	first := 10
	got := ResolveWarmupCandidates([]CachedModel{
		{ModelID: "oc/zeta-free", ActualModelID: "zeta-free", Status: "AVAILABLE", SortOrder: &order},
		{ModelID: "oc/alpha-free", ActualModelID: "alpha-free", Status: "available", SortOrder: &first},
		{ModelID: "oc/maintenance", ActualModelID: "maintenance", Status: "MAINTENANCE"},
		{ModelID: "oc/deleted", ActualModelID: "deleted", Status: "AVAILABLE", Deleted: true},
	}, []string{"oc/bootstrap"}, nil)
	if len(got) != 2 || got[0].ModelID != "oc/alpha-free" || got[0].ActualModelID != "alpha-free" || got[1].ModelID != "oc/zeta-free" {
		t.Fatalf("unexpected candidates: %+v", got)
	}
}

func TestResolveWarmupCandidatesOnlyBootstrapsEmptyCatalogue(t *testing.T) {
	got := ResolveWarmupCandidates(nil, []string{"oc/bootstrap"}, map[string]string{"oc/bootstrap": "bootstrap-upstream"})
	if len(got) != 1 || got[0].ActualModelID != "bootstrap-upstream" {
		t.Fatalf("unexpected bootstrap candidates: %+v", got)
	}
	got = ResolveWarmupCandidates([]CachedModel{{ModelID: "oc/retired", Status: "MAINTENANCE"}}, []string{"oc/bootstrap"}, nil)
	if len(got) != 0 {
		t.Fatalf("retired catalogue must not bootstrap: %+v", got)
	}
}

func TestIsModelUnavailableErrorIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		status int
		msg    string
		want   bool
	}{
		{400, "unsupported model", true},
		{404, "model not found", true},
		{404, "route not found", false},
		{401, "invalid token", false},
		{429, "model rate limit", false},
		{500, "model unavailable", false},
	} {
		if got := IsModelUnavailableError(tc.status, tc.msg); got != tc.want {
			t.Errorf("IsModelUnavailableError(%d, %q) = %v, want %v", tc.status, tc.msg, got, tc.want)
		}
	}
	if !strings.Contains(DescribeStaleCatalogue("last rejection").Error(), "sync models") {
		t.Fatal("stale catalogue error should be actionable")
	}
}

func TestParseSortOrder(t *testing.T) {
	if got := ParseSortOrder("12"); got == nil || *got != 12 {
		t.Fatalf("string sort order = %+v", got)
	}
	if got := ParseSortOrder("bad"); got != nil {
		t.Fatalf("invalid sort order = %+v", got)
	}
}
