package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// postHdr posts a form with extra request headers.
func postHdr(t *testing.T, c *http.Client, u string, form url.Values, hdr map[string]string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String(), resp.Header.Get("Location")
}

// Login CSRF: a cross-site POST to wp-login signs nobody in; a same-site or header-less one still works.
func TestLoginRefusesCrossSitePost(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	creds := url.Values{"log": {"admin"}, "pwd": {"correct horse"}}
	for _, hdr := range []map[string]string{{"Origin": "http://evil.test"}, {"Sec-Fetch-Site": "cross-site"}, {"Origin": "null"}} {
		resp := asNewClient()
		if code, body, _ := postHdr(t, resp, ts.URL+"/wp-login", creds, hdr); code != http.StatusForbidden || !strings.Contains(body, "another site") {
			t.Errorf("%v: %d", hdr, code)
		}
		if u, _ := url.Parse(ts.URL); len(resp.Jar.Cookies(u)) > 0 {
			t.Errorf("%v: a cookie was set", hdr)
		}
	}
	for _, hdr := range []map[string]string{nil, {"Origin": ts.URL, "Sec-Fetch-Site": "same-origin"}} {
		if code, _, _ := postHdr(t, asNewClient(), ts.URL+"/wp-login", creds, hdr); code != http.StatusFound {
			t.Errorf("%v: login %d", hdr, code)
		}
	}
}

// wp-comments-post: a signed-in visitor's cookie cannot post from another site; anonymous posts are unchanged.
func TestCommentPostRefusesCrossSiteWithSession(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	form := url.Values{"comment_post_ID": {"1"}, "comment": {"csrf test"}}
	if code, _, _ := postHdr(t, c, ts.URL+"/wp-comments-post", form, map[string]string{"Origin": "http://evil.test"}); code != http.StatusForbidden {
		t.Errorf("signed-in, foreign Origin: %d", code)
	}
	if code, _, _ := postHdr(t, c, ts.URL+"/wp-comments-post", form, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Errorf("signed-in, cross-site: %d", code)
	}
	if code, _, _ := postHdr(t, c, ts.URL+"/wp-comments-post", form, map[string]string{"Origin": ts.URL}); code != http.StatusFound {
		t.Errorf("signed-in, same Origin: %d", code)
	}
	anon := url.Values{"comment_post_ID": {"1"}, "comment": {"anonymous words"}, "author": {"Ann"}, "email": {"ann@example.test"}}
	if code, _, _ := postHdr(t, asNewClient(), ts.URL+"/wp-comments-post", anon, map[string]string{"Origin": "http://evil.test"}); code == http.StatusForbidden {
		t.Errorf("anonymous comment refused for its Origin: %d", code)
	}
}

// My Account has its own lost-password form: with a hidden login URL nothing on the way reveals it, the
// mailed link opens My Account, and the new password works.
func TestGooAccountLostPassword(t *testing.T) {
	base, c, srv := gooSite(t)
	ctx := context.Background()
	asToggle(t, c, base, "activate")
	asConfigure(t, srv, func(st *asSettings) { st.HideLogin, st.LoginSlug = true, "secret-door" })
	if _, err := srv.gooCreateCustomer(ctx, "rita@example.test", "rita", "old-Pass-1234!", nil); err != nil {
		t.Fatal(err)
	}
	v := gooFrontClient()
	_, account, _ := get(t, v, base+"/my-account/")
	code, page, loc := get(t, v, base+"/my-account/lost-password/")
	if code != http.StatusOK || !strings.Contains(page, "woocommerce-lost-password-nonce") {
		t.Fatalf("lost-password endpoint: %d %s", code, loc)
	}
	for name, body := range map[string]string{"account": account, "lost-password": page} {
		if strings.Contains(body, "secret-door") || strings.Contains(body, "wp-login") {
			t.Errorf("%s page reveals the login URL", name)
		}
	}
	if !strings.Contains(account, `/my-account/lost-password/`) {
		t.Error("the login form's lost-password link does not open My Account")
	}
	nonce := gooHidden(t, page, "woocommerce-lost-password-nonce")
	// Unknown and known accounts get the same answer.
	_, _, locUnknown := post(t, v, base+"/my-account/lost-password/", url.Values{"user_login": {"nobody-here"}, "woocommerce-lost-password-nonce": {nonce}})
	_, _, locKnown := post(t, v, base+"/my-account/lost-password/", url.Values{"user_login": {"rita"}, "woocommerce-lost-password-nonce": {nonce}})
	if locKnown != locUnknown || !strings.Contains(locKnown, "reset-link-sent=true") {
		t.Fatalf("answers differ or wrong: %q %q", locUnknown, locKnown)
	}
	if _, body, _ := get(t, v, locKnown); !strings.Contains(body, "Password reset email has been sent.") {
		t.Error("no confirmation")
	}
	m := lastMail(t, "Password")
	link := regexp.MustCompile(`https?://[^\s<>"]*/my-account/lost-password/\?key=[^\s<>"]+`).FindString(m.Body)
	if link == "" || strings.Contains(m.Body, "secret-door") {
		t.Fatalf("reset mail link: %q", m.Body)
	}
	link = strings.ReplaceAll(link, "&amp;", "&")
	lu, _ := url.Parse(link)
	code, form, _ := get(t, v, base+lu.RequestURI())
	if code != http.StatusOK || !strings.Contains(form, "woocommerce-reset-password-nonce") {
		t.Fatalf("reset form: %d", code)
	}
	vals := url.Values{"reset_key": {gooHidden(t, form, "reset_key")}, "reset_login": {gooHidden(t, form, "reset_login")},
		"woocommerce-reset-password-nonce": {gooHidden(t, form, "woocommerce-reset-password-nonce")}}
	vals.Set("password_1", "new-Pass-5678!")
	vals.Set("password_2", "different")
	if _, _, loc := post(t, v, base+"/my-account/lost-password/", vals); !strings.Contains(loc, "key=") {
		t.Errorf("mismatch: %q", loc)
	}
	vals.Set("password_2", "new-Pass-5678!")
	if _, _, loc := post(t, v, base+"/my-account/lost-password/", vals); !strings.Contains(loc, "password-reset=true") {
		t.Fatalf("reset: %q", loc)
	}
	// The key is spent.
	if _, body, _ := get(t, v, base+lu.RequestURI()); !strings.Contains(body, "This key is invalid") {
		t.Error("the key still works")
	}
	u, _ := srv.store.UserByLoginOrEmail(ctx, "rita")
	if !srv.checkUserPassword(ctx, u, "new-Pass-5678!") {
		t.Error("the new password does not work")
	}
}

// Analytics: beacons from another site's page are dropped, the visitor hash ignores the Host header, and
// one address records at most analyticsViewsPerMinute views a minute.
func TestAnalyticsCollectOriginAndLimit(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	if err := srv.analyticsActivate(ctx); err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM analytics_events WHERE name = 'pageview'`).Scan(&n)
		return
	}
	beacon(t, ts.URL, map[string]any{"t": "v", "id": 11, "p": "/", "w": 1200}, map[string]string{"Origin": "http://evil.test"})
	beacon(t, ts.URL, map[string]any{"t": "v", "id": 12, "p": "/", "w": 1200}, map[string]string{"Referer": "http://evil.test/page"})
	if n := count(); n != 0 {
		t.Fatalf("%d foreign views recorded", n)
	}
	beacon(t, ts.URL, map[string]any{"t": "v", "id": 13, "p": "/", "w": 1200}, map[string]string{"Origin": ts.URL})
	if n := count(); n != 1 {
		t.Fatalf("same-site view: %d", n)
	}
	a, _ := srv.analyticsVisitor(ctx, "198.51.100.1", "ua", srv.analyticsSiteHost())
	b, _ := srv.analyticsVisitor(ctx, "198.51.100.1", "ua", srv.analyticsSiteHost())
	if a != b {
		t.Error("visitor hash is not stable")
	}
	var visitors int
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+analyticsCollect, strings.NewReader(`{"t":"v","id":`+itoa64(int64(100+i))+`,"p":"/","w":1200}`))
		req.Host = "spoof" + itoa64(int64(i)) + ".test"
		req.Header.Set("User-Agent", "Mozilla/5.0 Safari/604.1")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(DISTINCT visitor) FROM analytics_events WHERE page_id >= 100 AND page_id < 103`).Scan(&visitors)
	if visitors > 1 {
		t.Errorf("Host header made %d visitors", visitors)
	}
	for i := 0; i < analyticsViewsPerMinute+10; i++ {
		beacon(t, ts.URL, map[string]any{"t": "v", "id": 1000 + i, "p": "/", "w": 1200}, nil)
	}
	var flood int
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM analytics_events WHERE page_id >= 1000`).Scan(&flood)
	if flood >= analyticsViewsPerMinute+10 {
		t.Errorf("%d views from one address in a minute", flood)
	}
}

// Notification mails are capped per source network and per site; the content is still saved.
func TestNotifyMailCap(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	forumLimiterReset()
	defer forumLimiterReset()
	for i := 0; i < notifyMailPerSource; i++ {
		if !srv.notifyMailAllowed("comment", "203.0.113.5") {
			t.Fatalf("mail %d refused", i)
		}
	}
	if srv.notifyMailAllowed("comment", "203.0.113.77") {
		t.Error("the same /24 was not capped")
	}
	allowed := notifyMailPerSource
	for i := 0; i < 2*notifyMailPerSite; i++ {
		if srv.notifyMailAllowed("contact", "198.51."+itoa64(int64(i))+".1") {
			allowed++
		}
	}
	if allowed != notifyMailPerSite {
		t.Errorf("site cap: %d mails, want %d", allowed, notifyMailPerSite)
	}

	// Comments past the cap are saved but not mailed.
	forumLimiterReset()
	_ = srv.settings.Set(context.Background(), map[string]any{"moderation_notify": true})
	before := asCount(t, `SELECT count(*) FROM jobs WHERE name = 'wp_mail' AND payload->>'Subject' LIKE '%Please moderate%'`)
	for i := 0; i < notifyMailPerSource+3; i++ {
		srv.notifyMailAllowed("comment", "127.0.0.1") // use up the loopback source
	}
	anon := url.Values{"comment_post_ID": {"1"}, "comment": {"capped words"}, "author": {"Bo"}, "email": {"bo@example.test"}}
	post(t, asNewClient(), ts.URL+"/wp-comments-post", anon)
	if n := asCount(t, `SELECT count(*) FROM comments WHERE content = 'capped words'`); n != 1 {
		t.Errorf("capped comment not saved: %d", n)
	}
	if after := asCount(t, `SELECT count(*) FROM jobs WHERE name = 'wp_mail' AND payload->>'Subject' LIKE '%Please moderate%'`); after != before {
		t.Errorf("moderation mail sent past the cap: %d -> %d", before, after)
	}
}

// An unfiltered_html user's comment keeps raw HTML only when the form's _wp_unfiltered_html_comment
// nonce is valid; without it the comment is filtered like everyone else's.
func TestCommentUnfilteredHTMLNeedsNonce(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx := context.Background()
	_, _, loc := get(t, c, ts.URL+"/?p=1")
	_, page, _ := get(t, c, loc)
	nonce := gooHidden(t, page, "_wp_unfiltered_html_comment")
	stored := func(marker string) string {
		var s string
		_ = srv.store.Pool.QueryRow(ctx, `SELECT content FROM comments WHERE content LIKE '%'||$1||'%'`, marker).Scan(&s)
		return s
	}
	send := func(marker, n string) {
		form := url.Values{"comment_post_ID": {"1"}, "comment": {`<div class="` + marker + `">` + marker + `</div>`}}
		if n != "" {
			form.Set("_wp_unfiltered_html_comment", n)
		}
		if code, _, _ := postHdr(t, c, ts.URL+"/wp-comments-post", form, nil); code != http.StatusFound {
			t.Fatalf("%s: %d", marker, code)
		}
	}
	send("nononce", "")
	send("badnonce", "0123456789")
	send("goodnonce", nonce)
	for _, m := range []string{"nononce", "badnonce"} {
		if strings.Contains(stored(m), "<div") {
			t.Errorf("%s: raw HTML kept: %q", m, stored(m))
		}
	}
	if !strings.Contains(stored("goodnonce"), `<div class="goodnonce">`) {
		t.Errorf("valid nonce: raw HTML not kept: %q", stored("goodnonce"))
	}
}
