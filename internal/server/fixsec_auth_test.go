package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// A forced password change (Pursae Security) closes every API door, not only cookie REST: an
// Application Password made earlier, a Headless password login, and the profile form's
// "Add New Application Password" at the gate.
func TestFixsecBlockedAccountNoAPI(t *testing.T) {
	base, c, srv := hgSite(t)
	ctx := context.Background()
	pool := getPool(t)
	asToggle(t, c, base, "activate")
	st := srv.hgLoad()
	st.UserAuth = true
	_ = srv.hgSaveSettings(ctx, st)

	rc := newRESTClient(t, c, base)
	code, ap, _, _ := rc.do("POST", "/wp/v2/users/me/application-passwords", map[string]any{"name": "cli"})
	appPass, _ := ap["password"].(string)
	if code != http.StatusCreated || appPass == "" {
		t.Fatalf("control: create application password: %d %v", code, ap)
	}
	basic := func() (int, string) {
		req, _ := http.NewRequest("GET", base+"/wp-json/wp/v2/users/me", nil)
		req.SetBasicAuth("admin", appPass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := basic(); code != http.StatusOK {
		t.Fatalf("control: Basic auth: %d %s", code, body)
	}
	asCreateUser(t, srv, "frank", "subscriber", "frank-secret-pw")
	if r := hgCall(t, base, "POST", "/auth/local", "", map[string]any{"identifier": "frank", "password": "frank-secret-pw"}); r.Code != http.StatusOK {
		t.Fatalf("control: headless login: %d %s", r.Code, r.Raw)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET meta = meta || '{"adminsec_force_password": true}' WHERE login IN ('admin','frank')`); err != nil {
		t.Fatal(err)
	}
	if code, body := basic(); code != http.StatusForbidden || !strings.Contains(body, "adminsec_account_action_required") {
		t.Errorf("Basic auth of a blocked account: %d %s", code, body)
	}
	if r := hgCall(t, base, "POST", "/auth/local", "", map[string]any{"identifier": "frank", "password": "frank-secret-pw"}); r.Code != http.StatusForbidden || r.Body["jwt"] != nil {
		t.Errorf("headless login of a blocked account: %d %s", r.Code, r.Raw)
	}

	// The profile screen stays open to change the password, but mints no Application Password.
	_, page, _ := get(t, c, base+"/wp-admin/profile")
	form := url.Values{"_wpnonce": {formNonce(t, page)}, "do_new_application_password": {"1"}, "new_application_password_name": {"gate"}}
	if code, body, _ := post(t, c, base+"/wp-admin/profile", form); code != http.StatusForbidden {
		t.Errorf("application password at the gate: %d %.200s", code, body)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM app_passwords WHERE name = 'gate'`).Scan(&n)
	if n != 0 {
		t.Errorf("application password created at the gate")
	}
}

// Installing a theme with activate:true needs switch_themes as well as install_themes.
func TestFixsecThemeInstallActivateNeedsSwitchThemes(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	asCreateUser(t, srv, "ed", "editor", "ed-secret-pw-123")
	u, err := srv.store.UserByLoginOrEmail(context.Background(), "ed")
	if err != nil || u.Can("switch_themes", nil) {
		t.Fatalf("editor: %v", err)
	}
	// The handler itself (the route's guard checks install_themes): the check runs before the package is read.
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodPost, "/wp-json/pursae/v1/themes", nil)
	gc.Set(restUserKey, u)
	srv.pursaeThemeInstall(gc, &restRequest{Body: map[string]any{"activate": true, "package": "AAAA"}, Params: map[string]string{}})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "rest_cannot_switch_themes") {
		t.Errorf("install + activate without switch_themes: %d %s", rec.Code, rec.Body.String())
	}
}

// Headless: create alone stores a draft; ?status=published needs update (like /publish).
// A write reply of a caller without draft access shows no merged draft values of components.
func TestFixsecHeadlessCreateCannotPublish(t *testing.T) {
	base, _, srv := hgSite(t)
	hgModel(t, srv)
	creator := hgNewToken(t, srv, &hgToken{Name: "Creator", Kind: "custom", Permissions: map[string][]string{"category": {"create", "find", "findOne"}}})
	if r := hgCall(t, base, "POST", "/categories?status=published", creator, map[string]any{"data": map[string]any{"name": "Instant"}}); r.Code != http.StatusForbidden {
		t.Errorf("create-only token published: %d %s", r.Code, r.Raw)
	}
	if r := hgCall(t, base, "POST", "/categories", creator, map[string]any{"data": map[string]any{"name": "Draft"}}); r.Code != http.StatusCreated {
		t.Errorf("create-only token draft: %d %s", r.Code, r.Raw)
	}
	pub := hgNewToken(t, srv, &hgToken{Name: "Publisher", Kind: "custom", Permissions: map[string][]string{"category": {"create", "update", "find", "findOne"}}})
	if r := hgCall(t, base, "POST", "/categories?status=published", pub, map[string]any{"data": map[string]any{"name": "Live"}}); r.Code != http.StatusCreated {
		t.Errorf("create+update token publish: %d %s", r.Code, r.Raw)
	}

	full := hgNewToken(t, srv, &hgToken{Name: "Full", Kind: "full-access"})
	r := hgCall(t, base, "POST", "/articles", full, map[string]any{"data": map[string]any{"title": "Draft one",
		"seo": map[string]any{"metaTitle": "Public", "metaDescription": "UNPUBLISHED-SECRET"}}})
	doc, _ := r.data()["documentId"].(string)
	if doc == "" {
		t.Fatalf("create article: %d %s", r.Code, r.Raw)
	}
	upd := hgNewToken(t, srv, &hgToken{Name: "Updater", Kind: "custom", Permissions: map[string][]string{"article": {"update", "find", "findOne"}}})
	r = hgCall(t, base, "PUT", "/articles/"+doc+"?populate=*", upd, map[string]any{"data": map[string]any{"seo": map[string]any{"metaTitle": "New"}}})
	if r.Code != http.StatusOK || strings.Contains(r.Raw, "UNPUBLISHED-SECRET") {
		t.Errorf("write reply echoes merged draft keys: %d %s", r.Code, r.Raw)
	}
}

// With user enumeration blocked, an anonymous wp:user relation names only authors of published content.
func TestFixsecHeadlessAnonymousUsersHidden(t *testing.T) {
	_, _, srv := hgSite(t)
	ctx := context.Background()
	if !srv.blockEnumeration() {
		t.Skip("user enumeration is not blocked by default here")
	}
	id := asCreateUser(t, srv, "quiet", "subscriber", "quiet-secret-pw")
	anon := &hgAuth{Kind: "public"}
	if !srv.hgHidesUsers(anon) || srv.hgHidesUsers(&hgAuth{Kind: "token"}) {
		t.Fatal("hgHidesUsers")
	}
	if srv.hgUserPublished(ctx, id) {
		t.Error("an account without content counts as a public author")
	}
}

// REST: NUL and invalid UTF-8 bodies are 400s; pgx encode errors are masked; overflow is a 400.
func TestFixsecRESTErrorHygiene(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{{`{"a":"x\u0000y"}`, true}, {`{"a":"x\\u0000y"}`, false}, {"{\"a\":\"\x00\"}", true}, {`{"a":"ok"}`, false}} {
		if got := restJSONHasNUL([]byte(tc.body)); got != tc.want {
			t.Errorf("restJSONHasNUL(%q) = %v", tc.body, got)
		}
	}
	msg := "failed to encode args[7]: unable to encode 9223372036854775807 into binary format for int4 (OID 23): 9223372036854775807 is greater than maximum value for int4"
	if !restInternalError("woocommerce_rest_cannot_edit", msg) || !restOutOfRange(msg) {
		t.Errorf("pgx encode error not masked as out of range")
	}
	ts, c, _ := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	rc := newRESTClient(t, c, ts.URL)
	for _, b := range []string{`{"title":"a\u0000b"}`, "{\"title\":\"\xff\xfe\"}"} {
		req, _ := http.NewRequest("POST", ts.URL+"/wp-json/wp/v2/posts", strings.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-WP-Nonce", rc.nonce)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: %d", b, resp.StatusCode)
		}
	}
}

// Alert webhook URLs are credentials: masked on output, kept when the masked list is sent back.
func TestFixsecWebhookURLsMasked(t *testing.T) {
	v := map[string]any{"alert_slack_webhook": map[string]any{"enabled": true, "urls": []any{"https://hooks.slack.test/T/B/SECRET"}}, "site_url": "https://x.test"}
	out := maskSecrets(v, "").(map[string]any)
	w := out["alert_slack_webhook"].(map[string]any)
	if l := w["urls"].([]any); l[0] != secretMask {
		t.Errorf("webhook url not masked: %v", w)
	}
	if out["site_url"] != "https://x.test" {
		t.Errorf("a plain url was masked: %v", out["site_url"])
	}
	back := dropMasked(map[string]any{"alert_slack_webhook": map[string]any{"enabled": false, "urls": []any{secretMask}}}).(map[string]any)
	if _, ok := back["alert_slack_webhook"].(map[string]any)["urls"]; ok {
		t.Errorf("masked urls not dropped: %v", back)
	}
}
