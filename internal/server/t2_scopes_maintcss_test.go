package server

import (
	"strings"
	"testing"
)

// The maintenance page's custom CSS can never open or close a tag inside its style element, however
// the closing tag is nested, cased or spaced.
func TestMaintSafeCSSNoTagSurvives(t *testing.T) {
	for _, in := range []string{
		"</style><script>alert(1)</script>",
		"<</style>/style><script>alert(1)</script>",
		"<<//style>/style>>",
		"</STYLE ><ScRiPt>x</sCrIpT>",
		"< /style>",
		"<!--</style>-->",
		"a{} <</</style>/style>/style>",
		"<",
	} {
		got := maintSafeCSS(in)
		if strings.Contains(got, "<") {
			t.Errorf("maintSafeCSS(%q) = %q keeps a \"<\"", in, got)
		}
	}
	for in, want := range map[string]string{
		"a{color:red}":               "a{color:red}",
		`a::before{content:"<b>"}`:   `a::before{content:"\3C b>"}`,
		"p > span{margin:0}":         "p > span{margin:0}",
		"@media (max-width:600px){}": "@media (max-width:600px){}",
	} {
		if got := maintSafeCSS(in); got != want {
			t.Errorf("maintSafeCSS(%q) = %q, want %q", in, got, want)
		}
	}
}

// A changed custom CSS is raw code, like the custom HTML: it needs unfiltered_html.
func TestMaintCheckRawCustomCSS(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	base := ts.URL
	content, _ := mcpTestAgent(t, base, c, "Settings", []string{"settings"})
	old := srv.maintLoad()
	if code, out := fscCall(t, base, content, "POST", "/pursae/v1/options/maintenance", map[string]any{"custom_css": old.CustomCSS + "body{color:red}"}); code != 400 {
		t.Errorf("settings scope changed custom CSS: %d %v", code, out)
	}
	if srv.maintLoad().CustomCSS != old.CustomCSS {
		t.Errorf("custom CSS stored without unfiltered_html")
	}
}
