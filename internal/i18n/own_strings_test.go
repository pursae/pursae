package i18n

import (
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	ownPlaceholder = regexp.MustCompile(`%(?:\d+\$)?[sdfu]|%%|%[A-Za-z_]+%`)
	ownTag         = regexp.MustCompile(`<[^>]+>`)
)

func ownSig(re *regexp.Regexp, s string) string {
	m := re.FindAllString(html.UnescapeString(s), -1)
	sort.Strings(m)
	return strings.Join(m, "\x00")
}

// TestOwnStringFiles is the validator for web/languages/wpgo-<locale>.json: wpgo-de_DE.json is the
// reference set of msgids that every language (English variants excepted, they only hold spelling
// differences) must have; wpgo-tr_TR.json, wpgo-sv_SE.json and wpgo-he_IL.json hold more. Each
// translation in every file must keep the placeholders and HTML markup of its msgid (entities may be written as the
// character itself; numbers may be localised, "2FA", "02-Feb").
func TestOwnStringFiles(t *testing.T) {
	dir := filepath.Join("..", "..", "web", "languages")
	load := func(name string) map[string]json.RawMessage {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return m
	}
	ref := load("wpgo-de_DE.json")
	files, _ := filepath.Glob(filepath.Join(dir, "wpgo-*.json"))
	if len(files) < 100 {
		t.Fatalf("only %d language files", len(files))
	}
	for _, f := range files {
		name := filepath.Base(f)
		locale := strings.TrimSuffix(strings.TrimPrefix(name, "wpgo-"), ".json")
		if strings.HasPrefix(locale, "en_") {
			continue
		}
		got := load(name)
		problems := 0
		report := func(format string, args ...any) {
			if problems++; problems <= 5 {
				t.Errorf(locale+": "+format, args...)
			}
		}
		for msgid := range ref {
			if _, ok := got[msgid]; !ok {
				report("missing %.70q", msgid)
			}
		}
		for msgid, raw := range got {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				continue // plural forms: checked by the catalog loader
			}
			if strings.TrimSpace(s) == "" {
				report("empty translation of %.70q", msgid)
				continue
			}
			for what, re := range map[string]*regexp.Regexp{"placeholders": ownPlaceholder, "markup": ownTag} {
				if ownSig(re, msgid) != ownSig(re, s) {
					report("%s differ: %.70q => %.70q", what, msgid, s)
				}
			}
		}
		if problems > 5 {
			t.Errorf("%s: %d problems in all", locale, problems)
		}
	}
}

// The forum's counts and relative times read naturally in Turkish even without a core language
// pack: "%s mins" has its own plural entry, the Count phrases their own entries.
func TestOwnStringsForumTurkish(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "languages", "wpgo-tr_TR.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := parseJSONCatalog(b)
	c := &Catalog{layers: []layer{{domain: "wpgo", mo: f}}}
	if got := c.N("%s min", "%s mins", 20); got != "%s dk" {
		t.Errorf("minutes: %q", got)
	}
	if got := c.N("%s second", "%s seconds", 5); got != "%s sn" {
		t.Errorf("seconds: %q", got)
	}
	for _, k := range []string{"%s members", "%s results", "%s points", "November"} {
		if c.T(k) == k {
			t.Errorf("%q is not translated", k)
		}
	}
}

// The Pursae-only strings of the install screen, the profile's "Pursae updates" row and the
// one-time update e-mails notice have no WordPress msgid to fall back on, so the Turkish file carries
// them; the profile's &#8217; matches whichever form the page has.
func TestOwnStringsInstallTurkish(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "languages", "wpgo-tr_TR.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Catalog{layers: []layer{{domain: "wpgo", mo: parseJSONCatalog(b)}}}
	for _, k := range []string{
		"Pursae keeps this address to send you Pursae product updates. You can turn this off any time in your user profile.",
		"Pursae updates",
		"Email me Pursae product updates",
		"Pursae keeps this account’s email address to send product updates. Untick to stop them; the address is removed from Pursae within a day.",
		"Want Pursae product update e-mails at <code>%s</code>? They stay off until you turn them on.",
		"Turn them on in your profile",
	} {
		if tr, ok := c.Lookup(k); !ok || tr == k {
			t.Errorf("%.60q is not translated", k)
		}
	}
	// The install screen's and the profile's lines (the latter with &#8217;) as the page translation
	// sees them.
	for _, page := range []string{
		`<p class="description">Pursae keeps this address to send you Pursae product updates. You can turn this off any time in your user profile.</p>`,
		`<p class="description">Pursae keeps this account&#8217;s email address to send product updates. Untick to stop them; the address is removed from Pursae within a day.</p>`,
	} {
		if out := string(c.TranslateHTML([]byte(page))); strings.Contains(out, "Pursae keeps") {
			t.Errorf("left in English: %s", out)
		}
	}
}

// Pursae Security's passkey screens (adminsec_passkey.go) read in Turkish: the button, the second step
// with the site name in markup, the counts and dates of the Security screen, a notice with a name.
func TestOwnStringsPasskeyTurkish(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "languages", "wpgo-tr_TR.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Catalog{layers: []layer{{domain: "wpgo", mo: parseJSONCatalog(b)}}}
	for _, page := range []string{
		`<button type="button" class="button" id="adminsec-passkey-btn"><span class="dashicons" aria-hidden="true"></span> Log in with a passkey</button>`,
		`<p class="adminsec-lead">Use the passkey you saved for <strong>My Site</strong>. Your device asks for your fingerprint, face or screen lock.</p>`,
		`<span class="description"><span>Added November 23, 2026</span> &middot; <span>Last used November 23, 2026</span></span>`,
		`<span>Passkeys: 2</span>`,
		`<p>Passkey “Work laptop” added. You can now log in with it.</p>`,
		`<p><strong>Error:</strong> The passkey could not be verified. Try again, or log in with your password.</p>`,
	} {
		out := string(c.TranslateHTML([]byte(page)))
		text := ownTag.ReplaceAllString(out, "")
		for _, en := range []string{"passkey", "Added", "Last used"} {
			if strings.Contains(text, en) {
				t.Errorf("left in English: %s", out)
				break
			}
		}
	}
}
