package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// A users+commerce scoped credential is not full access: through wc/v3 customers it can neither plant
// sign-in state in users.meta nor change another account's password or e-mail (single and batch).
func TestT2RestMetaScopedCustomerSecurityMeta(t *testing.T) {
	base, c, srv := gooSite(t)
	ctx := context.Background()
	rc := newRESTClient(t, c, base)
	code, obj, _, _ := rc.do("POST", "/wp/v2/users/me/application-passwords", map[string]any{"name": "Shop", "scopes": []string{"users", "commerce"}})
	if code != http.StatusCreated {
		t.Fatalf("create scoped password: %d %v", code, obj)
	}
	pass, _ := obj["password"].(string)
	hdr := gooBasic("admin", pass)
	anon := &http.Client{}

	victim := asCreateUser(t, srv, "victim", "administrator", asStrongPass)
	before, _ := srv.store.UserByID(ctx, victim)
	url := fmt.Sprintf("%s/wp-json/wc/v3/customers/%d", base, victim)

	planted := []any{
		map[string]any{"key": "password_reset", "value": map[string]any{"hash": "x", "expires": 9999999999}},
		map[string]any{"key": "adminsec_force_password", "value": "admin"},
		map[string]any{"key": "forum_google_sub", "value": "attacker"},
		map[string]any{"key": "favourite_colour", "value": "teal"},
	}
	r := gooReq(t, anon, "PUT", url, map[string]any{"meta_data": planted}, hdr)
	if r.Code != http.StatusOK {
		t.Fatalf("meta update: %d %s", r.Code, r.Raw)
	}
	r = gooReq(t, anon, "POST", base+"/wp-json/wc/v3/customers/batch", map[string]any{"update": []any{map[string]any{"id": victim, "meta_data": planted}}}, hdr)
	if r.Code != http.StatusOK {
		t.Fatalf("batch meta update: %d %s", r.Code, r.Raw)
	}
	u, _ := srv.store.UserByID(ctx, victim)
	for _, k := range []string{"password_reset", "adminsec_force_password", "forum_google_sub"} {
		if _, ok := u.Meta[k]; ok {
			t.Errorf("wc/v3 customers wrote security meta %q", k)
		}
	}
	if u.Meta["favourite_colour"] != "teal" {
		t.Errorf("ordinary customer meta lost: %v", u.Meta["favourite_colour"])
	}

	if r := gooReq(t, anon, "PUT", url, map[string]any{"password": "Taken-over-0123456789!"}, hdr); r.Code != http.StatusForbidden {
		t.Errorf("scoped password change of another account: %d %s", r.Code, r.Raw)
	}
	if r := gooReq(t, anon, "PUT", url, map[string]any{"email": "attacker@example.test"}, hdr); r.Code != http.StatusForbidden {
		t.Errorf("scoped e-mail change of another account: %d %s", r.Code, r.Raw)
	}
	_ = gooReq(t, anon, "POST", base+"/wp-json/wc/v3/customers/batch", map[string]any{"update": []any{
		map[string]any{"id": victim, "email": "attacker@example.test", "password": "Taken-over-0123456789!"}}}, hdr)
	u, _ = srv.store.UserByID(ctx, victim)
	if u.Email != before.Email || u.PasswordHash != before.PasswordHash {
		t.Errorf("scoped credential changed another account's credentials: %q", u.Email)
	}

	// Shop integrations keep managing customers: the users scope reaches level-0 accounts.
	cust := asCreateUser(t, srv, "shopper", "customer", asStrongPass)
	if r := gooReq(t, anon, "PUT", fmt.Sprintf("%s/wp-json/wc/v3/customers/%d", base, cust), map[string]any{"email": "shopper-new@example.test"}, hdr); r.Code != http.StatusOK {
		t.Errorf("scoped e-mail change of a customer: %d %s", r.Code, r.Raw)
	}
	editor := asCreateUser(t, srv, "staff", "editor", asStrongPass)
	if r := gooReq(t, anon, "PUT", fmt.Sprintf("%s/wp-json/wc/v3/customers/%d", base, editor), map[string]any{"password": "Taken-over-0123456789!"}, hdr); r.Code != http.StatusForbidden {
		t.Errorf("scoped password change of an editor: %d %s", r.Code, r.Raw)
	}
	if r := gooReq(t, anon, "DELETE", fmt.Sprintf("%s/wp-json/wc/v3/customers/%d?force=true", base, editor), nil, hdr); r.Code != http.StatusForbidden {
		t.Errorf("scoped delete of an editor: %d %s", r.Code, r.Raw)
	}

	// Full access (the administrator's own unscoped API) still may.
	api := gooAdminAPI(t, base, c)
	if r := api("PUT", fmt.Sprintf("/customers/%d", victim), map[string]any{"email": "victim-new@example.test"}); r.Code != http.StatusOK {
		t.Errorf("administrator e-mail change: %d %s", r.Code, r.Raw)
	}
}

func TestT2RestMetaProtectedUserMetaKey(t *testing.T) {
	for _, k := range []string{"password_reset", "adminsec_force_password", "adminsec_grace_start", "forum_google_sub", "forum_verify",
		"forum_email_verified", "forum_no_password", "session_tokens", "wp_capabilities", "_new_email", "_web_builder_kit_trust"} {
		if !gooProtectedUserMetaKey(k) {
			t.Errorf("%q is not protected", k)
		}
	}
	for _, k := range []string{"favourite_colour", "billing_vat", "nickname_pref"} {
		if gooProtectedUserMetaKey(k) {
			t.Errorf("%q is protected", k)
		}
	}
}
