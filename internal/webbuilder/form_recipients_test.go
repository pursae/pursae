package webbuilder

import (
	"encoding/json"
	"strings"
	"testing"
)

// Form recipients are privileged: an author's save keeps the stored ones and drops their own, a
// library component (no stored doc) loses them, an unfiltered_html user writes them.
func TestFormRecipientsPrivileged(t *testing.T) {
	doc := func(to string) string {
		return `{"site":{"brand":"#0a7","fontPair":"modern","corners":"soft"},"sections":[{"id":"f1","kind":"form","variant":"default","content":{"title":"Contact","formTo":"` + to + `"}}]}`
	}
	to := func(r *DocResult) string {
		for _, s := range FormSchemas(&r.Doc) {
			return s.To
		}
		return "?"
	}
	admin := mustParse(t, doc("boss@example.com"), DocOptions{UnfilteredHTML: true})
	if !admin.Valid() || to(admin) != "boss@example.com" {
		t.Fatalf("admin save: valid=%v to=%q %v", admin.Valid(), to(admin), admin.Errors)
	}
	author := mustParse(t, doc("evil@attacker.test"), DocOptions{Stored: []json.RawMessage{admin.JSON}})
	if got := to(author); got != "boss@example.com" {
		t.Fatalf("author save kept %q, want the stored recipient", got)
	}
	if !strings.Contains(strings.Join(author.Stripped, ","), StrippedRecipients) {
		t.Fatalf("stripped = %v", author.Stripped)
	}
	fresh := mustParse(t, doc("evil@attacker.test"), DocOptions{})
	if got := to(fresh); got != "" {
		t.Fatalf("author save without a stored doc kept %q", got)
	}
	sec, err := ParseSection([]byte(`{"id":"f1","kind":"form","variant":"default","content":{"formTo":"evil@attacker.test","formCc":"x@attacker.test"}}`), DocOptions{})
	if err != nil || strings.Contains(string(sec.JSON), "attacker") {
		t.Fatalf("library section kept recipients: %s %v", sec.JSON, err)
	}
	legacy := StripFormRecipients(json.RawMessage(`{"id":"f1","kind":"form","content":{"formTo":"evil@attacker.test"}}`))
	if strings.Contains(string(legacy), "attacker") {
		t.Fatalf("StripFormRecipients kept: %s", legacy)
	}
}
