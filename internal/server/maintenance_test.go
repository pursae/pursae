package server

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"wpgo/internal/auth"
	"wpgo/internal/store"
)

func TestMaintSafeCSSStripsCloseTags(t *testing.T) {
	for _, in := range []string{"</style><script>", "</STYLE>", "< /script>", "</ScRiPt>", "a{color:red}</style >", "</style"} {
		if got := maintSafeCSS(in); strings.Contains(strings.ToLower(got), "</") {
			t.Errorf("maintSafeCSS(%q) = %q", in, got)
		}
	}
	if got := maintSafeCSS("a{color:red}"); !strings.Contains(got, "color") {
		t.Fatalf("ordinary CSS removed: %q", got)
	}
}

func TestMaintIPAllowed(t *testing.T) {
	list := "203.0.113.7\n10.0.0.0/8 # office\n\n2001:db8::/32, 192.0.2.1"
	for ip, want := range map[string]bool{"203.0.113.7": true, "10.20.30.40": true, "2001:db8::1": true, "192.0.2.1": true,
		"203.0.113.8": false, "11.0.0.1": false, "": false, "garbage": false} {
		if got := maintIPAllowed(list, ip); got != want {
			t.Errorf("maintIPAllowed(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestMaintURLRules(t *testing.T) {
	s := &Server{}
	wl := maintSettings{URLRules: "whitelist", URLList: "/contact/\nhttps://example.test/About\n/shop/*\n/feed.xml"}
	for p, gated := range map[string]bool{"/": true, "/contact": false, "/contact/": false, "/CONTACT/": false, "/about/": false,
		"/shop/": false, "/shop/item/1": false, "/feed.xml": false, "/contact/form/": true, "/blog/": true} {
		if got := s.maintURLGated(wl, p); got != gated {
			t.Errorf("whitelist %q gated = %v, want %v", p, got, gated)
		}
	}
	bl := maintSettings{URLRules: "blacklist", URLList: "/private/"}
	if !s.maintURLGated(bl, "/private") || s.maintURLGated(bl, "/") {
		t.Error("blacklist mode closes the wrong URLs")
	}
	if !s.maintURLGated(maintSettings{URLList: "/"}, "/") {
		t.Error("the URL list is used while URL Based Rules are disabled")
	}
	for in, want := range map[string]string{"": "/", "a": "/a/", "/a/": "/a/", "/x.php": "/x.php", "/a/b": "/a/b/"} {
		if got := maintSlashit(in); got != want {
			t.Errorf("maintSlashit(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaintSecretKeyAndColors(t *testing.T) {
	if k := maintSecretKey(`pre view<script>"-site_1`); k != "previewscript-site_1" {
		t.Errorf("secret key %q", k)
	}
	if maintSecretKey(maintPreviewParam) != "" {
		t.Error("the preview parameter is accepted as a secret key")
	}
	if maintColor("red;}body{x", "#000") != "#000" || maintColor("#AbCdEf", "#000") != "#AbCdEf" {
		t.Error("maintColor")
	}
	if maintCSSURL(`/a.png");}`) != "" || maintCSSURL("javascript:x") != "" || maintCSSURL("/wp-content/uploads/a.jpg") == "" {
		t.Error("maintCSSURL")
	}
}

// maintenanceSite installs a site, logs the admin in and activates Maintenance-Go.
func maintenanceSite(t *testing.T) (*httptest.Server, *http.Client, *Server) {
	t.Helper()
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	_, body, _ := get(t, c, ts.URL+"/wp-admin/plugins")
	m := regexp.MustCompile(`href="(/wp-admin/plugins\?action=activate&amp;plugin=maintenance[^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no Activate link for Pursae Maintenance on the Plugins screen")
	}
	if code, _, loc := get(t, c, ts.URL+html.UnescapeString(m[1])); code != http.StatusFound || !activationOK(loc) {
		t.Fatalf("activate Pursae Maintenance: %d %s", code, loc)
	}
	if !srv.maintOn() {
		t.Fatal("Pursae Maintenance is not active after activation")
	}
	return ts, c, srv
}

// maintSaveTab posts one settings tab through the admin screen.
func maintSaveTab(t *testing.T, c *http.Client, base, tab string, form url.Values) {
	t.Helper()
	_, body, _ := get(t, c, base+"/wp-admin/admin?page=maintenance&tab="+tab)
	form.Set("_wpnonce", formNonce(t, body))
	form.Set("tab", tab)
	if code, _, loc := post(t, c, base+"/wp-admin/admin?page=maintenance", form); code != http.StatusFound || strings.Contains(loc, "mtnc-error") {
		t.Fatalf("save %s: %d %s", tab, code, loc)
	}
}

func maintEdit(t *testing.T, srv *Server, f func(*maintSettings)) {
	t.Helper()
	st := srv.maintLoad()
	f(&st)
	if err := srv.maintSave(context.Background(), st); err != nil {
		t.Fatal(err)
	}
}

func anonGet(t *testing.T, u string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(strings.NewReader(string(b)))
	return resp
}

func bodyOf(r *http.Response) string { b, _ := io.ReadAll(r.Body); return string(b) }

func TestMaintenanceOffByDefault(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	if srv.maintOn() {
		t.Fatal("Pursae Maintenance is on after a fresh install")
	}
	_, body, _ := get(t, c, base+"/wp-admin/plugins")
	if row := regexp.MustCompile(`(?s)<tr class="(\w+)" data-slug="maintenance"`).FindStringSubmatch(body); row == nil || row[1] != "inactive" {
		t.Fatalf("Pursae Maintenance is not listed as an inactive plugin: %v", row)
	}
	// Settings saying "on" change nothing while the plugin is off.
	maintEdit(t, srv, func(st *maintSettings) { st.Enabled = true })
	if r := anonGet(t, base+"/"); r.StatusCode != http.StatusOK || strings.Contains(bodyOf(r), "mtnc") {
		t.Fatalf("inactive plugin affects the front page: %d", r.StatusCode)
	}
	_, body, _ = get(t, c, base+"/wp-admin/")
	if strings.Contains(body, "toplevel_page_maintenance") || strings.Contains(body, "wp-admin-bar-maintenance") {
		t.Error("an inactive Pursae Maintenance shows in the admin menu or toolbar")
	}
	if code, _, _ := get(t, c, base+"/wp-admin/admin?page=maintenance"); code != http.StatusForbidden {
		t.Errorf("settings screen of an inactive plugin: %d", code)
	}
	_, body, _ = get(t, c, base+"/wp-admin/plugin-install?tab=native")
	if !strings.Contains(body, "Pursae Maintenance") {
		t.Error("Pursae Maintenance is missing from the built-in plugins tab")
	}
}

func TestMaintenanceGate(t *testing.T) {
	ts, c, srv := maintenanceSite(t)
	base := ts.URL

	// Active, mode off: nothing changes; the screen, menu and toolbar node are there.
	if r := anonGet(t, base+"/"); r.StatusCode != http.StatusOK {
		t.Fatalf("mode off: %d", r.StatusCode)
	}
	_, body, _ := get(t, c, base+"/wp-admin/")
	if !strings.Contains(body, "toplevel_page_maintenance") || !strings.Contains(body, "wp-admin-bar-maintenance") {
		t.Fatal("no Maintenance menu or toolbar node")
	}
	for _, tab := range maintTabs {
		if code, body, _ := get(t, c, base+"/wp-admin/admin?page=maintenance&tab="+tab.Key); code != http.StatusOK || !strings.Contains(body, `nav-tab nav-tab-active">`+tab.Label) {
			t.Fatalf("tab %s: %d", tab.Key, code)
		}
	}

	// The toolbar switch turns the mode on.
	m := regexp.MustCompile(`id="wp-admin-bar-maintenance-toggle"><a class="ab-item" role="menuitem" href="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no toolbar toggle")
	}
	if code, _, _ := get(t, c, base+html.UnescapeString(m[1])); code != http.StatusFound || !srv.maintLoad().Enabled {
		t.Fatalf("toggle: %d, enabled %v", code, srv.maintLoad().Enabled)
	}
	maintSaveTab(t, c, base, "general", url.Values{"status": {"1"}, "mode": {"maintenance"}, "header_text": {"Back at noon"},
		"content": {"<p>Upgrading %sitetitle%</p>"}, "footer_text": {"Footer %year%"}, "logo": {"javascript:alert(1)"}})

	// Visitors: 503 with Retry-After, no caching, noindex.
	r := anonGet(t, base+"/")
	b := bodyOf(r)
	if r.StatusCode != http.StatusServiceUnavailable || r.Header.Get("Retry-After") != "3600" {
		t.Fatalf("visitor: %d Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	for _, want := range []string{"Back at noon", "Upgrading Toggle Site", "Footer " + strconv.Itoa(time.Now().Year()), `name="robots" content="noindex`, `action="/wp-login"`} {
		if !strings.Contains(b, want) {
			t.Errorf("maintenance page misses %q", want)
		}
	}
	if strings.Contains(b, "javascript:") {
		t.Error("a javascript: logo URL was saved")
	}
	if cc := r.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") || r.Header.Get("X-Robots-Tag") == "" {
		t.Errorf("headers: Cache-Control %q, X-Robots-Tag %q", cc, r.Header.Get("X-Robots-Tag"))
	}
	if r := anonGet(t, base+"/?p=1"); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a post: %d", r.StatusCode)
	}
	if r := anonGet(t, base+"/feed/"); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("feed: %d", r.StatusCode)
	}
	if r := anonGet(t, base+"/wp-json/wp/v2/posts"); r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(bodyOf(r), "maintenance_mode") {
		t.Errorf("REST: %d", r.StatusCode)
	}
	if code, _ := restGet(t, c, base, "/wp-json/wp/v2/posts"); code != http.StatusOK {
		t.Errorf("REST for the admin: %d", code)
	}
	// Per-request credentials reach the REST layer (which refuses bad ones itself); an unknown scheme does not.
	for auth, want := range map[string]int{"Basic YWRtaW46d3Jvbmc=": http.StatusUnauthorized, "Bearer not-a-token": http.StatusUnauthorized, "Token x": http.StatusServiceUnavailable,
		"Basic x": http.StatusServiceUnavailable, "Basic bm9jb2xvbg==": http.StatusServiceUnavailable} { // undecodable / no colon: anonymous for REST

		req, _ := http.NewRequest("GET", base+"/wp-json/wp/v2/posts", nil)
		req.Header.Set("Authorization", auth)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Errorf("REST with Authorization %q: %d, want %d", auth, r.StatusCode, want)
		}
	}
	if !maintRESTCredentials(httptest.NewRequest("GET", "/wp-json/wc/v3/orders?consumer_key=ck_x&consumer_secret=cs_y", nil)) ||
		!maintRESTCredentials(httptest.NewRequest("GET", "/?rest_route=/wc/v3/orders&consumer_key=ck_x", nil)) ||
		maintRESTCredentials(httptest.NewRequest("GET", "/wp-json/wp/v2/posts?consumer_key=ck_x", nil)) ||
		maintRESTCredentials(httptest.NewRequest("GET", "/wp-json/wc/store/v1/products?consumer_key=x", nil)) ||
		maintRESTCredentials(httptest.NewRequest("POST", "/wp-json/wc/store/v1/checkout?consumer_key=x", nil)) {
		t.Error("consumer_key credentials: only on wc/v3 routes, which validate them")
	}
	// The public Store API ignores query keys: a fake one must not open it during maintenance.
	if r := anonGet(t, base+"/wp-json/wc/store/v1/products?consumer_key=x"); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Store API with a fake consumer_key: %d, want 503", r.StatusCode)
	}
	// Always reachable: the login screen, wp-admin (its login redirect), assets, robots.txt.
	if r := anonGet(t, base+"/wp-login"); r.StatusCode != http.StatusOK {
		t.Errorf("wp-login: %d", r.StatusCode)
	}
	if r := anonGet(t, base+"/wp-login.php"); r.StatusCode != http.StatusMovedPermanently {
		t.Errorf("wp-login.php: %d", r.StatusCode)
	}
	if r := anonGet(t, base+"/wp-admin/"); r.StatusCode != http.StatusFound || !strings.HasPrefix(r.Header.Get("Location"), "/wp-login") {
		t.Errorf("wp-admin: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := anonGet(t, base+"/wp-includes/css/dashicons.min.css"); r.StatusCode != http.StatusOK {
		t.Errorf("assets: %d", r.StatusCode)
	}
	if r := anonGet(t, base+"/robots.txt"); r.StatusCode != http.StatusOK {
		t.Errorf("robots.txt: %d", r.StatusCode)
	}

	// The administrator sees the site.
	if code, body, _ := get(t, c, base+"/"); code != http.StatusOK || strings.Contains(body, "mtnc-main") {
		t.Fatalf("admin: %d", code)
	}

	// Logged-in users by role.
	ctx := context.Background()
	hash, _ := auth.HashPassword("sub password")
	if _, err := srv.store.CreateUser(ctx, store.NewUser{Login: "sub", Email: "sub@example.test", PasswordHash: hash, Role: "subscriber"}); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	sub := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if code, _, _ := post(t, sub, base+"/wp-login", url.Values{"log": {"sub"}, "pwd": {"sub password"}}); code != http.StatusFound {
		t.Fatalf("subscriber login: %d", code)
	}
	if code, _, _ := get(t, sub, base+"/"); code != http.StatusOK {
		t.Errorf("subscriber with the default roles: %d", code)
	}
	maintSaveTab(t, c, base, "access", url.Values{"show_logged_in": {"1"}, "roles[]": {"editor", "author"}, "login_button": {"1"}})
	if code, _, _ := get(t, sub, base+"/"); code != http.StatusServiceUnavailable {
		t.Errorf("subscriber not in the roles: %d", code)
	}
	// block_rest uses the same role bypass as the HTML gate: a logged-in subscriber who
	// cannot see the site cannot use the REST API either. Administrators still can.
	if code, _ := restGet(t, sub, base, "/wp-json/wp/v2/posts"); code != http.StatusServiceUnavailable {
		t.Errorf("subscriber REST while block_rest: %d", code)
	}
	if code, _ := restGet(t, c, base, "/wp-json/wp/v2/posts"); code != http.StatusOK {
		t.Errorf("admin REST after the role change: %d", code)
	}
	if code, _, _ := get(t, sub, base+"/wp-admin/profile"); code != http.StatusFound { // front-end member: no wp-admin
		t.Errorf("subscriber's profile in wp-admin: %d", code)
	}
	if code, _, _ := get(t, c, base+"/"); code != http.StatusOK {
		t.Errorf("administrators always pass: %d", code)
	}

	// IP whitelist (the test client connects from 127.0.0.1).
	maintEdit(t, srv, func(st *maintSettings) { st.IPWhitelist = "127.0.0.0/8" })
	if r := anonGet(t, base+"/"); r.StatusCode != http.StatusOK {
		t.Errorf("whitelisted IP: %d", r.StatusCode)
	}
	maintEdit(t, srv, func(st *maintSettings) { st.IPWhitelist = "203.0.113.7" })
	if r := anonGet(t, base+"/"); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("IP not on the list: %d", r.StatusCode)
	}

	// Secret access link: ?<key> sets a cookie; a new key locks it out.
	maintEdit(t, srv, func(st *maintSettings) { st.SecretKey = "let-me-in" })
	r = anonGet(t, base+"/?let-me-in")
	var access *http.Cookie
	for _, ck := range r.Cookies() {
		if ck.Name == maintAccessCookie {
			access = ck
		}
	}
	if r.StatusCode != http.StatusOK || access == nil || access.MaxAge != 24*3600 {
		t.Fatalf("secret link: %d, cookie %v", r.StatusCode, access)
	}
	if r := anonGet(t, base+"/?p=1", access); r.StatusCode == http.StatusServiceUnavailable {
		t.Errorf("access cookie: %d", r.StatusCode)
	}
	if r := anonGet(t, base+"/?wrong-key"); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("wrong key: %d", r.StatusCode)
	}
	maintEdit(t, srv, func(st *maintSettings) { st.SecretKey = "new-key" })
	if r := anonGet(t, base+"/", access); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("old access cookie after a key change: %d", r.StatusCode)
	}

	// Site password form.
	maintEdit(t, srv, func(st *maintSettings) { st.SitePassword, st.PasswordButton = "open sesame", true })
	b = bodyOf(anonGet(t, base+"/"))
	nonce := regexp.MustCompile(`name="_wpnonce" value="([^"]+)"`).FindStringSubmatch(b)
	if nonce == nil || !strings.Contains(b, `action="/wp-admin/admin-post?action=mtnc_unlock"`) {
		t.Fatal("no password form")
	}
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := anon.PostForm(base+"/wp-admin/admin-post?action=mtnc_unlock", url.Values{"_wpnonce": {nonce[1]}, "password": {"nope"}, "_wp_http_referer": {"/"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "mtnc_unlock=wrong") {
		t.Errorf("wrong password: %s", resp.Header.Get("Location"))
	}
	resp, err = anon.PostForm(base+"/wp-admin/admin-post?action=mtnc_unlock", url.Values{"_wpnonce": {nonce[1]}, "password": {"open sesame"}, "_wp_http_referer": {"/"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var unlock *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == maintUnlockCookie {
			unlock = ck
		}
	}
	if unlock == nil {
		t.Fatal("no unlock cookie")
	}
	if r := anonGet(t, base+"/", unlock); r.StatusCode != http.StatusOK {
		t.Errorf("unlocked visitor: %d", r.StatusCode)
	}

	// URL Based Rules.
	maintSaveTab(t, c, base, "access", url.Values{"show_logged_in": {"1"}, "per_url_settings": {"whitelist"}, "per_url_enable_disable": {"/hello-world/"}})
	if r := anonGet(t, base+"/hello-world/"); r.StatusCode == http.StatusServiceUnavailable {
		t.Errorf("whitelisted URL: %d", r.StatusCode)
	}
	if r := anonGet(t, base+"/"); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("other URL with a whitelist: %d", r.StatusCode)
	}
	maintSaveTab(t, c, base, "access", url.Values{"per_url_settings": {"blacklist"}, "per_url_enable_disable": {"/hello-world/"}})
	if r := anonGet(t, base+"/"); r.StatusCode != http.StatusOK {
		t.Errorf("URL off the blacklist: %d", r.StatusCode)
	}
	if r := anonGet(t, base+"/hello-world/"); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("blacklisted URL: %d", r.StatusCode)
	}
	maintSaveTab(t, c, base, "access", url.Values{"show_logged_in": {"1"}})

	// Search engines, when allowed.
	maintEdit(t, srv, func(st *maintSettings) { st.ExcludeSE = true })
	req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")
	if resp, err := anon.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("Googlebot with excludese: %v %v", resp, err)
	}

	// Coming Soon: 200, still not cached and noindex.
	maintSaveTab(t, c, base, "general", url.Values{"status": {"1"}, "mode": {"coming_soon"}, "header_text": {"Launching soon"}})
	r = anonGet(t, base+"/")
	if b := bodyOf(r); r.StatusCode != http.StatusOK || !strings.Contains(b, "Launching soon") || r.Header.Get("Retry-After") != "" {
		t.Errorf("coming soon: %d Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}

	// Preview works while the mode is off; the Themes tab previews other themes.
	maintSaveTab(t, c, base, "general", url.Values{"mode": {"maintenance"}})
	if r := anonGet(t, base+"/"); r.StatusCode != http.StatusOK || strings.Contains(bodyOf(r), "mtnc-main") {
		t.Fatalf("mode off: %d", r.StatusCode)
	}
	_, pv, _ := get(t, c, base+"/?maintenance-preview&theme=neon")
	if !strings.Contains(pv, "theme-neon") || !strings.Contains(pv, "maintenance mode is off") {
		t.Error("preview")
	}

	// Deactivating the plugin opens the site even with the mode on.
	maintEdit(t, srv, func(st *maintSettings) { st.Enabled = true })
	_, body, _ = get(t, c, base+"/wp-admin/plugins")
	m = regexp.MustCompile(`href="(/wp-admin/plugins\?action=deactivate&amp;plugin=maintenance[^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no Deactivate link")
	}
	get(t, c, base+html.UnescapeString(m[1]))
	if r := anonGet(t, base+"/"); r.StatusCode != http.StatusOK {
		t.Errorf("deactivated plugin: %d", r.StatusCode)
	}
}

func TestMaintenanceCountdown(t *testing.T) {
	ts, _, srv := maintenanceSite(t)
	base := ts.URL
	future := time.Now().In(srv.loc()).Add(2 * time.Hour).Format("2006-01-02T15:04")
	maintEdit(t, srv, func(st *maintSettings) {
		st.Enabled, st.Countdown, st.CountdownEnd, st.AutoDisable = true, true, future, true
	})
	r := anonGet(t, base+"/")
	ra, _ := strconv.Atoi(r.Header.Get("Retry-After"))
	if r.StatusCode != http.StatusServiceUnavailable || ra < 6600 || ra > 7200 || !strings.Contains(bodyOf(r), `id="mtnc-countdown"`) {
		t.Fatalf("countdown running: %d Retry-After %d", r.StatusCode, ra)
	}
	// The end passes: the first request switches maintenance off and purges the cache.
	past := time.Now().In(srv.loc()).Add(-time.Minute).Format("2006-01-02T15:04")
	maintEdit(t, srv, func(st *maintSettings) { st.CountdownEnd, st.AutoDisable = past, false })
	if r := anonGet(t, base+"/"); r.StatusCode != http.StatusServiceUnavailable || r.Header.Get("Retry-After") != "3600" {
		t.Errorf("ended countdown without auto-disable: %d %s", r.StatusCode, r.Header.Get("Retry-After"))
	}
	maintEdit(t, srv, func(st *maintSettings) { st.AutoDisable = true })
	if r := anonGet(t, base+"/"); r.StatusCode != http.StatusOK {
		t.Errorf("ended countdown with auto-disable: %d", r.StatusCode)
	}
	if srv.maintLoad().Enabled {
		t.Error("maintenance mode is still on after the countdown")
	}
}

func TestMaintenancePageCache(t *testing.T) {
	ts, c, srv := maintenanceSite(t)
	base := ts.URL
	if !srv.settings.Bool("wpgo_cache_enabled") {
		t.Skip("Pursae Cache is off by default")
	}
	anonGet(t, base+"/")
	if r := anonGet(t, base+"/"); r.Header.Get("X-WPGo-Cache") != "hit" {
		t.Fatalf("the front page is not cached: %q", r.Header.Get("X-WPGo-Cache"))
	}
	// Turning maintenance on: visitors never get the cached page, nor a cached maintenance page.
	maintSaveTab(t, c, base, "general", url.Values{"status": {"1"}, "mode": {"coming_soon"}, "header_text": {"Closed"}})
	for i := 0; i < 2; i++ {
		r := anonGet(t, base+"/")
		if !strings.Contains(bodyOf(r), "Closed") || r.Header.Get("X-WPGo-Cache") != "" {
			t.Fatalf("closed site, request %d: %d cache %q", i, r.StatusCode, r.Header.Get("X-WPGo-Cache"))
		}
	}
	// Bypassing users get the real site, never the maintenance page from the cache.
	if _, body, _ := get(t, c, base+"/"); strings.Contains(body, "mtnc-main") {
		t.Error("the administrator gets the maintenance page")
	}
	maintEdit(t, srv, func(st *maintSettings) { st.SecretKey = "k" })
	r := anonGet(t, base+"/?k")
	var ck *http.Cookie
	for _, x := range r.Cookies() {
		if x.Name == maintAccessCookie {
			ck = x
		}
	}
	for i := 0; i < 2; i++ {
		if b := bodyOf(anonGet(t, base+"/", ck)); strings.Contains(b, "mtnc-main") {
			t.Fatalf("secret link visitor gets the maintenance page (request %d)", i)
		}
	}
	// Turning it off purges: the next visitor gets a fresh page, not one from before.
	maintSaveTab(t, c, base, "general", url.Values{"mode": {"maintenance"}})
	r = anonGet(t, base+"/")
	if r.StatusCode != http.StatusOK || strings.Contains(bodyOf(r), "mtnc-main") {
		t.Fatalf("open site: %d", r.StatusCode)
	}
}

func TestMaintenanceSubscribeAndExport(t *testing.T) {
	ts, c, srv := maintenanceSite(t)
	base := ts.URL
	maintSaveTab(t, c, base, "modules", url.Values{"subscribe": {"1"}, "gdpr": {"1"}, "subscribe_title": {"Stay tuned"},
		"social_facebook": {"https://facebook.com/example"}, "social_twitter": {"javascript:alert(1)"}, "social_email": {"hi@example.test"}})
	maintEdit(t, srv, func(st *maintSettings) { st.Enabled = true })
	b := bodyOf(anonGet(t, base+"/"))
	if !strings.Contains(b, "Stay tuned") || !strings.Contains(b, "https://facebook.com/example") || !strings.Contains(b, "mailto:hi@example.test") || strings.Contains(b, "javascript:") {
		t.Fatal("modules missing from the page")
	}
	nonce := regexp.MustCompile(`name="_wpnonce" value="([^"]+)"`).FindStringSubmatch(b)[1]
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(form url.Values) string {
		t.Helper()
		resp, err := anon.PostForm(base+"/wp-admin/admin-post?action=mtnc_subscribe", form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	if loc := send(url.Values{"_wpnonce": {nonce}, "email": {"fan@example.test"}}); !strings.Contains(loc, "mtnc_error=consent") {
		t.Errorf("no consent: %s", loc)
	}
	if loc := send(url.Values{"_wpnonce": {nonce}, "email": {"not an email"}, "gdpr": {"1"}}); !strings.Contains(loc, "mtnc_error=email") {
		t.Errorf("bad address: %s", loc)
	}
	if loc := send(url.Values{"_wpnonce": {"forged"}, "email": {"fan@example.test"}, "gdpr": {"1"}}); !strings.Contains(loc, "mtnc_error=expired") {
		t.Errorf("forged nonce: %s", loc)
	}
	if loc := send(url.Values{"_wpnonce": {nonce}, "email": {"Fan@Example.test"}, "gdpr": {"1"}, "_wp_http_referer": {"/?p=1"}}); !strings.Contains(loc, "mtnc_subscribed=1") || !strings.HasPrefix(loc, "/?") {
		t.Errorf("subscribe: %s", loc)
	}
	send(url.Values{"_wpnonce": {nonce}, "email": {"fan@example.test"}, "gdpr": {"1"}}) // a duplicate is fine
	send(url.Values{"_wpnonce": {nonce}, "email": {"bot@example.test"}, "gdpr": {"1"}, "mtnc_website": {"spam"}})
	subs, err := srv.maintSubscribers(context.Background())
	if err != nil || len(subs) != 1 || subs[0].Email != "fan@example.test" || !subs[0].Consent {
		t.Fatalf("subscribers: %v %v", subs, err)
	}
	// A script looping over the form is slowed down per network.
	var limited bool
	for i := 0; i < 8 && !limited; i++ {
		limited = strings.Contains(send(url.Values{"_wpnonce": {nonce}, "email": {"flood" + strconv.Itoa(i) + "@example.test"}, "gdpr": {"1"}}), "mtnc_error=busy")
	}
	if !limited {
		t.Error("subscriptions are not rate limited")
	}
	_, _ = srv.store.Pool.Exec(context.Background(), `DELETE FROM maintenance_subscribers WHERE email LIKE 'flood%'`)
	_, body, _ := get(t, c, base+"/wp-admin/admin?page=maintenance&tab=subscribers")
	if !strings.Contains(body, "fan@example.test") {
		t.Error("subscriber not listed")
	}
	n := formNonce(t, body)
	_, csv, _ := get(t, c, base+"/wp-admin/admin?page=maintenance&action=export_subscribers&_wpnonce="+n)
	if !strings.HasPrefix(csv, "email,consent,subscribed\nfan@example.test,true,") {
		t.Errorf("CSV: %q", csv)
	}

	// Export, change, import: the settings come back, status and password stay.
	maintEdit(t, srv, func(st *maintSettings) { st.SitePassword = "secret1" })
	_, exported, _ := get(t, c, base+"/wp-admin/admin?page=maintenance&action=export&_wpnonce="+n)
	if !strings.Contains(exported, `"plugin": "maintenance-go"`) || strings.Contains(exported, "secret1") {
		t.Fatalf("export: %.200s", exported)
	}
	maintEdit(t, srv, func(st *maintSettings) { st.SubscribeTitle, st.Enabled = "Changed", false })
	if !maintImportFile(t, c, base, n, exported) {
		t.Fatal("import failed")
	}
	if st := srv.maintLoad(); st.SubscribeTitle != "Stay tuned" || st.Enabled || st.SitePassword != "secret1" {
		t.Errorf("after import: title %q enabled %v password %q", st.SubscribeTitle, st.Enabled, st.SitePassword)
	}
	if maintImportFile(t, c, base, n, `{"something":"else"}`) {
		t.Error("a foreign file was imported")
	}
	// Reset.
	get(t, c, base+"/wp-admin/admin?page=maintenance&action=reset&_wpnonce="+n)
	if st := srv.maintLoad(); st.SubscribeTitle != maintDefaults().SubscribeTitle {
		t.Error("reset kept the settings")
	}
}

func maintImportFile(t *testing.T, c *http.Client, base, nonce, content string) bool {
	t.Helper()
	var buf strings.Builder
	bd := "mtncboundary"
	buf.WriteString("--" + bd + "\r\nContent-Disposition: form-data; name=\"_wpnonce\"\r\n\r\n" + nonce + "\r\n")
	buf.WriteString("--" + bd + "\r\nContent-Disposition: form-data; name=\"tab\"\r\n\r\nadvanced\r\n")
	buf.WriteString("--" + bd + "\r\nContent-Disposition: form-data; name=\"import\"\r\n\r\n1\r\n")
	buf.WriteString("--" + bd + "\r\nContent-Disposition: form-data; name=\"import_file\"; filename=\"s.json\"\r\nContent-Type: application/json\r\n\r\n" + content + "\r\n--" + bd + "--\r\n")
	resp, err := c.Post(base+"/wp-admin/admin?page=maintenance", "multipart/form-data; boundary="+bd, strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return strings.Contains(resp.Header.Get("Location"), "mtnc-msg=imported")
}

func TestMaintAlwaysOpenWholeSegments(t *testing.T) {
	for _, p := range []string{"/wp-admin", "/wp-admin/", "/wp-admin/edit", "/wp-login", "/wp-login.php",
		"/wp-content/uploads/a.png", "/wp-includes/css/x.css", "/robots.txt"} {
		if !maintAlwaysOpen(http.MethodGet, p) {
			t.Errorf("%s should stay open", p)
		}
	}
	// /xmlrpc and /wp-cron must not skip the gate: the site router serves ?p= on any path.
	for _, p := range []string{"/xmlrpc", "/xmlrpc.php", "/xmlrpc.php/", "/wp-cron", "/wp-cron.php"} {
		if maintAlwaysOpen(http.MethodGet, p) || !maintRPCOrCron(p) {
			t.Errorf("%s must be answered by the maintenance gate", p)
		}
	}
	// The site router serves ?p= / ?page_id= on any unknown path: these must stay behind the gate.
	for _, p := range []string{"/wp-login-x", "/wp-loginx", "/xmlrpcs", "/wp-cron-job", "/wp-admin-page", "/"} {
		if maintAlwaysOpen(http.MethodGet, p) {
			t.Errorf("%s must not bypass maintenance", p)
		}
	}
}

// While maintenance is on, /xmlrpc.php?p= and /wp-cron.php?page_id= must not render the post.
func TestMaintenanceXMLRPCDoesNotServePosts(t *testing.T) {
	ts, _, srv := maintenanceSite(t)
	base := ts.URL
	now := time.Now()
	p := mustInsertPost(t, srv, &store.Post{Type: "post", Title: "MaintSecretTitle", Slug: "maint-secret-title",
		Content: "<p>MaintSecretBody</p>", Status: "publish", PublishedAt: &now, CommentStatus: "closed", PingStatus: "closed"})
	id := strconv.FormatInt(p.ID, 10)
	if open := anonGet(t, base+"/xmlrpc.php?p="+id); open.StatusCode == http.StatusServiceUnavailable {
		t.Fatal("maintenance is off, but /xmlrpc.php was closed")
	}
	if err := srv.maintSetMode(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/xmlrpc.php?p=" + id, "/wp-cron.php?page_id=" + id, "/xmlrpc.php/"} {
		r := anonGet(t, base+path)
		b := bodyOf(r)
		if r.StatusCode != http.StatusServiceUnavailable || strings.Contains(b, "MaintSecretTitle") || strings.Contains(b, "MaintSecretBody") {
			t.Errorf("%s: status %d, body contains the post: %t", path, r.StatusCode, strings.Contains(b, "MaintSecret"))
		}
	}
}
