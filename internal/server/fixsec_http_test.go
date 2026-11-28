package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Reserved first segments are matched whole, and anything below them is never the theme's.
func TestFixsecHTTPReservedPaths(t *testing.T) {
	for _, p := range []string{"wp-content", "wp-content/x", "wp-includes", "wp-admin", "wp-adminx", "wp-json", "wp-login.php",
		"wp-loginx", "xmlrpc", "xmlrpcs", "wp-cron.php", "wp-cronjob", "oauth", "oauth/x", ".well-known/x", "WP-CONTENT"} {
		if !siteReservedPath(p) {
			t.Errorf("%q should be reserved", p)
		}
	}
	for _, p := range []string{"", "hello-world", "wp-sitemap.xml", "author/admin", "oauthx", "my-wp-content"} {
		if siteReservedPath(p) {
			t.Errorf("%q should not be reserved", p)
		}
	}
	open := func(u string) bool { return hgAlwaysOpen(httptest.NewRequest(http.MethodGet, u, nil)) }
	for _, u := range []string{"/wp-admin", "/wp-admin/edit", "/wp-login", "/wp-login.php", "/wp-json/wp/v2/posts", "/xmlrpc.php",
		"/wp-cron.php", "/oauth/authorize", "/.well-known/oauth-authorization-server", "/wp-content/uploads/a.png", "/?rest_route=/"} {
		if !open(u) {
			t.Errorf("headless: %s should stay open", u)
		}
	}
	for _, u := range []string{"/wp-loginx?p=1", "/xmlrpcs?s=a", "/wp-cronjob", "/wp-adminx", "/wp-jsonx", "/oauthx"} {
		if open(u) {
			t.Errorf("headless: %s must not be always open", u)
		}
	}
}

// Search excludes "-term" posts; site and REST searches agree.
func TestFixsecHTTPSearchExclusion(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	now := time.Now()
	mustInsertPost(t, srv, &store.Post{Type: "post", Title: "Zebra alpha", Slug: "zebra-alpha", Content: "<p>zebra</p>", Status: "publish", PublishedAt: &now})
	mustInsertPost(t, srv, &store.Post{Type: "post", Title: "Zebra beta", Slug: "zebra-beta", Content: "<p>zebra quokka</p>", Status: "publish", PublishedAt: &now})
	posts, _, err := srv.store.QueryPosts(context.Background(), store.PostQuery{Types: []string{"post"}, Statuses: []string{"publish"}, Search: "zebra -quokka"})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].Slug != "zebra-alpha" {
		t.Errorf("exclusion: got %d posts", len(posts))
	}
	// A huge anonymous search is truncated, not an error.
	r := anonGet(t, ts.URL+"/wp-json/wp/v2/search?search="+url.QueryEscape(strings.Repeat("zebra x ", 500)))
	if r.StatusCode != http.StatusOK {
		t.Errorf("long search: %d", r.StatusCode)
	}
}

// Maintenance: /wp-content/ with ?p= / ?s= must not render content (it is a bare 404 now).
func TestFixsecHTTPMaintenanceWPContentBypass(t *testing.T) {
	ts, _, srv := maintenanceSite(t)
	now := time.Now()
	p := mustInsertPost(t, srv, &store.Post{Type: "post", Title: "FixsecSecretTitle", Slug: "fixsec-secret", Content: "<p>FixsecSecretBody</p>",
		Status: "publish", PublishedAt: &now})
	if err := srv.maintSetMode(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(p.ID, 10)
	for _, u := range []string{"/wp-content/?p=" + id + "&preview=true", "/wp-content/?s=FixsecSecret", "/wp-includes/?p=" + id,
		"/wp-loginx?p=" + id, "/oauth/x?p=" + id} {
		r := anonGet(t, ts.URL+u)
		if strings.Contains(bodyOf(r), "FixsecSecret") || r.StatusCode == http.StatusOK {
			t.Errorf("%s: %d, content served", u, r.StatusCode)
		}
	}
}

// With enumeration blocked, an author without published posts answers like an unknown slug; ?attachment_id=
// redirects only for a public attachment.
func TestFixsecHTTPAuthorAndAttachment(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	if _, err := srv.store.CreateUser(ctx, store.NewUser{Login: "ghost", Email: "ghost@example.test", PasswordHash: "x", Role: "author"}); err != nil {
		t.Fatal(err)
	}
	if !srv.blockEnumeration() {
		t.Skip("enumeration blocking off in this environment")
	}
	if r := anonGet(t, ts.URL+"/author/ghost/"); r.StatusCode != http.StatusNotFound {
		t.Errorf("/author/ghost/: %d", r.StatusCode)
	}
	if r := anonGet(t, ts.URL+"/author/admin/"); r.StatusCode != http.StatusOK {
		t.Errorf("/author/admin/ (has posts): %d", r.StatusCode)
	}

	draft := mustInsertPost(t, srv, &store.Post{Type: "post", Title: "Draft parent", Slug: "draft-parent", Status: "draft"})
	priv := &store.Attachment{UploaderID: 1, PostID: draft.ID, Slug: "secret-scan", FilePath: "2026/11/secret-scan.pdf", Mime: "application/pdf", Title: "s"}
	pub := &store.Attachment{UploaderID: 1, Slug: "open-logo", FilePath: "2026/11/open-logo.png", Mime: "image/png", Title: "o"}
	for _, a := range []*store.Attachment{priv, pub} {
		if err := srv.store.InsertAttachment(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if r := anonGet(t, ts.URL+"/?attachment_id="+strconv.FormatInt(priv.ID, 10)); r.StatusCode != http.StatusNotFound || strings.Contains(r.Header.Get("Location"), "secret-scan") {
		t.Errorf("private attachment: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := anonGet(t, ts.URL+"/?attachment_id="+strconv.FormatInt(pub.ID, 10)); r.StatusCode != http.StatusMovedPermanently {
		t.Errorf("public attachment: %d", r.StatusCode)
	}
}

// Static assets keep their cacheability for signed-in users; exports are private.
func TestFixsecHTTPCacheHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		url     string
		noStore bool
	}{
		{"/wp-content/themes/x/style.css?ver=1", false},
		{"/wp-includes/js/a.js", false},
		{"/wp-content/?s=a", true},
		{"/hello-world/", true},
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, tc.url, nil)
		c.Set(ctxUser, &store.User{ID: 1})
		w := newNoStoreWriter(c)
		_, _ = w.Write([]byte("x"))
		if got := rec.Header().Get("Cache-Control") == noStoreCacheControl; got != tc.noStore {
			t.Errorf("%s: no-store=%v, want %v", tc.url, got, tc.noStore)
		}
	}
	_, _, srv := testServerBuilderOff(t)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/wp-content/uploads/wp-personal-data-exports/wp-personal-data-file-abc.zip", nil)
	if cc := srv.uploadCacheControl(c); cc != "private, no-store" {
		t.Errorf("export Cache-Control %q", cc)
	}
}

// Hide-login: visitor-open wp-admin endpoints never redirect to the secret slug.
func TestFixsecHTTPHideLoginLeaks(t *testing.T) {
	base, _, srv := asStart(t)
	asConfigure(t, srv, func(st *asSettings) { st.HideLogin, st.LoginSlug = true, "door-777" })
	for _, p := range []string{"/wp-admin/admin-post.php?action=x", "/wp-admin/admin-post", "/wp-admin/admin-ajax.php", "/wp-admin/install"} {
		code, body, loc := get(t, asNewClient(), base+p)
		if strings.Contains(loc, "door-777") || strings.Contains(body, "door-777") {
			t.Errorf("%s leaks the login slug: %d %s", p, code, loc)
		}
	}
}
