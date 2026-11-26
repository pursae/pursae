package server

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// gontactEntryEnv sets up the plugin, a page with the sample form and returns what a post needs.
func gontactEntryEnv(t *testing.T) (base string, c *http.Client, srv *Server, f *gfForm, hidden map[string]string, post func(kv ...string) (int, map[string]any, string)) {
	t.Helper()
	base, c, srv = gontactSetup(t, true)
	forms, _, _ := srv.gontactForms(context.Background(), "", "", "", 1, -1)
	f = forms[0]
	shortcode := `[contact-form-7 id="` + itoa64(f.ID) + `" title="x"]`
	_, link := gontactPage(t, srv, shortcode)
	_, page, _ := get(t, anonClient(), link)
	hidden = hiddenFields(t, page)
	feedback := base + "/wp-json/contact-form-7/v1/contact-forms/" + itoa64(f.ID) + "/feedback"
	post = func(kv ...string) (int, map[string]any, string) {
		m := map[string]string{"your-name": "Ann", "your-email": "ann@example.org", "your-subject": "Hi", "your-message": "=cmd|' /C calc'!A0"}
		for k, v := range hidden {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return multipartPost(t, anonClient(), feedback, m, nil, testUA)
	}
	return
}

func entryCount(t *testing.T, srv *Server, where string) int {
	t.Helper()
	var n int
	if err := srv.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM gontact_entries `+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGontactEntriesStoredAndMailStatus(t *testing.T) {
	base, c, srv, f, _, post := gontactEntryEnv(t)
	ctx := context.Background()
	if _, out, raw := post(); out["status"] != "mail_sent" {
		t.Fatalf("submission: %s", raw)
	}
	if n := entryCount(t, srv, ""); n != 1 {
		t.Fatalf("entries: %d", n)
	}
	var id int64
	var status, posted string
	_ = srv.store.Pool.QueryRow(ctx, `SELECT id, mail_status, posted::text FROM gontact_entries`).Scan(&id, &status, &posted)
	if status != "queued" || !strings.Contains(posted, "ann@example.org") {
		t.Fatalf("entry: %s %s", status, posted)
	}
	// The job carries the entry id and reports back, success then failure.
	jobs := mailJobs(t, srv)
	if len(jobs) != 1 || jobs[0]["gontact_entry"] != float64(id) {
		t.Fatalf("mail job: %v", jobs)
	}
	srv.gontactJobDelivered(ctx, jobs[0], nil)
	_ = srv.store.Pool.QueryRow(ctx, `SELECT mail_status FROM gontact_entries WHERE id = $1`, id).Scan(&status)
	if status != "sent" {
		t.Fatalf("after delivery: %s", status)
	}
	srv.gontactJobDelivered(ctx, jobs[0], errors.New("smtp down"))
	_ = srv.store.Pool.QueryRow(ctx, `SELECT mail_status FROM gontact_entries WHERE id = $1`, id).Scan(&status)
	if status != "failed" {
		t.Fatalf("after failure: %s", status)
	}

	// Admin: failed-delivery warning, list, detail, CSV (formula-safe), delete.
	_, body, _ := get(t, c, base+"/wp-admin/admin?page=wpcf7")
	if !strings.Contains(body, "could not be delivered") {
		t.Fatal("no failed delivery warning on Contact Forms")
	}
	_, body, _ = get(t, c, base+"/wp-admin/admin?page=wpcf7-entries&s=ann%40example")
	if !strings.Contains(body, "your-email: ann@example.org") || !strings.Contains(body, "failed") {
		t.Fatalf("entries list:\n%s", body)
	}
	_, body, _ = get(t, c, base+"/wp-admin/admin?page=wpcf7-entries&entry="+itoa64(id))
	if !strings.Contains(body, "Ann") || !strings.Contains(body, "IP address") {
		t.Fatal("entry detail")
	}
	if code, _, _ := get(t, c, base+"/wp-admin/admin?page=wpcf7-entries&action=export"); code == http.StatusOK {
		t.Fatal("export without a nonce")
	}
	_, csv, _ := get(t, c, base+"/wp-admin/admin?page=wpcf7-entries&action=export&_wpnonce="+scrapeExportNonce(t, c, base))
	if !strings.Contains(csv, "ID,Form,Date,IP,Mail status,your-email") || !strings.Contains(csv, "'=cmd") {
		t.Fatalf("csv:\n%s", csv)
	}
	resp, err := c.PostForm(base+"/wp-admin/admin?page=wpcf7-entries", url.Values{"action": {"delete"}, "entry[]": {itoa64(id)}, "_wpnonce": {scrapeBulkNonce(t, c, base)}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n := entryCount(t, srv, ""); n != 0 {
		t.Fatalf("entry not deleted: %d", n)
	}
	_ = f
}

func TestGontactDoNotStoreAndRetention(t *testing.T) {
	_, _, srv, f, _, post := gontactEntryEnv(t)
	ctx := context.Background()
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE posts SET meta = jsonb_set(meta, '{_additional_settings}', '"do_not_store: true"') WHERE id = $1`, f.ID); err != nil {
		t.Fatal(err)
	}
	srv.purgeAll(ctx)
	if _, out, raw := post(); out["status"] != "mail_sent" {
		t.Fatalf("submission: %s", raw)
	}
	if n := entryCount(t, srv, ""); n != 0 {
		t.Fatalf("do_not_store form stored %d entries", n)
	}
	// Retention and the attachment scrub of the hourly event.
	_, _ = srv.store.Pool.Exec(ctx, `INSERT INTO gontact_entries (form_id, posted, created_at) VALUES (1, '{}', now() - interval '40 days'), (1, '{}', now())`)
	_, _ = srv.store.Pool.Exec(ctx, `INSERT INTO jobs (name, status, payload) VALUES ('wp_mail', 'done', '{"Subject":"s","Attachments":[{"Name":"a","Data":"QQ=="}]}')`)
	if err := srv.settings.Set(ctx, map[string]any{gontactRetentionOption: 30}); err != nil {
		t.Fatal(err)
	}
	if err := srv.gontactEntriesCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if n := entryCount(t, srv, ""); n != 1 {
		t.Fatalf("retention kept %d", n)
	}
	var has bool
	_ = srv.store.Pool.QueryRow(ctx, `SELECT payload ? 'Attachments' FROM jobs WHERE name = 'wp_mail' AND status = 'done'`).Scan(&has)
	if has {
		t.Fatal("attachments stay in a finished job")
	}
}

func TestGontactHoneypotAndTimestamp(t *testing.T) {
	_, _, srv, _, hidden, post := gontactEntryEnv(t)
	if hidden[gontactTimestampField] == "" {
		t.Fatal("the form has no signed timestamp")
	}
	if _, out, raw := post(gontactHoneypotField, "http://spam.example"); out["status"] != "spam" {
		t.Fatalf("honeypot: %s", raw)
	}
	if _, out, raw := post(gontactTimestampField, "123.deadbeef"); out["status"] != "spam" {
		t.Fatalf("forged timestamp: %s", raw)
	}
	gontactMinFillSeconds = 3
	if _, out, raw := post(gontactTimestampField, srv.gontactSignTimestamp(time.Now().Unix())); out["status"] != "spam" {
		t.Fatalf("instant post: %s", raw)
	}
	if _, out, raw := post(gontactTimestampField, srv.gontactSignTimestamp(time.Now().Add(-10*time.Second).Unix())); out["status"] != "mail_sent" {
		t.Fatalf("slow post: %s", raw)
	}
	if entryCount(t, srv, "") != 1 {
		t.Fatal("spam submissions were stored")
	}
}

func TestGontactRateLimitsAndCaptchaCap(t *testing.T) {
	base, _, srv, f, _, post := gontactEntryEnv(t)
	gooikeLimiter.Lock()
	gooikeLimiter.window, gooikeLimiter.hits = time.Now(), map[string]int{}
	gooikeLimiter.Unlock()
	limited := 0
	for i := 0; i < gontactFeedbackPerMin+3; i++ {
		if code, out, _ := post(); code == http.StatusTooManyRequests && out["code"] == "wpcf7_rate_limited" {
			limited++
		}
	}
	if limited != 3 {
		t.Fatalf("limited feedback posts: %d", limited)
	}
	refill := base + "/wp-json/contact-form-7/v1/contact-forms/" + itoa64(f.ID) + "/refill"
	got429 := false
	for i := 0; i < gontactRefillPerMin+2; i++ {
		resp, err := http.Get(refill)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		got429 = got429 || resp.StatusCode == http.StatusTooManyRequests
	}
	if !got429 {
		t.Fatal("refill is not rate limited")
	}
	old := gontactCaptchaMaxRows
	gontactCaptchaMaxRows = 1
	defer func() { gontactCaptchaMaxRows = old }()
	for i := 0; i < 3; i++ {
		if _, err := srv.gontactCaptchaNew(context.Background(), gfCaptchaOptions(nil)); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	_ = srv.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM gontact_captcha`).Scan(&rows)
	if rows > 1 {
		t.Fatalf("CAPTCHA challenges are not capped: %d rows", rows)
	}
}

func scrapeBulkNonce(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	_, body, _ := get(t, c, base+"/wp-admin/admin?page=wpcf7-entries")
	m := regexp.MustCompile(`name="_wpnonce" value="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no nonce on the entries screen")
	}
	return m[1]
}

func scrapeExportNonce(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	_, body, _ := get(t, c, base+"/wp-admin/admin?page=wpcf7-entries")
	m := regexp.MustCompile(`_wpnonce=([0-9a-f]+)[^"]*action=export`).FindStringSubmatch(html.UnescapeString(body))
	if m == nil {
		t.Fatal("no export link on the entries screen")
	}
	return m[1]
}
