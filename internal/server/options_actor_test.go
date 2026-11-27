package server

import "testing"

// The raw-field checks of /pursae/v1/options judge the REST caller: an Application Password with
// full access (or the themes scope) may change the maintenance page's custom CSS, a settings-only one
// may not. Before, they read only the cookie session, so every token request was refused.
func TestOptionsRawChecksJudgeTheToken(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	base := ts.URL

	settingsOnly, _ := mcpTestAgent(t, base, c, "Settings", []string{"settings"})
	if code, out := fscCall(t, base, settingsOnly, "POST", "/pursae/v1/options/maintenance", map[string]any{"custom_css": "body{color:red}"}); code != 400 {
		t.Errorf("settings scope changed custom CSS: %d %v", code, out)
	}

	full, _ := mcpTestAgent(t, base, c, "Full", nil) // nil scopes: full access
	if code, out := fscCall(t, base, full, "POST", "/pursae/v1/options/maintenance", map[string]any{"custom_css": "body{color:blue}"}); code != 200 {
		t.Errorf("full access could not change custom CSS: %d %v", code, out)
	}
	if got := srv.maintLoad().CustomCSS; got != "body{color:blue}" {
		t.Errorf("custom CSS after a full-access save: %q", got)
	}

	themes, _ := mcpTestAgent(t, base, c, "Themes", []string{"settings", "themes"})
	if code, out := fscCall(t, base, themes, "POST", "/pursae/v1/options/maintenance", map[string]any{"custom_css": "body{color:green}"}); code != 200 {
		t.Errorf("settings + themes could not change custom CSS: %d %v", code, out)
	}
}
