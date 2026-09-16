package filterrule

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyLiteralAndRegexSnapshot(t *testing.T) {
	r := New(nil)
	rules := []compiledRule{{pattern: "Neonix", replacement: "gateway"}}
	r.snapshot.Store(&rules)
	require.Equal(t, "gateway gateway", r.Apply("Neonix Neonix"))

	require.NoError(t, Validate(`server\s+marker`, "", true))
	re := regexpMustCompile(t, `(?i)server\s+marker`)
	rules = []compiledRule{{pattern: `server\s+marker`, replacement: "clean", regex: re}}
	r.snapshot.Store(&rules)
	require.Equal(t, "clean", r.Apply("SERVER marker"))
}

func regexpMustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	value, err := regexp.Compile(pattern)
	require.NoError(t, err)
	return value
}

func TestFilterRequestJSONOnlyFiltersMessageAndSystemText(t *testing.T) {
	r := New(nil)
	rules := []compiledRule{{pattern: "secret", replacement: "clean"}}
	r.snapshot.Store(&rules)
	SetCurrent(r)
	t.Cleanup(func() { SetCurrent(nil) })
	body := []byte(`{"model":"secret-model","system":"secret system","messages":[{"role":"user","content":"secret text"},{"role":"user","content":[{"type":"text","text":"secret part"},{"type":"image_url","image_url":{"url":"https://secret.invalid"}}]}],"tools":[{"name":"secret"}]}`)
	got := string(FilterRequestJSON(body))
	require.JSONEq(t, `{"model":"secret-model","system":"clean system","messages":[{"role":"user","content":"clean text"},{"role":"user","content":[{"type":"text","text":"clean part"},{"type":"image_url","image_url":{"url":"https://secret.invalid"}}]}],"tools":[{"name":"secret"}]}`, got)
}

func TestValidateRejectsInvalidRegexAndEmptyPattern(t *testing.T) {
	require.Error(t, Validate("", "", false))
	require.Error(t, Validate("[", "", true))
}
