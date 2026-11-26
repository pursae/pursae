package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Option groups that steer sign-in, site mail or the scripts every page loads take writes only from
// a credential with full access; the settings scope still reads them and writes the other groups.
func TestScopedCredentialOptionGroupsFullAccessOnly(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	base := ts.URL
	full, fullCall := mcpTestAgent(t, base, c, "Full", nil)
	settingsPass, settingsCall := mcpTestAgent(t, base, c, "Settings", []string{"settings"})

	patches := map[string]map[string]any{
		"gocdn":    {"assets-url": "https://cdn.attacker.example"},
		"smtp":     {"mail": map[string]any{"from_email": "relay@attacker.example"}},
		"security": {"ip_max_failures": 7},
		"headless": {"user_register": true},
	}
	for group, patch := range patches {
		route := "/pursae/v1/options/" + group
		if code, out := fscCall(t, base, settingsPass, http.MethodPost, route, patch); code != http.StatusForbidden || out["code"] != "rest_scope_forbidden" || fscRequiredScope(out) != appScopeFullAccess {
			t.Errorf("settings scope wrote %s over REST: %d %v", group, code, out)
		}
		if r := settingsCall("update_options", map[string]any{"group": group, "settings": patch}); r["isError"] != true {
			t.Errorf("settings scope wrote %s over MCP: %v", group, r)
		}
		if code, out := fscCall(t, base, settingsPass, http.MethodGet, route, nil); code != http.StatusOK {
			t.Errorf("settings scope read %s: %d %v", group, code, out)
		}
	}
	if got := srv.gocdnLoad(); strings.Contains(strings.ToLower(jsonString(got)), "attacker") {
		t.Fatalf("gocdn changed by a settings-scoped credential: %+v", got)
	}

	// Full access, over REST and MCP, still may.
	if code, out := fscCall(t, base, full, http.MethodPost, "/pursae/v1/options/gocdn", patches["gocdn"]); code != http.StatusOK {
		t.Errorf("full access, gocdn: %d %v", code, out)
	}
	if r := fullCall("update_options", map[string]any{"group": "smtp", "settings": patches["smtp"]}); r["isError"] == true {
		t.Errorf("full access, smtp over MCP: %v", r)
	}
	// A signed-in administrator (cookie and nonce) still may.
	_, nonce, _ := get(t, c, base+"/wp-admin/admin-ajax?action=rest-nonce")
	b, _ := json.Marshal(patches["security"])
	req, _ := http.NewRequest(http.MethodPost, base+"/wp-json/pursae/v1/options/security", bytes.NewReader(b))
	req.Header.Set("X-WP-Nonce", nonce)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin session, security: %d", resp.StatusCode)
	}
	// The other groups stay with the settings scope.
	if code, out := fscCall(t, base, settingsPass, http.MethodPost, "/pursae/v1/options/forum", map[string]any{"title": "Scoped forum"}); code != http.StatusOK {
		t.Errorf("settings scope, forum: %d %v", code, out)
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
