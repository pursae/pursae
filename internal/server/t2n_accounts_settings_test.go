package server

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

// wp/v2/settings: a settings-scoped credential may change the site settings but not the administration
// e-mail address; a full-access credential may, as in WordPress.
func TestT2NAccountsSettingsAdminEmail(t *testing.T) {
	base, _, srv := gooSite(t)
	ctx := context.Background()
	admin, err := srv.store.UserByLoginOrEmail(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	scoped := &restClient{t: t, c: http.DefaultClient, base: base, basic: [2]string{"admin", t2nAccountsAppPassword(t, srv, admin.ID, "{settings}")}}
	code, obj, _, _ := scoped.do("POST", "/wp/v2/settings", map[string]any{"email": "attacker@example.test"})
	if code != http.StatusForbidden || obj["code"] != "rest_scope_forbidden" {
		t.Fatalf("scoped admin_email change: %d %v", code, obj)
	}
	if got := srv.settings.String("admin_email"); got != "admin@example.test" {
		t.Fatalf("admin_email = %q", got)
	}
	// Other settings, and the unchanged address, still go through.
	if code, obj, _, _ := scoped.do("POST", "/wp/v2/settings", map[string]any{"title": "Scoped Title", "email": "admin@example.test"}); code != http.StatusOK || obj["title"] != "Scoped Title" {
		t.Fatalf("scoped title change: %d %v", code, obj)
	}
	full := &restClient{t: t, c: http.DefaultClient, base: base, basic: [2]string{"admin", t2nAccountsAppPassword(t, srv, admin.ID, "{full_access}")}}
	if code, obj, _, _ := full.do("POST", "/wp/v2/settings", map[string]any{"email": "owner@example.test"}); code != http.StatusOK || obj["email"] != "owner@example.test" {
		t.Fatalf("full-access admin_email change: %d %v", code, obj)
	}
}

// /batch/v1 re-checks every inner request's scope: a scoped credential cannot write a full-access-only
// option group (or the administration address) through it.
func TestT2NAccountsBatchFullAccessOnlyGroups(t *testing.T) {
	base, _, srv := gooSite(t)
	ctx := context.Background()
	admin, err := srv.store.UserByLoginOrEmail(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	before := srv.gocdnLoad()
	scoped := &restClient{t: t, c: http.DefaultClient, base: base, basic: [2]string{"admin", t2nAccountsAppPassword(t, srv, admin.ID, "{content,settings}")}}
	reqs := []any{}
	for _, g := range []string{"gocdn", "security", "smtp", "headless"} {
		reqs = append(reqs, map[string]any{"method": "POST", "path": "/pursae/v1/options/" + g, "body": map[string]any{"enabled": true}})
	}
	reqs = append(reqs, map[string]any{"method": "POST", "path": "/wp/v2/settings", "body": map[string]any{"email": "attacker@example.test"}})
	code, obj, _, _ := scoped.do("POST", "/batch/v1", map[string]any{"requests": reqs})
	if code != http.StatusMultiStatus {
		t.Fatalf("batch: %d %v", code, obj)
	}
	responses, _ := obj["responses"].([]any)
	if len(responses) != len(reqs) {
		t.Fatalf("responses: %v", obj)
	}
	for i, r := range responses {
		m, _ := r.(map[string]any)
		if st, _ := m["status"].(float64); int(st) != http.StatusForbidden {
			t.Errorf("inner request %d: status %v body %v", i, m["status"], m["body"])
		}
	}
	if !reflect.DeepEqual(srv.gocdnLoad(), before) {
		t.Error("Pursae CDN settings changed through a scoped batch")
	}
	if got := srv.settings.String("admin_email"); got != "admin@example.test" {
		t.Errorf("admin_email = %q", got)
	}
}
