package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func gooCommerceToken(t *testing.T, base string, c *http.Client) map[string]string {
	t.Helper()
	rc := newRESTClient(t, c, base)
	code, obj, _, _ := rc.do("POST", "/wp/v2/users/me/application-passwords", map[string]any{"name": "Shop", "scopes": []string{"commerce"}})
	if code != http.StatusCreated {
		t.Fatalf("create commerce password: %d %v", code, obj)
	}
	pass, _ := obj["password"].(string)
	return gooBasic("admin", pass)
}

// A commerce-scoped token has edit_products but not unfiltered_html: product HTML is kses-filtered.
func TestGooV3ProductHTMLFilteredWithoutUnfilteredHTML(t *testing.T) {
	base, c, _ := gooSite(t)
	hdr := gooCommerceToken(t, base, c)
	anon := &http.Client{}
	body := map[string]any{"name": "X", "type": "simple", "description": "<p>ok</p><script>alert(1)</script>",
		"short_description": "<img src=x onerror=alert(2)>", "purchase_note": "<script>alert(3)</script>"}
	r := gooReq(t, anon, "POST", base+"/wp-json/wc/v3/products", body, hdr)
	if r.Code != http.StatusCreated && r.Code != http.StatusOK {
		t.Fatalf("create: %d %s", r.Code, r.Raw)
	}
	for _, k := range []string{"description", "short_description", "purchase_note"} {
		v := gooJS(r.Body, k)
		if strings.Contains(v, "<script") || strings.Contains(v, "onerror") {
			t.Errorf("%s stored unfiltered: %q", k, v)
		}
	}
	if !strings.Contains(gooJS(r.Body, "description"), "<p>ok</p>") {
		t.Errorf("safe HTML lost: %q", gooJS(r.Body, "description"))
	}
	id := int64(gooInt(gooJ(r.Body, "id")))
	vr := gooReq(t, anon, "POST", fmt.Sprintf("%s/wp-json/wc/v3/products/%d/variations", base, id), map[string]any{"description": "<script>alert(4)</script>"}, hdr)
	if strings.Contains(gooJS(vr.Body, "description"), "<script") {
		t.Errorf("variation description stored unfiltered: %d %s", vr.Code, vr.Raw)
	}
	// An unscoped administrator keeps raw HTML, as in WordPress.
	api := gooAdminAPI(t, base, c)
	ar := api("PUT", fmt.Sprintf("/products/%d", id), map[string]any{"description": "<script>ok()</script>"})
	if !strings.Contains(gooJS(ar.Body, "description"), "<script>") {
		t.Errorf("administrator HTML was filtered: %d %s", ar.Code, ar.Raw)
	}
}

// Changing a user's e-mail (the recovery channel) needs the same scope/level check as the password.
func TestGooV3CustomerEmailNeedsCredentialCheck(t *testing.T) {
	base, c, srv := gooSite(t)
	ctx := context.Background()
	anon := &http.Client{}
	// A shop manager granted edit_users must not take over the administrator's recovery e-mail.
	mgr := asCreateUser(t, srv, "mgr", "shop_manager", asStrongPass)
	if err := srv.store.SetUserCapExceptions(ctx, mgr, map[string]string{"edit_users": "allow", "list_users": "allow"}); err != nil {
		t.Fatal(err)
	}
	ck, cs, _, _ := srv.gooRESTNewKey(ctx, mgr, "Mgr", "read_write")
	hdr := gooBasic(ck, cs)
	if r := gooReq(t, anon, "PUT", base+"/wp-json/wc/v3/customers/1", map[string]any{"email": "attacker@example.com"}, hdr); r.Code != http.StatusForbidden {
		t.Fatalf("lower-level email change of admin: %d %s", r.Code, r.Raw)
	}
	if r := gooReq(t, anon, "POST", base+"/wp-json/wc/v3/customers/batch", map[string]any{"update": []any{map[string]any{"id": 1, "email": "attacker@example.com"}}}, hdr); strings.Contains(r.Raw, "attacker@example.com") {
		t.Errorf("batch email change went through: %s", r.Raw)
	}
	if admin, _ := srv.store.UserByID(ctx, 1); admin.Email == "attacker@example.com" {
		t.Fatal("administrator e-mail was changed")
	}
	cust := asCreateUser(t, srv, "cust", "customer", asStrongPass)
	if r := gooReq(t, anon, "PUT", fmt.Sprintf("%s/wp-json/wc/v3/customers/%d", base, cust), map[string]any{"email": "cust2@example.com"}, hdr); r.Code != http.StatusOK {
		t.Errorf("shop manager could not change a customer e-mail: %d %s", r.Code, r.Raw)
	}
	api := gooAdminAPI(t, base, c)
	if r := api("PUT", fmt.Sprintf("/customers/%d", cust), map[string]any{"email": "new@example.com"}); r.Code != http.StatusOK {
		t.Errorf("administrator email change: %d %s", r.Code, r.Raw)
	}
}

// A batch delete needs the resource's delete capability, not only the write one.
func TestGooV3BatchDeleteNeedsDeleteCap(t *testing.T) {
	base, _, srv := gooSite(t)
	ctx := context.Background()
	mgr := asCreateUser(t, srv, "mgr2", "shop_manager", asStrongPass)
	if err := srv.store.SetUserCapExceptions(ctx, mgr, map[string]string{"edit_users": "allow", "list_users": "allow", "delete_users": "deny"}); err != nil {
		t.Fatal(err)
	}
	cust := asCreateUser(t, srv, "victim", "customer", asStrongPass)
	ck, cs, _, _ := srv.gooRESTNewKey(ctx, mgr, "Mgr", "read_write")
	r := gooReq(t, &http.Client{}, "POST", base+"/wp-json/wc/v3/customers/batch", map[string]any{"delete": []any{cust}}, gooBasic(ck, cs))
	if r.Code != http.StatusOK || !strings.Contains(r.Raw, "woocommerce_rest_cannot_delete") {
		t.Errorf("batch delete without delete_users: %d %s", r.Code, r.Raw)
	}
	if u, _ := srv.store.UserByID(ctx, cust); u == nil {
		t.Fatal("customer was deleted")
	}
}
