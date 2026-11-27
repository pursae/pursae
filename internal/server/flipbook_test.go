package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"wpgo/internal/store"
)

func TestFlipbookSanitize(t *testing.T) {
	b := flipbookSanitize(flipbook{Title: "  Katalog ", PDFURL: "javascript:alert(1)", Height: 5, Background: "red"})
	if b.Title != "Katalog" || b.PDFURL != "" || b.Height != flipbookDefaultHeight || b.Background != flipbookDefaultBG {
		t.Fatalf("sanitize: %+v", b)
	}
	b = flipbookSanitize(flipbook{PDFURL: "/wp-content/uploads/a.pdf", Height: 480, Background: "#112233"})
	if b.PDFURL != "/wp-content/uploads/a.pdf" || b.Height != 480 || b.Background != "#112233" {
		t.Fatalf("sanitize kept values: %+v", b)
	}
}

func TestFlipbookTitleCutsCharacters(t *testing.T) {
	b := flipbookSanitize(flipbook{Title: strings.Repeat("ğ", 250)})
	if !utf8.ValidString(b.Title) || utf8.RuneCountInString(b.Title) != 200 {
		t.Fatalf("title: %d runes, valid=%v", utf8.RuneCountInString(b.Title), utf8.ValidString(b.Title))
	}
}

// The PDF comes from the Media Library, not the form; an author manages only their own books; and
// /flipbook/ addresses that are not a book's are the site's.
func TestFlipbookAdminAndRoute(t *testing.T) {
	ts, c, srv := newTestServer(t, false)
	base := ts.URL
	ctx := context.Background()
	installAndLogin(t, ts, c)
	if err := srv.settings.Set(ctx, map[string]any{flipbookSetting: true}); err != nil {
		t.Fatal(err)
	}
	pdf := &store.Attachment{UploaderID: 1, Slug: "katalog", FilePath: "2026/11/katalog.pdf", Mime: "application/pdf", Title: "Katalog"}
	png := &store.Attachment{UploaderID: 1, Slug: "logo", FilePath: "2026/11/logo.png", Mime: "image/png", Title: "Logo"}
	for _, a := range []*store.Attachment{pdf, png} {
		if err := srv.store.InsertAttachment(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	nonceRE := regexp.MustCompile(`name="_wpnonce" value="([^"]+)"`)
	create := func(cl *http.Client, form url.Values) string {
		t.Helper()
		_, page, _ := get(t, cl, base+"/wp-admin/admin?page="+flipbookSlug)
		m := nonceRE.FindStringSubmatch(page)
		if m == nil {
			t.Fatal("no nonce on the Flipbook screen")
		}
		form.Set("_wpnonce", m[1])
		form.Set("op", "create")
		_, _, loc := post(t, cl, base+"/wp-admin/admin?page="+flipbookSlug, form)
		return loc
	}

	// A forged address or a non-PDF attachment is refused; the real PDF keeps its own URL.
	if loc := create(c, url.Values{"attachment_id": {itoa64(png.ID)}, "pdf_url": {"https://evil.example/x.pdf"}, "mime": {"application/pdf"}}); !strings.Contains(loc, "pfb-error") {
		t.Fatalf("non-PDF attachment accepted: %s", loc)
	}
	if loc := create(c, url.Values{"pdf_url": {"https://evil.example/x.pdf"}, "mime": {"application/pdf"}}); !strings.Contains(loc, "pfb-error") {
		t.Fatalf("PDF without an attachment accepted: %s", loc)
	}
	if loc := create(c, url.Values{"attachment_id": {itoa64(pdf.ID)}, "pdf_url": {"https://evil.example/x.pdf"}}); !strings.Contains(loc, "pfb-msg=created") {
		t.Fatalf("create: %s", loc)
	}
	books := srv.flipbookLoad().Items
	if len(books) != 1 || strings.Contains(books[0].PDFURL, "evil") || !strings.HasSuffix(books[0].PDFURL, "/wp-content/uploads/2026/11/katalog.pdf") ||
		books[0].Author != 1 || books[0].FileName != "katalog.pdf" {
		t.Fatalf("book: %+v", books)
	}
	adminBook := books[0].ID

	// An author creates their own book but may not edit or delete the admin's.
	asCreateUser(t, srv, "writer", "author", asStrongPass)
	ac := asNewClient()
	if code, _, _ := asLogin(t, ac, base, "/wp-login", "writer", asStrongPass); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	if loc := create(ac, url.Values{"attachment_id": {itoa64(pdf.ID)}}); !strings.Contains(loc, "pfb-msg=created") {
		t.Fatalf("author create: %s", loc)
	}
	_, list, _ := get(t, ac, base+"/wp-admin/admin?page="+flipbookSlug)
	if strings.Contains(list, "action=edit&amp;id="+strconv.Itoa(adminBook)+`"`) || strings.Contains(list, "action=delete&amp;id="+strconv.Itoa(adminBook)+"&") {
		t.Error("the author sees edit or delete links for the admin's book")
	}
	if _, _, loc := get(t, ac, base+"/wp-admin/admin?page="+flipbookSlug+"&action=edit&id="+strconv.Itoa(adminBook)); !strings.Contains(loc, "pfb-error") {
		t.Errorf("author opened the admin's book: %s", loc)
	}
	m := nonceRE.FindStringSubmatch(list)
	if _, _, loc := post(t, ac, base+"/wp-admin/admin?page="+flipbookSlug, url.Values{"_wpnonce": {m[1]}, "op": {"save"},
		"id": {strconv.Itoa(adminBook)}, "title": {"Hijacked"}}); !strings.Contains(loc, "pfb-error") {
		t.Errorf("author saved the admin's book: %s", loc)
	}
	if b, ok := srv.flipbookGet(adminBook); !ok || b.Title == "Hijacked" {
		t.Fatalf("admin's book changed: %+v %v", b, ok)
	}

	// The book's own page is the preview of its editors: never a visitor's or another author's, and
	// kept out of search engines. Its QR codes wait for the shortcode to be published.
	bookURL := base + "/flipbook/" + strconv.Itoa(adminBook)
	qrQuery := "?u=" + url.QueryEscape("/catalog/")
	for _, u := range []string{bookURL, bookURL + "/qr.svg" + qrQuery, bookURL + "/qr.png" + qrQuery} {
		if code, _, _ := get(t, http.DefaultClient, u); code != http.StatusNotFound {
			t.Errorf("unpublished book answered a visitor: %s %d", u, code)
		}
	}
	if code, _, _ := get(t, ac, bookURL); code != http.StatusNotFound {
		t.Errorf("the preview answered another author: %d", code)
	}
	preview := func() {
		t.Helper()
		resp, err := c.Get(bookURL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Robots-Tag") != "noindex, nofollow" || !strings.Contains(string(body), `content="noindex, nofollow"`) ||
			!strings.Contains(resp.Header.Get("Cache-Control"), "no-store") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "worker-src 'self'") ||
			strings.Contains(string(body), "data-share") || strings.Contains(string(body), "canonical") {
			t.Fatalf("editor preview: %d robots=%q cache=%q", resp.StatusCode, resp.Header.Get("X-Robots-Tag"), resp.Header.Get("Cache-Control"))
		}
	}
	preview()

	// A draft holding the shortcode does not publish the book; a published post does. The page
	// stays the editors' even then; the QR codes open, for a path of this site only.
	holder := wbrPost(t, srv, "post", "Catalog", "draft", 1)
	setContent := func(status string) {
		t.Helper()
		if _, err := srv.store.Pool.Exec(ctx, `UPDATE posts SET content = $2, status = $3 WHERE id = $1`, holder.ID,
			`<p>[pursae_flipbook id="`+strconv.Itoa(adminBook)+`"]</p>`, status); err != nil {
			t.Fatal(err)
		}
		flipbookMisses.Delete(adminBook)
		srv.purgeAll(ctx) // saving a post in the admin does this
	}
	setContent("draft")
	if code, _, _ := get(t, http.DefaultClient, bookURL+"/qr.svg"+qrQuery); code != http.StatusNotFound {
		t.Errorf("a draft published the book: %d", code)
	}
	setContent("publish")
	if code, body, _ := get(t, http.DefaultClient, bookURL+"/qr.svg"+qrQuery+"&p=3"); code != http.StatusOK || !strings.HasPrefix(body, "<svg") {
		t.Errorf("qr.svg: %d", code)
	}
	if code, body, _ := get(t, http.DefaultClient, bookURL+"/qr.png"+qrQuery); code != http.StatusOK || !strings.HasPrefix(body, "\x89PNG") {
		t.Errorf("qr.png: %d", code)
	}
	if b, _ := srv.flipbookGet(adminBook); !b.Published {
		t.Error("the book was not marked published")
	}
	for _, bad := range []string{"", "?u=" + url.QueryEscape("https://evil.example/"), "?u=" + url.QueryEscape("//evil.example/"), "?u=catalog"} {
		if code, _, _ := get(t, http.DefaultClient, bookURL+"/qr.svg"+bad); code != http.StatusBadRequest {
			t.Errorf("qr.svg%s: %d, want 400", bad, code)
		}
	}
	if code, _, _ := get(t, http.DefaultClient, bookURL+"/qr.gif"+qrQuery); code != http.StatusNotFound {
		t.Errorf("qr.gif: %d", code)
	}
	for _, cl := range []*http.Client{http.DefaultClient, ac} {
		if code, _, _ := get(t, cl, bookURL); code != http.StatusNotFound {
			t.Errorf("a published book's preview answered someone who cannot edit it: %d", code)
		}
	}
	preview()

	// Other /flipbook/ addresses go to the site; while the plugin is off, so does the preview.
	if code, _, _ := get(t, http.DefaultClient, base+"/flipbook/999/"); code != http.StatusNotFound {
		t.Errorf("missing book: %d", code)
	}
	if err := srv.settings.Set(ctx, map[string]any{flipbookSetting: false}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := get(t, c, bookURL); code != http.StatusNotFound {
		t.Errorf("preview while off: %d", code)
	}
	if err := srv.settings.Set(ctx, map[string]any{flipbookSetting: true}); err != nil {
		t.Fatal(err)
	}
	parent := wbrPost(t, srv, "page", "Flipbook", "publish", 1)
	child := wbrPost(t, srv, "page", "Guide", "publish", 1)
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE posts SET slug = 'flipbook' WHERE id = $1`, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE posts SET slug = 'guide', parent_id = $2 WHERE id = $1`, child.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	srv.purgeAll(ctx)
	code, body, _ := get(t, http.DefaultClient, base+"/flipbook/guide/")
	if code != http.StatusOK || !strings.Contains(body, "Guide") {
		t.Errorf("child page under /flipbook/: %d", code)
	}
}

func TestFlipbookEmbedHTMLEscapes(t *testing.T) {
	b := flipbookNew(`"><script>x</script>`, "/wp-content/uploads/a.pdf", "a.pdf", 3, 1)
	b.ID = 7
	out := flipbookEmbedHTML(b, false)
	if strings.Contains(out, "<script>x") {
		t.Fatalf("title not escaped: %s", out)
	}
	for _, want := range []string{`data-id="7"`, `data-pdf="/wp-content/uploads/a.pdf"`, `data-share="1"`, `data-qr="/flipbook/7/qr"`, `data-download="1"`, `data-file="a.pdf"`, "--pfb-height:640px", "alignwide", "fc-loading"} {
		if !strings.Contains(out, want) {
			t.Errorf("embed lacks %s", want)
		}
	}
	if strings.Contains(out, "/flipbook/7\"") {
		t.Errorf("the embed links to the book's own page: %s", out)
	}
	if full := flipbookEmbedHTML(b, true); strings.Contains(full, "data-share") || strings.Contains(full, "--pfb-height") || !strings.Contains(full, "pfb--full") {
		t.Errorf("full page markup: %s", full)
	}
	b.Share = false
	if out := flipbookEmbedHTML(b, false); strings.Contains(out, "data-share") || strings.Contains(out, "data-qr") {
		t.Errorf("share off still offers the link: %s", out)
	}
	for in, want := range map[string]string{`[pursae_flipbook id="7"]`: "7", `[pursae_flipbook height="500" id=&#8220;12&#8221;]`: "12", `[pursae_flipbook id=3]`: "3", `[pursae_flipbook_x id="7"]`: ""} {
		got := ""
		if m := flipbookShortcodeIDRe.FindStringSubmatch(in); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("shortcode id of %s = %q, want %q", in, got, want)
		}
	}
	if flipbookShortcodeText(7) != `[pursae_flipbook id="7"]` {
		t.Error("shortcode text")
	}
}
