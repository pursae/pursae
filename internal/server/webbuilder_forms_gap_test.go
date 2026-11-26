package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

// wbfSetContent edits the content of one section of the stored doc of a page.
func wbfSetContent(t *testing.T, srv *Server, postID int64, section string, kv map[string]string) {
	t.Helper()
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("..", "webbuilder", "testdata", "render", "variants-form.doc.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sec := range doc["sections"].([]any) {
		m := sec.(map[string]any)
		if m["id"] != section {
			continue
		}
		content, _ := m["content"].(map[string]any)
		if content == nil {
			content = map[string]any{}
			m["content"] = content
		}
		for k, v := range kv {
			content[k] = v
		}
		found = true
	}
	if !found {
		t.Fatalf("section %s not in the fixture", section)
	}
	cur, err := srv.store.WebBuilder().Doc(ctx, postID)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(doc)
	d := &store.WebBuilderDoc{PostID: postID, Doc: out, Renderer: webbuilder.RendererVersion, ContentHash: store.WebBuilderContentHash("<p>fallback</p>"), SnapshotUser: 1}
	if err := srv.store.WebBuilder().PublishDoc(ctx, d, cur.Rev); err != nil {
		t.Fatal(err)
	}
}

func TestWebBuilderFormAddresses(t *testing.T) {
	got := webbuilder.FormAddresses("a@example.test, Bob <b@example.test>; a@EXAMPLE.test\nc@example.test, nope, d@x")
	if strings.Join(got, "|") != "a@example.test|c@example.test" {
		t.Fatalf("addresses: %v", got)
	}
}

func TestWebBuilderFormStoredRecipientsMessages(t *testing.T) {
	w := wbformStart(t)
	ctx := context.Background()
	if err := w.srv.settings.Set(ctx, map[string]any{wbFormToOption: "sales@example.test", wbFormCcOption: "boss@example.test",
		wbFormSuccessOption: "Thanks, we will call you."}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := w.submit(t, nil, w.page.ID, wbfNewsletter, url.Values{"email": {"bob@example.test"}}, true)
	if code != http.StatusOK || out["message"] != "Thanks, we will call you." {
		t.Fatalf("site defaults: %d %v", code, out)
	}
	m := mailJobs(t, w.srv)[0]
	to, _ := m["To"].([]any)
	cc, _ := m["Cc"].([]any)
	if len(to) != 1 || to[0] != "sales@example.test" || len(cc) != 1 || cc[0] != "boss@example.test" {
		t.Fatalf("recipients: %v", m)
	}

	// The form's own settings win over the site defaults.
	wbfSetContent(t, w.srv, w.page.ID, wbfNewsletter, map[string]string{
		webbuilder.FormKeyTo: "news@example.test", webbuilder.FormKeySuccess: "Subscribed!",
		webbuilder.FormKeyRedirect: "/thanks/"})
	code, out, _ = w.submit(t, nil, w.page.ID, wbfNewsletter, url.Values{"email": {"carl@example.test"}}, true)
	if code != http.StatusOK || out["message"] != "Subscribed!" || out["redirect"] != strings.TrimSuffix(w.base, "/")+"/thanks" {
		t.Fatalf("form overrides: %d %v", code, out)
	}
	m = mailJobs(t, w.srv)[1]
	if to, _ := m["To"].([]any); len(to) != 1 || to[0] != "news@example.test" {
		t.Fatalf("own recipient: %v", m)
	}
	// No JavaScript: the redirect is a 303 to the thank-you page.
	code, _, hdr := w.submit(t, nil, w.page.ID, wbfNewsletter, url.Values{"email": {"dan@example.test"}}, false)
	if code != http.StatusSeeOther || !strings.HasSuffix(hdr.Get("Location"), "/thanks") {
		t.Fatalf("no-JS redirect: %d %s", code, hdr.Get("Location"))
	}
	// An address of another site is never used.
	wbfSetContent(t, w.srv, w.page.ID, wbfNewsletter, map[string]string{webbuilder.FormKeyRedirect: "https://evil.example.org/x"})
	_, out, _ = w.submit(t, nil, w.page.ID, wbfNewsletter, url.Values{"email": {"eve@example.test"}}, true)
	if r, _ := out["redirect"].(string); r != "" {
		t.Fatalf("external redirect kept: %v", out)
	}

	rows, err := w.srv.store.WebBuilder().Submissions(ctx, 10, 0)
	if err != nil || len(rows) != 4 {
		t.Fatalf("stored submissions: %d %v", len(rows), err)
	}
	if rows[3].Fields["email"] != "bob@example.test" || !rows[3].Mailed || rows[3].SectionID != wbfNewsletter || rows[3].PageURL == "" {
		t.Fatalf("first submission: %+v", rows[3])
	}
}

func TestWebBuilderFormStoredWhenMailFails(t *testing.T) {
	w := wbformStart(t)
	ctx := context.Background()
	if err := w.srv.settings.Set(ctx, map[string]any{"admin_email": ""}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := w.submit(t, nil, w.page.ID, wbfNewsletter, url.Values{"email": {"bob@example.test"}}, true)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("stored but not mailed: %d %v", code, out)
	}
	rows, _ := w.srv.store.WebBuilder().Submissions(ctx, 10, 0)
	if len(rows) != 1 || rows[0].Mailed || len(mailJobs(t, w.srv)) != 0 {
		t.Fatalf("rows=%+v mails=%d", rows, len(mailJobs(t, w.srv)))
	}
	// With storing off and no way to mail, the visitor is told.
	if err := w.srv.settings.Set(ctx, map[string]any{wbFormStoreOption: false}); err != nil {
		t.Fatal(err)
	}
	code, _, _ = w.submit(t, nil, w.page.ID, wbfNewsletter, url.Values{"email": {"bob@example.test"}}, true)
	if code != http.StatusInternalServerError {
		t.Fatalf("not stored, not mailed: %d", code)
	}
}

func TestWebBuilderFormDefaultMessageUsesTranslation(t *testing.T) {
	w := wbformStart(t)
	if w.srv.lang == nil {
		t.Skip("no language manager")
	}
	// English site: the default text, through frontT (so Multi Language can translate it).
	if got := w.srv.wbFormMessage(context.Background(), "", wbFormSuccessOption, wbFormSentMsg); got != wbFormSentMsg {
		t.Fatalf("default: %q", got)
	}
	if got := w.srv.wbFormMessage(context.Background(), "  Custom\r\nline ", wbFormSuccessOption, wbFormSentMsg); got != "Custom line" {
		t.Fatalf("custom: %q", got)
	}
}

func TestWebBuilderFormCaptchaWidgets(t *testing.T) {
	w := wbformStart(t)
	ctx := context.Background()
	html := `<div class="wb-page"><form data-wb-form="" method="post" action="/wp-json/web-builder/v2/forms/1/s_a"><input name="email"></form>` +
		`<form data-wb-form="" method="post" action="/wp-json/web-builder/v2/forms/1/s_b"></form><form data-wb-search="" action="/"></form></div>`
	run := func() (string, *gin.Context) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		return w.srv.wbFormEnhance(c, nil, html), c
	}
	out, _ := run()
	if strings.Count(out, `name="wb_nonce"`) != 2 || strings.Contains(out, `data-wb-search="" action="/"><input type="hidden" name="wb_nonce"`) {
		t.Fatalf("nonce on builder forms only:\n%s", out)
	}
	if !strings.Contains(out, `value="s.`) {
		t.Fatalf("anonymous token missing:\n%s", out)
	}
	if err := w.srv.settings.Set(ctx, map[string]any{gontactSetting: true, "gontact_turnstile_sitekey": "ts-site", "gontact_turnstile_secret": "ts-secret",
		"gontact_recaptcha_sitekey": "rc-site", "gontact_recaptcha_secret": "rc-secret"}); err != nil {
		t.Fatal(err)
	}
	out, _ = run() // captcha widgets plus the form token
	if n := strings.Count(out, `<div class="cf-turnstile" data-sitekey="ts-site"></div>`); n != 2 {
		t.Fatalf("turnstile widgets: %d\n%s", n, out)
	}
	if n := strings.Count(out, `name="g-recaptcha-response"`); n != 2 {
		t.Fatalf("recaptcha inputs: %d", n)
	}
	if !regexp.MustCompile(`(?s)<input name="email">.*<div class="cf-turnstile".*name="wb_nonce".*?</form>`).MatchString(out) {
		t.Fatalf("widget not inside the form:\n%s", out)
	}
	for _, want := range []string{"challenges.cloudflare.com/turnstile/v0/api.js", "recaptcha/api.js?render=rc-site", `id="wb-recaptcha-run"`} {
		if strings.Count(out, want) != 1 {
			t.Fatalf("%q count != 1", want)
		}
	}
	if strings.Contains(out, `data-wb-search="" action="/"><div`) {
		t.Fatal("a search form got a widget")
	}
}

func TestWebBuilderFormRevisionsPrune(t *testing.T) {
	w := wbformStart(t)
	ctx := context.Background()
	wb := w.srv.store.WebBuilder()
	var ids []int64
	for i := 1; i <= 5; i++ {
		var rid int64
		if err := w.srv.store.Pool.QueryRow(ctx, `INSERT INTO post_revisions(post_id, author_id, title, excerpt, content) VALUES ($1, 1, 't', '', 'c') RETURNING id`, w.page.ID).Scan(&rid); err != nil {
			t.Fatal(err)
		}
		if err := wb.AddRevision(ctx, &store.WebBuilderRevision{RevisionID: rid, PostID: w.page.ID, Rev: i, Doc: json.RawMessage(`{"v":1}`)}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rid)
	}
	if n, err := wb.PruneRevisions(ctx, w.page.ID, 0); err != nil || n != 0 {
		t.Fatalf("keep 0 must keep all: %d %v", n, err)
	}
	if n, err := wb.PruneRevisions(ctx, w.page.ID, 2); err != nil || n != 3 {
		t.Fatalf("prune: %d %v", n, err)
	}
	list, _ := wb.Revisions(ctx, w.page.ID, 0, 0)
	if len(list) != 2 || list[0].Rev != 5 || list[1].Rev != 4 {
		t.Fatalf("kept: %+v", list)
	}
	// The core revisions stay.
	var n int
	_ = w.srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM post_revisions WHERE id = ANY($1)`, ids).Scan(&n)
	if n != 5 {
		t.Fatalf("core revisions: %d", n)
	}

	if got := w.srv.wbRevisionsKeep(); got != wbRevisionsKeepDefault {
		t.Fatalf("default keep: %d", got)
	}
	for in, want := range map[any]int{"10": 10, float64(0): 0, "-3": wbRevisionsKeepDefault, "99999": wbRevisionsKeepMax, "x": wbRevisionsKeepDefault} {
		if err := w.srv.settings.Set(ctx, map[string]any{wbRevisionsKeepOption: in}); err != nil {
			t.Fatal(err)
		}
		if got := w.srv.wbRevisionsKeep(); got != want {
			t.Fatalf("keep %v: %d, want %d", in, got, want)
		}
	}
}

func TestWebBuilderFormSubmissionsScreen(t *testing.T) {
	w := wbformStart(t)
	ctx := context.Background()
	w.submit(t, nil, w.page.ID, wbfNewsletter, url.Values{"email": {"bob@example.test"}}, true)
	w.submit(t, nil, w.page.ID, wbfNewsletter, url.Values{"email": {"amy@example.test"}}, true)

	_, body, _ := get(t, w.user, w.base+wbSettingsURL)
	for _, want := range []string{`id="web-builder-submissions"`, "bob@example.test", "amy@example.test", `name="web_builder_form_to"`, `name="web_builder_revisions_keep"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings screen lacks %s", want)
		}
	}
	nonce := regexp.MustCompile(`name="_wpnonce" value="([^"]+)"`).FindStringSubmatch(body)
	rows, _ := w.srv.store.WebBuilder().Submissions(ctx, 10, 0)
	form := url.Values{"option_page": {"plugin:web-builder"}, "action": {"update"}, "_wpnonce": {nonce[1]},
		"wb_submissions": {"delete"}, "submission[]": {strconvI(rows[0].ID)}}
	if code, _, _ := post(t, w.user, w.base+"/wp-admin/options", form); code != http.StatusFound {
		t.Fatalf("delete: %d", code)
	}
	if n, _ := w.srv.store.WebBuilder().CountSubmissions(ctx); n != 1 {
		t.Fatalf("after delete: %d", n)
	}
	form.Set("wb_submissions", "delete-all")
	post(t, w.user, w.base+"/wp-admin/options", form)
	if n, _ := w.srv.store.WebBuilder().CountSubmissions(ctx); n != 0 {
		t.Fatalf("after delete all: %d", n)
	}
	form.Set("_wpnonce", "bad")
	if code, _, _ := post(t, w.user, w.base+"/wp-admin/options", form); code != http.StatusForbidden {
		t.Fatalf("bad nonce: %d", code)
	}
}

func strconvI(n int64) string { return strconv.FormatInt(n, 10) }
