package server

import (
	"strings"
	"testing"

	"wpgo/internal/webbuilder"
)

// An escape is part of an identifier, never a quote: an escaped quote cannot hide a url() from the
// URL allowlist, and an escaped function name is still url().
func TestT2NMiscWebBuilderCSSEscapedURL(t *testing.T) {
	for _, css := range []string{
		`.a{background:\22  url(data:text/html,x) \22}`,
		`.a{background:\" url(ftp://example.com/x.png) \"}`,
		`.a{background:u\72l(ftp://example.com/x.png)}`,
		`.a{background:\75 rl(data:text/html,x)}`,
		`.a{background:url(ftp://example.com/x\).png)}`,
	} {
		out, _, err := webbuilder.SanitizeCSS(css, webbuilder.CSSOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "background") {
			t.Errorf("%s kept as %q", css, out)
		}
	}
	for _, css := range []string{
		`.a{background:url("https://example.com/a.png")}`,
		`.a{background:url(/wp-content/uploads/a\ b.png)}`,
		`.a{content:"\22 url(ftp://x) \22"}`,
		`.a{font-family:\22 Fancy\22, serif}`,
	} {
		out, _, err := webbuilder.SanitizeCSS(css, webbuilder.CSSOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "{") || strings.Count(out, ":") < 1 {
			t.Errorf("%s dropped: %q", css, out)
		}
	}
}
