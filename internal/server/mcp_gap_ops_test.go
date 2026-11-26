package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMCPTruncateKeepsRunes(t *testing.T) {
	in := strings.Repeat("ş", 10) // two bytes each
	for _, max := range []int{5, 6, 7} {
		out, cut := mcpTruncate(in, max)
		if !cut || !utf8.ValidString(out) {
			t.Errorf("max %d: cut=%v valid=%v %q", max, cut, utf8.ValidString(out), out)
		}
	}
	if out, cut := mcpTruncate("short", 400_000); cut || out != "short" {
		t.Errorf("short text changed: %q %v", out, cut)
	}
}

// TestMCPOpsAndAudit covers the audit log, the new tools (comments, users, health, backups, themes),
// the commerce scope and the extra option groups end to end.
func TestMCPOpsAndAudit(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	installAndLogin(t, ts, c)
	_, nonce, _ := get(t, c, base+"/wp-admin/admin-ajax?action=rest-nonce")
	mkPass := func(name string, scopes []string) string {
		body := map[string]any{"name": name}
		if scopes == nil {
			body["full_access"] = true
		} else {
			body["scopes"] = scopes
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+"/wp-json/wp/v2/users/me/application-passwords", bytes.NewReader(b))
		req.Header.Set("X-WP-Nonce", nonce)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var ap map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&ap)
		return strAny(ap["password"])
	}
	full := mkPass("Full agent", nil)
	shop := mkPass("Shop agent", []string{"commerce"})
	anon := &http.Client{}
	rpc := func(pass, method string, params map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest("POST", base+"/wp-json/pursae/v1/mcp", bytes.NewReader(b))
		req.SetBasicAuth("admin", pass)
		req.Header.Set("Content-Type", "application/json")
		resp, err := anon.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		res, _ := out["result"].(map[string]any)
		return res
	}
	call := func(pass, tool string, args map[string]any) map[string]any {
		return rpc(pass, "tools/call", map[string]any{"name": tool, "arguments": args})
	}
	names := func(pass string) map[string]bool {
		out := map[string]bool{}
		for _, x := range rpc(pass, "tools/list", nil)["tools"].([]any) {
			out[x.(map[string]any)["name"].(string)] = true
		}
		return out
	}

	// Writes land in the activity log; reads do not.
	if r := call(full, "create_post", map[string]any{"title": "Logged", "status": "draft"}); r["isError"] == true {
		t.Fatalf("create_post: %v", r)
	}
	call(full, "list_posts", nil)
	var n int
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM mcp_activity WHERE tool = 'create_post' AND method = 'POST' AND route = '/wp/v2/posts' AND client = 'Application Password: Full agent' AND status < 300`).Scan(&n)
	if n != 1 {
		t.Errorf("create_post activity rows = %d, want 1", n)
	}
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM mcp_activity WHERE tool = 'list_posts'`).Scan(&n)
	if n != 0 {
		t.Errorf("a read was logged")
	}
	res := call(full, "ai_activity", map[string]any{"tool": "create_post"})
	if res["isError"] == true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), `"tool":"create_post"`) {
		t.Errorf("ai_activity: %v", res)
	}

	// Comments and users tools.
	post := call(full, "create_post", map[string]any{"title": "Commented", "status": "publish"})["structuredContent"].(map[string]any)["id"]
	cm := call(full, "create_comment", map[string]any{"post": post, "content": "Nice", "author_name": "Zoe", "author_email": "zoe@example.test"})
	if cm["isError"] == true {
		t.Fatalf("create_comment: %v", cm)
	}
	cid := cm["structuredContent"].(map[string]any)["id"]
	if r := call(full, "moderate_comment", map[string]any{"id": cid, "status": "hold"}); r["isError"] == true || r["structuredContent"].(map[string]any)["status"] != "hold" {
		t.Errorf("moderate_comment: %v", r)
	}
	if r := call(full, "delete_comment", map[string]any{"id": cid, "force": true}); r["isError"] == true {
		t.Errorf("delete_comment: %v", r)
	}
	u := call(full, "create_user", map[string]any{"username": "ed", "email": "ed@example.test", "password": "Str0ng!pass-phrase", "roles": []string{"editor"}})
	if u["isError"] == true {
		t.Fatalf("create_user: %v", u)
	}
	if r := call(full, "delete_user", map[string]any{"id": u["structuredContent"].(map[string]any)["id"], "force": true}); r["isError"] == true {
		t.Errorf("delete_user: %v", r)
	}

	// Site operations.
	h := call(full, "site_health", nil)
	if h["isError"] == true {
		t.Fatalf("site_health: %v", h)
	}
	sc := h["structuredContent"].(map[string]any)
	if sc["version"] != Version || sc["database"].(map[string]any)["ok"] != true || sc["cron"] == nil {
		t.Errorf("site_health: %v", sc)
	}
	if r := call(full, "list_updates", nil); r["isError"] == true {
		t.Errorf("list_updates: %v", r)
	}
	if r := call(full, "run_update", map[string]any{"type": "plugin", "slug": "nope"}); r["isError"] != true {
		t.Errorf("run_update of an unknown plugin succeeded: %v", r)
	}
	if r := call(full, "run_update", map[string]any{"type": "bogus"}); r["isError"] != true {
		t.Errorf("run_update with a bad type succeeded: %v", r)
	}
	bk := call(full, "create_backup", map[string]any{"categories": []string{"settings"}, "note": "mcp test"})
	if bk["isError"] == true {
		t.Fatalf("create_backup: %v", bk)
	}
	job := bk["structuredContent"].(map[string]any)["job"]
	for i := 0; i < 100; i++ {
		st := call(full, "list_backups", map[string]any{"job": job})["structuredContent"].(map[string]any)
		if st["state"] == "done" {
			break
		}
		if st["state"] == "error" {
			t.Fatalf("backup failed: %v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r := call(full, "list_backups", nil); r["isError"] == true || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "mcp test") {
		t.Errorf("list_backups: %v", r)
	}
	if r := call(full, "delete_theme", map[string]any{"slug": srv.activeTheme().Slug}); r["isError"] != true {
		t.Errorf("deleting the active theme succeeded: %v", r)
	}
	if r := call(full, "delete_theme", map[string]any{"slug": "no-such-theme"}); r["isError"] != true {
		t.Errorf("deleting an unknown theme succeeded")
	}

	// Option groups of the other built-in plugins.
	for _, g := range []string{"multilang", "pixel", "cookie", "contact"} {
		if r := call(full, "get_options", map[string]any{"group": g}); r["isError"] == true {
			t.Errorf("get_options %s: %v", g, r)
		}
	}
	if r := call(full, "update_options", map[string]any{"group": "contact", "settings": map[string]any{"recaptcha_sitekey": "site-123", "recaptcha_secret": "sec-456"}}); r["isError"] == true {
		t.Fatalf("update_options contact: %v", r)
	}
	if srv.revealSecret(srv.settings.String("gontact_recaptcha_secret")) != "sec-456" { // stored sealed
		t.Errorf("contact secret not stored")
	}
	g := call(full, "get_options", map[string]any{"group": "contact"})["structuredContent"].(map[string]any)
	if g["recaptcha_secret"] == "sec-456" {
		t.Errorf("secret returned in clear: %v", g)
	}
	if r := call(full, "update_options", map[string]any{"group": "multilang", "settings": map[string]any{"mt_key": "plain-key"}}); r["isError"] == true {
		t.Fatalf("update_options multilang: %v", r)
	}
	if k := srv.pllOpts().MTKey; k == "" || k == "plain-key" {
		t.Errorf("MT key stored as %q, want it sealed", k)
	}

	// Commerce scope: the tools follow Pursae Commerce being on, and a "commerce" password reaches wc/v3
	// but not the rest of the site.
	if names(full)["commerce_list_products"] != srv.gooOn() {
		t.Errorf("commerce tools listed = %v with commerce on = %v", names(full)["commerce_list_products"], srv.gooOn())
	}
	if err := srv.settings.Set(ctx, map[string]any{gooSetting: true}); err != nil {
		t.Fatal(err)
	}
	if n := names(shop); !n["commerce_list_products"] || !n["commerce_update_stock"] || n["list_posts"] || n["delete_theme"] {
		t.Errorf("commerce-scoped tool list wrong: %v", n)
	}
	cp := call(shop, "commerce_create_product", map[string]any{"name": "Mug", "regular_price": "9.50", "sku": "MUG-1"})
	if cp["isError"] == true {
		t.Fatalf("commerce_create_product: %v", cp)
	}
	pid := cp["structuredContent"].(map[string]any)["id"]
	st := call(shop, "commerce_update_stock", map[string]any{"id": pid, "stock_quantity": 7})
	if st["isError"] == true || st["structuredContent"].(map[string]any)["stock_quantity"] != 7.0 || st["structuredContent"].(map[string]any)["manage_stock"] != true {
		t.Errorf("commerce_update_stock: %v", st)
	}
	if r := call(shop, "commerce_list_orders", nil); r["isError"] == true {
		t.Errorf("commerce_list_orders: %v", r)
	}
	if r := call(shop, "rest_request", map[string]any{"method": "GET", "path": "/wp/v2/posts"}); r["isError"] != true {
		t.Errorf("a commerce-scoped password reached /wp/v2/posts")
	}
	if code := appScopeForRoute("/wc/v3/products/5"); code != "commerce" {
		t.Errorf("wc/v3 scope = %q", code)
	}
}
