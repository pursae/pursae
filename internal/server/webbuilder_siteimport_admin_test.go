package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"wpgo/internal/gocdn/s3fake"
	"wpgo/internal/store"
)

// --- helpers ----------------------------------------------------------------------------------

type wizEntry struct {
	name string
	body string
	mode os.FileMode // 0: a plain file
}

func wizZipBytes(t *testing.T, entries []wizEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(e.body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// wizSite is the fixture: the static site of TestMCPImportSite, in a top folder, with junk files.
func wizSite(t *testing.T) []byte {
	t.Helper()
	body := func(s string) string { return s }
	e := []wizEntry{
		{"site/css/site.css", "@media (min-width: 768px){.nav.on{color:red}} @media (max-width: 1023px){.x{}}", 0},
		{"site/js/main.js", "document.documentElement.classList.remove('no-js')", 0},
		{"site/index.html", siteTestPage("index.html", "Home", body(`<section id="hero"><h1>Welcome</h1></section><section id="more"><h2>More</h2></section>`)), 0},
		{"site/iletisim.html", siteTestPage("iletisim.html", "Contact", `<section><h1>Contact</h1></section>`), 0},
		{"site/hizmet/1.html", siteTestPage("../hizmet/1.html", "One", `<section><h1>One</h1><a href="2.html">next</a></section>`), 0},
		{"site/hizmet/2.html", siteTestPage("../hizmet/2.html", "Two", `<section><h1>Two</h1><a href="../index.html">home</a></section>`), 0},
		{"site/404.html", siteTestPage("404.html", "Not found", `<section><h1>Lost</h1></section>`), 0},
		{"site/.DS_Store", "junk", 0},
		{"__MACOSX/site/._index.html", "junk", 0},
	}
	return wizZipBytes(t, e)
}

type wizClient struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newWizClient(t *testing.T, base string, w *wbrClient) *wizClient {
	return &wizClient{t: t, base: base, c: &http.Client{Jar: w.c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (w *wizClient) do(req *http.Request) (int, string, string) {
	w.t.Helper()
	resp, err := w.c.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header.Get("Location")
}

func (w *wizClient) get(path string) (int, string, string) {
	w.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, w.base+path, nil)
	return w.do(req)
}

func (w *wizClient) postForm(path string, form url.Values) (int, string, string) {
	w.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, w.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return w.do(req)
}

var (
	wizNonceRE  = regexp.MustCompile(`action=upload&amp;_wpnonce=([0-9a-zA-Z]+)`)
	wizHiddenRE = regexp.MustCompile(`name="_wpnonce" value="([0-9a-zA-Z]+)"`)
	wizIDRE     = regexp.MustCompile(`import=([0-9a-f]{32})`)
)

// upload sends a zip to the screen with the nonce of the start page.
func (w *wizClient) upload(zipBytes []byte, name, kit string) (int, string, string) {
	w.t.Helper()
	_, page, _ := w.get("/wp-admin/web-builder-import")
	m := wizNonceRE.FindStringSubmatch(page)
	if m == nil {
		w.t.Fatalf("no upload nonce on the start page: %.300s", page)
	}
	return w.uploadWith(m[1], zipBytes, name, kit)
}

func (w *wizClient) uploadWith(nonce string, zipBytes []byte, name, kit string) (int, string, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("kit", kit)
	fw, _ := mw.CreateFormFile("archive", name)
	_, _ = fw.Write(zipBytes)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, w.base+"/wp-admin/web-builder-import?action=upload&_wpnonce="+nonce, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return w.do(req)
}

// --- the zip --------------------------------------------------------------------------------

func TestWBImportZipValidation(t *testing.T) {
	small := wbZipLimits{Entries: 100, Files: 3, Total: 200, File: 100, Media: 150, Page: 100, Read: 1 << 20}
	page := func(refs ...string) string {
		var b strings.Builder
		b.WriteString("<html><body>")
		for _, r := range refs {
			b.WriteString(`<img src="` + r + `">`)
		}
		return b.String() + "</body></html>"
	}
	cases := []struct {
		name    string
		entries []wizEntry
		lim     wbZipLimits
		want    string // "" = accepted
	}{
		{"slip", []wizEntry{{"index.html", "x", 0}, {"../evil.css", "x", 0}}, wbZipDefaultLimits, "leaves its folder"},
		{"slip deep", []wizEntry{{"a/../../evil.css", "x", 0}}, wbZipDefaultLimits, "leaves its folder"},
		{"slip backslash", []wizEntry{{`a\..\..\evil.css`, "x", 0}}, wbZipDefaultLimits, "leaves its folder"},
		{"absolute", []wizEntry{{"/etc/x.css", "x", 0}}, wbZipDefaultLimits, "absolute"},
		{"drive", []wizEntry{{"C:/x.css", "x", 0}}, wbZipDefaultLimits, "absolute"},
		{"symlink", []wizEntry{{"index.html", "x", 0}, {"link.css", "/etc/passwd", os.ModeSymlink | 0o777}}, wbZipDefaultLimits, "symbolic link"},
		{"symlink in junk", []wizEntry{{".git/HEAD", "/etc/passwd", os.ModeSymlink | 0o777}}, wbZipDefaultLimits, "symbolic link"},
		{"case twins", []wizEntry{{"index.html", "x", 0}, {"a.css", "x", 0}, {"A.css", "x", 0}}, wbZipDefaultLimits, "letter case"},
		{"too many entries", []wizEntry{{"a.html", "x", 0}, {"b.css", "x", 0}, {"c.css", "x", 0}}, wbZipLimits{Entries: 2, Files: 3, Total: 200, File: 100, Media: 150, Page: 100, Read: 1 << 20}, "too many entries"},
		{"too many files", []wizEntry{{"a.html", page("b.css", "c.css", "d.css", "e.css"), 0}, {"b.css", "x", 0}, {"c.css", "x", 0}, {"d.css", "x", 0}, {"e.css", "x", 0}}, small, "at most 3 files"},
		{"page too large", []wizEntry{{"a.html", strings.Repeat("x", 101), 0}}, small, "larger than"},
		{"total too large", []wizEntry{{"a.html", page("b.png", "c.png", "d.png"), 0}, {"b.png", strings.Repeat("x", 90), 0}, {"c.png", strings.Repeat("x", 90), 0}, {"d.png", strings.Repeat("x", 90), 0}}, small, "at most 3 files and 0 MB"},
		{"zip bomb", []wizEntry{{"a.html", page("b.css"), 0}, {"b.css", strings.Repeat("x", 90), 0}}, wbZipLimits{Entries: 10, Files: 3, Total: 200, File: 100, Media: 150, Page: 100, Read: 60}, "unpacks to more"},
		{"empty", []wizEntry{{".DS_Store", "x", 0}}, wbZipDefaultLimits, "no files"},
		{"ok", []wizEntry{{"index.html", page("css/a.css"), 0}, {"css/a.css", "x", 0}, {".DS_Store", "x", 0}, {"__MACOSX/a", "x", 0}}, wbZipDefaultLimits, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			zp := filepath.Join(dir, "in.zip")
			if err := os.WriteFile(zp, wizZipBytes(t, tc.entries), 0o600); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, "out")
			_ = os.MkdirAll(dst, 0o755)
			ex, err := wbZipExtract(zp, dst, tc.lim)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if len(ex.HTML) != 1 || len(ex.Assets) != 1 {
					t.Errorf("unpacked %v %v", ex.HTML, ex.Assets)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			// nothing escaped the folder
			if _, err := os.Stat(filepath.Join(dir, "evil.css")); err == nil {
				t.Errorf("a file was written outside the folder")
			}
		})
	}
	// Only what the pages use is unpacked: a CSS file's font and image, a script's string literal,
	// a srcset candidate; unused files, files a kit cannot hold (shell.php, "my pic.png") are left
	// out and listed; a used file too large for a kit but not for the Media Library is unpacked for
	// it, one larger than that is left out.
	t.Run("referenced files only", func(t *testing.T) {
		dir := t.TempDir()
		zp := filepath.Join(dir, "in.zip")
		idx := `<html><head><link rel="stylesheet" href="css/site.css"><script src="js/app.js"></script></head><body>` +
			`<img src="img/a.png" srcset="img/a-2x.png 2x"><video><source data-src="media/film.mp4"></video><img src="media/huge.mp4">` +
			`<a href="shell.php">x</a><img src="my%20pic.png"></body></html>`
		_ = os.WriteFile(zp, wizZipBytes(t, []wizEntry{{"index.html", idx, 0}, {"css/site.css", `@font-face{src:url(../fonts/x.woff2)} .a{background:url("../img/bg.webp")}`, 0},
			{"fonts/x.woff2", "f", 0}, {"img/bg.webp", "b", 0}, {"js/app.js", `fetch("data/menu.json")`, 0}, {"data/menu.json", `{"icon":"img/icon.svg"}`, 0},
			{"img/icon.svg", "<svg/>", 0}, {"img/a.png", "a", 0}, {"img/a-2x.png", "a", 0}, {"img/unused-1536x864.png", strings.Repeat("u", 50), 0},
			{"media/film.mp4", strings.Repeat("v", 120), 0}, {"media/huge.mp4", strings.Repeat("v", 160), 0}, {"shell.php", "<?php", 0}, {"my pic.png", "p", 0},
			{"wp-includes/blocks/x.css", "z", 0}}), 0o600)
		ex, err := wbZipExtract(zp, dir, wbZipLimits{Entries: 100, Files: 20, Total: 1000, File: 100, Media: 150, Page: 1000, Read: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		var kit []string
		for p := range ex.Assets {
			kit = append(kit, p)
		}
		sort.Strings(kit)
		if strings.Join(kit, " ") != "css/site.css data/menu.json fonts/x.woff2 img/a-2x.png img/a.png img/bg.webp img/icon.svg js/app.js" {
			t.Errorf("kit: %v", kit)
		}
		if len(ex.Media) != 1 || ex.Media["media/film.mp4"] != 120 {
			t.Errorf("media: %v", ex.Media)
		}
		if strings.Join(ex.TooLarge, ",") != "media/huge.mp4 (160 B)" || strings.Join(ex.Unkeepable, ",") != "my pic.png,shell.php" {
			t.Errorf("too large %v, unkeepable %v", ex.TooLarge, ex.Unkeepable)
		}
		if len(ex.Skipped) != 2 || !strings.HasPrefix(ex.Skipped[0], "img/unused-1536x864.png") || ex.SkippedBytes != 51 {
			t.Errorf("skipped: %v %d", ex.Skipped, ex.SkippedBytes)
		}
		for _, p := range []string{"img/unused-1536x864.png", "shell.php", "media/huge.mp4"} {
			if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
				t.Errorf("%s was unpacked", p)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
			t.Errorf("the page was not unpacked: %v", err)
		}
	})
	t.Run("top folders are stripped", func(t *testing.T) {
		dir := t.TempDir()
		zp := filepath.Join(dir, "in.zip")
		_ = os.WriteFile(zp, wizZipBytes(t, []wizEntry{{"site/html/index.html", `<link rel="stylesheet" href="css/a.css">`, 0}, {"site/html/css/a.css", "x", 0}}), 0o600)
		ex, err := wbZipExtract(zp, dir, wbZipDefaultLimits)
		if err != nil || ex.HTML["index.html"] == 0 || ex.Assets["css/a.css"] == 0 {
			t.Fatalf("%v %v %v", err, ex.HTML, ex.Assets)
		}
	})
	t.Run("not a zip", func(t *testing.T) {
		dir := t.TempDir()
		zp := filepath.Join(dir, "in.zip")
		_ = os.WriteFile(zp, []byte("hello"), 0o600)
		if _, err := wbZipExtract(zp, dir, wbZipDefaultLimits); err == nil || !strings.Contains(err.Error(), "not a readable zip") {
			t.Fatalf("%v", err)
		}
	})
}

// --- the screen ---------------------------------------------------------------------------------

func wizStart(t *testing.T) (*Server, string, *wbrClient, *wizClient) {
	t.Helper()
	srv, base, admin := wbrStart(t)
	return srv, base, admin, newWizClient(t, base, admin)
}

func TestWBImportWizardPublishesCollectionDrafts(t *testing.T) {
	srv, _, _, w := wizStart(t)
	site := wizZipBytes(t, []wizEntry{
		{"css/site.css", "body{color:green}", 0},
		{"index.html", bindTestPage("index.html", "Home", "<section><h1>Home</h1></section>"), 0},
		{"haber/a.html", bindNews("a", "Alpha", "<p>Alpha is the first story with more than a title.</p>"), 0},
		{"haber/b.html", bindNews("b", "Beta", "<p>Beta is the second story with its distinct body text.</p>"), 0},
		{"haber/c.html", bindNews("c", "Gamma", "<p>Gamma is the third story with its own distinct facts.</p>"), 0},
	})
	code, _, loc := w.upload(site, "site.zip", "wizard-draft-collection")
	match := wizIDRE.FindStringSubmatch(loc)
	if code != http.StatusFound || len(match) != 2 {
		t.Fatalf("upload: %d %q", code, loc)
	}
	id := match[1]
	_, analysis, _ := w.get(loc)
	code, _, loc = w.postForm(wbWizURL+"?action=import&import="+id, url.Values{
		"_wpnonce": {wizHiddenRE.FindStringSubmatch(analysis)[1]}, "kit": {"wizard-draft-collection"},
		"status": {"draft"}, "templates": {"1"}, "collection": {"haber"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("import drafts: %d", code)
	}
	_, report, _ := w.get(loc)
	if !strings.Contains(report, "3 pages are content under single template") || !strings.Contains(report, "<code>haber/b.html</code>") {
		t.Fatalf("collection is missing from the report: %s", wbSnippet(report, "Content"))
	}
	code, report, _ = w.postForm(wbWizURL+"?action=publish&import="+id,
		url.Values{"_wpnonce": {wizHiddenRE.FindStringSubmatch(report)[1]}})
	if code != http.StatusOK || strings.Contains(report, "Publish the pages") {
		t.Fatalf("publish staged collection: %d %s", code, wbSnippet(report, "notice"))
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE type='page' AND status='publish'
		AND meta->>$1='wizard-draft-collection' AND meta->>$2 LIKE 'haber/%.html'`, wbSiteMetaKit, wbSiteMetaKey); n != 3 {
		t.Fatalf("published collection members = %d, want 3", n)
	}
}

func TestWBImportWizardPermissions(t *testing.T) {
	srv, base, admin, w := wizStart(t)
	code, page, _ := w.get("/wp-admin/web-builder-import")
	if code != http.StatusOK || !strings.Contains(page, "Import HTML site") || !strings.Contains(page, `name="archive"`) {
		t.Fatalf("admin: %d %.300s", code, page)
	}
	if _, dash, _ := w.get("/wp-admin/"); !strings.Contains(dash, ">Import HTML site<") {
		t.Errorf("the menu has no Import HTML site entry")
	}
	_ = admin
	for _, role := range []string{"editor", "author", "subscriber"} {
		sub := newWizClient(t, base, wbrUser(t, srv, base, "wiz"+role, role))
		if code, _, _ := sub.get("/wp-admin/web-builder-import"); code != http.StatusForbidden && !(role == "subscriber" && code == http.StatusFound) {
			t.Errorf("%s GET: %d", role, code)
		}
		if code, _, _ := sub.uploadWith(wizNonceRE.FindStringSubmatch(page)[1], wizSite(t), "site.zip", ""); code != http.StatusForbidden {
			t.Errorf("%s upload: %d", role, code)
		}
		if _, dash, _ := sub.get("/wp-admin/"); strings.Contains(dash, ">Import HTML site<") {
			t.Errorf("%s sees the menu entry", role)
		}
	}
	// no nonce, wrong nonce
	if code, _, _ := w.uploadWith("bogus", wizSite(t), "site.zip", ""); code != http.StatusForbidden {
		t.Errorf("bad nonce: %d", code)
	}
	if code, _, _ := w.postForm("/wp-admin/web-builder-import?action=import&import="+strings.Repeat("a", 32), url.Values{"_wpnonce": {"x"}}); code != http.StatusForbidden {
		t.Errorf("import with a bad nonce: %d", code)
	}
	// anonymous
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := anon.Get(base + "/wp-admin/web-builder-import")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.Contains(resp.Header.Get("Location"), "wp-login") {
		t.Errorf("anonymous: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestWBImportWizardUploadRejects(t *testing.T) {
	srv, _, _, w := wizStart(t)
	for _, tc := range []struct {
		name    string
		entries []wizEntry
		file    string
		want    string
	}{
		{"zip slip", []wizEntry{{"index.html", "<p>x</p>", 0}, {"../../evil.css", "x", 0}}, "a.zip", "leaves its folder"},
		{"symlink", []wizEntry{{"index.html", "<p>x</p>", 0}, {"l.css", "/etc/passwd", os.ModeSymlink | 0o777}}, "a.zip", "symbolic link"},
		{"page name", []wizEntry{{"index.html", "<p>x</p>", 0}, {"a|b.html", "<p>x</p>", 0}}, "a.zip", "cannot be imported"},
		{"no html", []wizEntry{{"a.css", "x", 0}}, "a.zip", "no HTML pages"},
		{"not zip name", []wizEntry{{"index.html", "x", 0}}, "a.txt", "must be a .zip"},
	} {
		code, body, loc := w.upload(wizZipBytes(t, tc.entries), tc.file, "")
		if code != http.StatusOK || !strings.Contains(body, tc.want) || loc != "" {
			t.Errorf("%s: %d %q", tc.name, code, strings.TrimSpace(wbSnippet(body, "notice")))
		}
	}
	// a failed upload leaves no staging folder behind
	if ents, _ := os.ReadDir(srv.wbWizBase()); len(ents) != 0 {
		t.Errorf("staging folders left behind: %d", len(ents))
	}
	if code, body, _ := w.upload([]byte("not a zip at all"), "a.zip", ""); code != http.StatusOK || !strings.Contains(body, "not a readable zip") {
		t.Errorf("garbage: %d", code)
	}
	// Files no page uses (a server script left in the folder) are left out and listed, not refused.
	code, _, loc := w.upload(wizZipBytes(t, []wizEntry{{"index.html", "<p>x</p>", 0}, {"x.php", "<?php", 0}}), "a.zip", "")
	if code != http.StatusFound || loc == "" {
		t.Fatalf("an unused x.php: %d", code)
	}
	if _, body, _ := w.get(loc); !strings.Contains(body, `id="wb-import-skipped"`) || !strings.Contains(body, "x.php") {
		t.Errorf("the analysis does not list the unused file: %s", wbSnippet(body, "wb-import-skipped"))
	}
}

func wizCount(t *testing.T, srv *Server, q string, args ...any) int {
	t.Helper()
	var n int
	if err := srv.store.Pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The whole flow: upload, dry-run analysis (nothing written), import, compare, serving the design
// sandboxed, undo.
func TestWBImportWizardFlow(t *testing.T) {
	srv, base, _, w := wizStart(t)
	ctx := context.Background()

	// A page of the site's own that the import must never touch, and the front page before it.
	own := wbrPost(t, srv, "page", "Mine", "publish", 1)

	code, _, loc := w.upload(wizSite(t), "Demo Site.zip", "")
	if code != http.StatusFound || !wizIDRE.MatchString(loc) {
		t.Fatalf("upload: %d %q", code, loc)
	}
	id := wizIDRE.FindStringSubmatch(loc)[1]

	// Analysis: the pages, the proposed addresses, the clusters, the breakpoints; no writes.
	code, page, _ := w.get("/wp-admin/web-builder-import?import=" + id)
	if code != http.StatusOK {
		t.Fatalf("analysis: %d", code)
	}
	for _, want := range []string{"<code>index.html</code>", "<code>/hizmet/1/</code>", "<code>/iletisim/</code>", "404 template", "Header #1", "Footer #1",
		"<code>768px</code>", "<code>1023px</code>", `value="demo-site"`, "Draft parent pages", "<code>/hizmet/</code>", "Import the site"} {
		if !strings.Contains(page, want) {
			t.Errorf("analysis lacks %q", want)
		}
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE meta ? $1`, wbSiteMetaKit); n != 0 {
		t.Errorf("the analysis wrote %d posts", n)
	}
	if len(srv.wbKitList()) != 0 {
		t.Errorf("the analysis wrote a kit")
	}
	nonce := wizHiddenRE.FindStringSubmatch(page)[1]

	// Import with the options: templates on, front page on, published.
	code, _, loc = w.postForm("/wp-admin/web-builder-import?action=import&import="+id, url.Values{
		"_wpnonce": {nonce}, "kit": {"demo-site"}, "name": {"Demo"}, "templates": {"1"}, "front": {"1"}, "status": {"publish"}})
	if code != http.StatusSeeOther || !strings.Contains(loc, "action=result") {
		t.Fatalf("import: %d %q", code, loc)
	}
	code, result, _ := w.get(loc)
	if code != http.StatusOK || !strings.Contains(result, "Import report") || strings.Contains(result, "The import stopped") {
		t.Fatalf("result: %d %s", code, wbSnippet(result, "notice"))
	}
	for _, want := range []string{"<code>index.html</code>", "<code>hizmet/2.html</code>", "Compare with the design", "Remove this import"} {
		if !strings.Contains(result, want) {
			t.Errorf("result lacks %q", want)
		}
	}
	if k := srv.wbKitList()["demo-site"]; k == nil || k.Files != 2 || k.Name != "Demo" {
		t.Fatalf("kit: %+v", k)
	}
	if _, err := os.Stat(filepath.Join(srv.wbKitRoot(), "demo-site", "css", "site.css")); err != nil {
		t.Errorf("kit file: %v", err)
	}
	// 4 pages + 1 draft parent ("hizmet"), 3 templates (header, footer, 404).
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE type='page' AND meta->>$1 = 'demo-site' AND status = 'publish'`, wbSiteMetaKit); n != 4 {
		t.Errorf("published pages: %d", n)
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE type='page' AND meta->>$1 = 'demo-site' AND status = 'draft'`, wbSiteMetaKit); n != 1 {
		t.Errorf("placeholder parents: %d", n)
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE type = 'wb_template' AND meta->>$1 = 'demo-site'`, wbSiteMetaKit); n != 3 {
		t.Errorf("templates: %d", n)
	}
	if srv.settings.String("show_on_front") != "page" {
		t.Errorf("front page not set")
	}
	resp, err := http.Get(base + "/iletisim/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "<h1>Contact</h1>") {
		t.Errorf("/iletisim/: %d", resp.StatusCode)
	}
	recs := srv.wbWizRecords()
	if r := recs[id]; r == nil || len(r.Pages) != 5 || len(r.Templates) != 3 || !r.KitCreated || !r.FrontSet {
		t.Fatalf("record: %+v", r)
	}

	// Compare: the design in a sandboxed frame beside the Pursae page.
	code, cmp, _ := w.get("/wp-admin/web-builder-import?action=compare&import=" + id + "&file=iletisim.html")
	if code != http.StatusOK || !strings.Contains(cmp, `sandbox="allow-scripts allow-forms allow-popups"`) || !strings.Contains(cmp, `src="/wb-design-preview/`+id+`/iletisim.html"`) ||
		!strings.Contains(cmp, `id="wb-import-pursae"`) || !strings.Contains(cmp, `src="/iletisim/"`) || strings.Contains(cmp, "allow-same-origin") {
		t.Errorf("compare: %d %s", code, wbSnippet(cmp, "wb-import-design"))
	}

	// Serving the design: no login needed, sandboxed, never as the admin origin.
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	fetch := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := anon.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := fetch(wbWizDesignRoute + id + "/iletisim.html")
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(body, "<h1>Contact</h1>") || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.HasPrefix(csp, "sandbox allow-scripts allow-forms allow-popups;") || strings.Contains(csp, "allow-same-origin") || strings.Contains(csp, "allow-top-navigation") ||
		!strings.Contains(csp, "form-action 'none'") || resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" ||
		resp.Header.Get("Cache-Control") != "private, no-store" || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("design html: %d %v %q", resp.StatusCode, resp.Header, csp)
	}
	if resp, body := fetch(wbWizDesignRoute + id + "/css/site.css"); resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") ||
		!strings.Contains(body, "@media") || !strings.HasPrefix(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("design css: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := fetch(wbWizDesignRoute + id + "/"); resp.StatusCode != 200 {
		t.Errorf("design index: %d", resp.StatusCode)
	}
	for _, p := range []string{id + "/..%2fmeta.json", id + "/../meta.json", id + "/%2e%2e/%2e%2e/meta.json", id + "/nope.html", strings.Repeat("0", 32) + "/index.html", "short/index.html"} {
		if resp, body := fetch(wbWizDesignRoute + p); resp.StatusCode == 200 || strings.Contains(body, `"owner"`) {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(srv.wbWizDir(id), "meta.json")); err != nil {
		t.Fatal(err)
	}

	// Remove: a confirmation first, then only what the import made.
	code, undo, _ := w.get("/wp-admin/web-builder-import?action=undo&import=" + id)
	if code != http.StatusOK || !strings.Contains(undo, "Delete permanently") || !strings.Contains(undo, "Demo") && !strings.Contains(undo, "Iletisim") && !strings.Contains(undo, "Contact") {
		t.Fatalf("undo page: %d", code)
	}
	if strings.Contains(undo, ">Mine<") {
		t.Errorf("the undo page lists a page the import did not make")
	}
	un := wizHiddenRE.FindStringSubmatch(undo)[1]
	// without the confirmation field nothing is deleted
	if code, _, _ := w.postForm("/wp-admin/web-builder-import?action=undo&import="+id, url.Values{"_wpnonce": {un}}); code != http.StatusOK {
		t.Errorf("unconfirmed undo: %d", code)
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE meta->>$1 = 'demo-site'`, wbSiteMetaKit); n != 8 {
		t.Fatalf("an unconfirmed undo deleted posts: %d left", n)
	}
	code, done, _ := w.postForm("/wp-admin/web-builder-import?action=undo&import="+id, url.Values{"_wpnonce": {un}, "confirm": {"1"}})
	if code != http.StatusOK || !strings.Contains(done, "Removed 8 pages and templates and its design kit") {
		t.Fatalf("undo: %d %s", code, wbSnippet(done, "notice"))
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE meta->>$1 = 'demo-site'`, wbSiteMetaKit); n != 0 {
		t.Errorf("%d posts left", n)
	}
	if _, err := srv.store.GetPost(ctx, own.ID); err != nil {
		t.Errorf("the site's own page was deleted: %v", err)
	}
	if srv.wbKitList()["demo-site"] != nil {
		t.Errorf("kit left")
	}
	if _, err := os.Stat(filepath.Join(srv.wbKitRoot(), "demo-site")); err == nil {
		t.Errorf("kit files left")
	}
	if srv.settings.String("show_on_front") == "page" {
		t.Errorf("the front page setting was not restored: %q", srv.settings.String("show_on_front"))
	}
	if len(srv.wbWizRecords()) != 0 {
		t.Errorf("record left")
	}
	if _, err := os.Stat(srv.wbWizDir(id)); err == nil {
		t.Errorf("the upload was kept after its import was removed")
	}
	if code, _, _ := w.get("/wp-admin/web-builder-import?action=undo&import=" + id); code != http.StatusOK {
		t.Errorf("undo of a removed import: %d", code)
	}
}

// An upload and its design preview expire a day after the upload: on access, with the screen's
// sweep and with the hourly wb_import_cleanup event. An import's record outlives its upload: its
// "Remove this import" page still opens.
func TestWBImportWizardExpiry(t *testing.T) {
	srv, base, _, w := wizStart(t)
	upload := func() string {
		t.Helper()
		code, _, loc := w.upload(wizSite(t), "Demo Site.zip", "")
		if code != http.StatusFound || !wizIDRE.MatchString(loc) {
			t.Fatalf("upload: %d %q", code, loc)
		}
		return wizIDRE.FindStringSubmatch(loc)[1]
	}
	age := func(id string, d time.Duration) {
		t.Helper()
		m := srv.wbWizLoad(id)
		if m == nil {
			t.Fatalf("no session %s", id)
		}
		m.Created = wbTime(time.Now().Add(-d))
		if err := srv.wbWizSave(m); err != nil {
			t.Fatal(err)
		}
	}
	preview := func(id string) int {
		code, _, _ := wbsFetch(t, base+wbWizDesignRoute+id+"/index.html", nil)
		return code
	}
	// On access.
	id := upload()
	if code := preview(id); code != http.StatusOK {
		t.Fatalf("fresh preview: %d", code)
	}
	age(id, 23*time.Hour)
	if code := preview(id); code != http.StatusOK {
		t.Errorf("preview within a day: %d", code)
	}
	age(id, 25*time.Hour)
	if code := preview(id); code != http.StatusNotFound {
		t.Errorf("preview after a day: %d", code)
	}
	if _, err := os.Stat(srv.wbWizDir(id)); err == nil {
		t.Error("an expired upload's folder is left")
	}
	// The screen's sweep and the hourly event.
	id1, id2 := upload(), upload()
	age(id1, 25*time.Hour)
	if code, _, _ := w.get("/wp-admin/web-builder-import"); code != http.StatusOK {
		t.Fatal("screen")
	}
	if _, err := os.Stat(srv.wbWizDir(id1)); err == nil {
		t.Error("the screen's sweep kept an expired upload")
	}
	if _, err := os.Stat(srv.wbWizDir(id2)); err != nil {
		t.Error("the screen's sweep removed a fresh upload")
	}
	age(id2, 25*time.Hour)
	if err := srv.jobHandlers()["wb_import_cleanup"](context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(srv.wbWizDir(id2)); err == nil {
		t.Error("wb_import_cleanup kept an expired upload")
	}
	// A record whose upload expired: its undo page still opens.
	recs := srv.wbWizRecords()
	recs[id2] = &wbWizRecord{ID: id2, Kit: "gone-kit", Pages: []int64{999999}, At: wbTime(time.Now())}
	if err := srv.wbWizSetRecords(wbsGin(), recs); err != nil {
		t.Fatal(err)
	}
	if code, page, _ := w.get("/wp-admin/web-builder-import?action=undo&import=" + id2); code != http.StatusOK || strings.Contains(page, "That upload is gone") {
		t.Errorf("undo page of an expired upload: %d %s", code, wbSnippet(page, "notice"))
	}
}

// Drafts, no templates, an existing kit that must be confirmed, publishing afterwards; and an undo
// that leaves a kit alone when the import did not create it.
func TestWBImportWizardOptions(t *testing.T) {
	srv, _, _, w := wizStart(t)
	_, _, loc := w.upload(wizSite(t), "site.zip", "mykit")
	id := wizIDRE.FindStringSubmatch(loc)[1]
	_, page, _ := w.get("/wp-admin/web-builder-import?import=" + id)
	if !strings.Contains(page, `value="mykit"`) {
		t.Errorf("the kit field does not hold the given slug")
	}
	nonce := wizHiddenRE.FindStringSubmatch(page)[1]
	form := func(extra url.Values) url.Values {
		v := url.Values{"_wpnonce": {nonce}, "kit": {"mykit"}, "name": {"My kit"}, "status": {"draft"}, "front": {"1"}}
		for k, x := range extra {
			v[k] = x
		}
		return v
	}
	// bad slug
	if code, body, _ := w.postForm("/wp-admin/web-builder-import?action=import&import="+id, form(url.Values{"kit": {"Bad Slug!"}})); code != http.StatusOK || !strings.Contains(body, "lowercase letters") {
		t.Errorf("bad slug: %d", code)
	}
	// drafts without templates
	code, _, loc := w.postForm("/wp-admin/web-builder-import?action=import&import="+id, form(nil))
	if code != http.StatusSeeOther {
		t.Fatalf("import: %d", code)
	}
	_, result, _ := w.get(loc)
	if !strings.Contains(result, "imported as drafts") || !strings.Contains(result, "Publish the pages") {
		t.Errorf("result: %s", wbSnippet(result, "Design kit"))
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE type='page' AND meta->>$1 = 'mykit' AND status = 'publish'`, wbSiteMetaKit); n != 0 {
		t.Errorf("drafts were published: %d", n)
	}
	if srv.settings.String("show_on_front") == "page" {
		t.Errorf("a draft became the front page")
	}
	// no header/footer templates: only the 404 template; pages keep layout canvas
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE type = 'wb_template' AND meta->>$1 = 'mykit'`, wbSiteMetaKit); n != 1 {
		t.Errorf("templates without the split: %d", n)
	}
	// publish them
	pn := wizHiddenRE.FindStringSubmatch(result)[1]
	code, pub, _ := w.postForm("/wp-admin/web-builder-import?action=publish&import="+id, url.Values{"_wpnonce": {pn}, "front": {"1"}})
	if code != http.StatusOK || strings.Contains(pub, "notice-error") {
		t.Fatalf("publish: %d %s", code, wbSnippet(pub, "notice"))
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE type='page' AND meta->>$1 = 'mykit' AND status = 'publish'`, wbSiteMetaKit); n != 4 {
		t.Errorf("published after publish: %d", n)
	}
	if srv.settings.String("show_on_front") != "page" {
		t.Errorf("front page not set after publishing")
	}
	// importing again onto the existing kit needs the confirmation
	_, _, loc2 := w.upload(wizSite(t), "site.zip", "mykit")
	id2 := wizIDRE.FindStringSubmatch(loc2)[1]
	_, page2, _ := w.get("/wp-admin/web-builder-import?import=" + id2)
	if !strings.Contains(page2, "A design kit with this slug exists") || !strings.Contains(page2, "Updates the page of an earlier import") {
		t.Errorf("second analysis: %s", wbSnippet(page2, "kit with this slug"))
	}
	n2 := wizHiddenRE.FindStringSubmatch(page2)[1]
	v := form(nil)
	v.Set("_wpnonce", n2)
	if code, body, _ := w.postForm("/wp-admin/web-builder-import?action=import&import="+id2, v); code != http.StatusOK || !strings.Contains(body, "tick the box") {
		t.Errorf("existing kit without the confirmation: %d", code)
	}
	v.Set("replace", "1")
	v.Set("status", "publish")
	v.Set("templates", "1")
	if code, _, loc := w.postForm("/wp-admin/web-builder-import?action=import&import="+id2, v); code != http.StatusSeeOther {
		t.Fatalf("re-import: %d %s", code, loc)
	}
	// the second import created only the templates (the pages were updated): undoing it keeps the
	// kit (the first import made it) and the pages of the first import.
	rec := srv.wbWizRecords()[id2]
	if rec == nil || rec.KitCreated || len(rec.Pages) != 0 || len(rec.Templates) != 2 {
		t.Fatalf("second record: %+v", rec)
	}
	// discard removes the staging folder only (the import stays, its undo works without it)
	dn := wizHiddenRE.FindStringSubmatch(page2)
	_, page3, _ := w.get("/wp-admin/web-builder-import?import=" + id2)
	dn = regexp.MustCompile(`action=discard[^"]*"[^>]*>\s*<input type="hidden" name="_wpnonce" value="([0-9a-zA-Z]+)"`).FindStringSubmatch(page3)
	if dn == nil {
		t.Fatalf("no discard form")
	}
	if code, _, _ := w.postForm("/wp-admin/web-builder-import?action=discard&import="+id2, url.Values{"_wpnonce": {dn[1]}}); code != http.StatusFound {
		t.Errorf("discard: %d", code)
	}
	if _, err := os.Stat(srv.wbWizDir(id2)); err == nil {
		t.Errorf("staging folder left after discard")
	}
	_, undo, _ := w.get("/wp-admin/web-builder-import?action=undo&import=" + id2)
	un := wizHiddenRE.FindStringSubmatch(undo)[1]
	if code, done, _ := w.postForm("/wp-admin/web-builder-import?action=undo&import="+id2, url.Values{"_wpnonce": {un}, "confirm": {"1"}}); code != http.StatusOK || !strings.Contains(done, "Removed 2 pages and templates.") {
		t.Fatalf("undo 2: %d %s", code, wbSnippet(done, "notice"))
	}
	if srv.wbKitList()["mykit"] == nil {
		t.Errorf("the kit of the first import was deleted")
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE type='page' AND meta->>$1 = 'mykit'`, wbSiteMetaKit); n != 5 {
		t.Errorf("pages of the first import: %d", n)
	}
}

// Removing the import that originally created a kit must keep the kit while a later import still
// uses it, and transfer cleanup ownership so removing the last import eventually removes the kit.
func TestWBImportWizardUndoTransfersSharedKitOwnership(t *testing.T) {
	srv, _, _, w := wizStart(t)
	importOne := func(split, replace bool) string {
		t.Helper()
		_, _, loc := w.upload(wizSite(t), "site.zip", "shared-kit")
		id := wizIDRE.FindStringSubmatch(loc)[1]
		_, page, _ := w.get("/wp-admin/web-builder-import?import=" + id)
		form := url.Values{
			"_wpnonce": {wizHiddenRE.FindStringSubmatch(page)[1]},
			"kit":      {"shared-kit"},
			"name":     {"Shared kit"},
			"status":   {"publish"},
		}
		if split {
			form.Set("templates", "1")
		}
		if replace {
			form.Set("replace", "1")
		}
		if code, body, _ := w.postForm("/wp-admin/web-builder-import?action=import&import="+id, form); code != http.StatusSeeOther {
			t.Fatalf("import %s: %d %s", id, code, wbSnippet(body, "notice"))
		}
		return id
	}
	undo := func(id string) {
		t.Helper()
		_, page, _ := w.get("/wp-admin/web-builder-import?action=undo&import=" + id)
		if code, body, _ := w.postForm("/wp-admin/web-builder-import?action=undo&import="+id, url.Values{
			"_wpnonce": {wizHiddenRE.FindStringSubmatch(page)[1]}, "confirm": {"1"},
		}); code != http.StatusOK {
			t.Fatalf("undo %s: %d %s", id, code, wbSnippet(body, "notice"))
		}
	}

	first := importOne(false, false)
	second := importOne(true, true)
	undo(first)
	kit := srv.wbKitList()["shared-kit"]
	if kit == nil || kit.Root == 0 {
		t.Fatalf("shared kit was disabled or deleted: %+v", kit)
	}
	if rec := srv.wbWizRecords()[second]; rec == nil || !rec.KitCreated {
		t.Fatalf("kit cleanup ownership was not transferred: %+v", rec)
	}
	undo(second)
	if kit := srv.wbKitList()["shared-kit"]; kit != nil {
		t.Fatalf("last import left its transferred kit behind: %+v", kit)
	}
}

// Undo never deletes a post that no longer carries the import's mark.
func TestWBImportWizardUndoKeepsOthers(t *testing.T) {
	srv, _, _, w := wizStart(t)
	_, _, loc := w.upload(wizSite(t), "site.zip", "k")
	id := wizIDRE.FindStringSubmatch(loc)[1]
	_, page, _ := w.get("/wp-admin/web-builder-import?import=" + id)
	nonce := wizHiddenRE.FindStringSubmatch(page)[1]
	if code, _, _ := w.postForm("/wp-admin/web-builder-import?action=import&import="+id, url.Values{"_wpnonce": {nonce}, "kit": {"k"}, "name": {"K"}, "templates": {"1"}, "status": {"publish"}}); code != http.StatusSeeOther {
		t.Fatalf("import: %d", code)
	}
	rec := srv.wbWizRecords()[id]
	if rec == nil || len(rec.Pages) == 0 {
		t.Fatalf("record: %+v", rec)
	}
	// the owner took one page over: its mark is gone
	if _, err := srv.store.Pool.Exec(context.Background(), `UPDATE posts SET meta = meta - $2 WHERE id = $1`, rec.Pages[0], wbSiteMetaKit); err != nil {
		t.Fatal(err)
	}
	_, undo, _ := w.get("/wp-admin/web-builder-import?action=undo&import=" + id)
	un := wizHiddenRE.FindStringSubmatch(undo)[1]
	_, done, _ := w.postForm("/wp-admin/web-builder-import?action=undo&import="+id, url.Values{"_wpnonce": {un}, "confirm": {"1"}})
	if !strings.Contains(done, "left alone") {
		t.Errorf("done: %s", wbSnippet(done, "notice"))
	}
	if _, err := srv.store.GetPost(context.Background(), rec.Pages[0]); err != nil {
		t.Errorf("a page without the mark was deleted: %v", err)
	}
}

func TestWBImportWizardUndoCanRetryAfterDeleteFailure(t *testing.T) {
	srv, _, _, w := wizStart(t)
	const kit = "retry-kit"
	code, _, loc := w.upload(wizSite(t), "site.zip", kit)
	if code != http.StatusFound || !wizIDRE.MatchString(loc) {
		t.Fatalf("upload: %d %q", code, loc)
	}
	id := wizIDRE.FindStringSubmatch(loc)[1]
	_, analysis, _ := w.get(wbWizURL + "?import=" + id)
	nonce := wizHiddenRE.FindStringSubmatch(analysis)
	if len(nonce) != 2 {
		t.Fatal("missing import form")
	}
	code, body, _ := w.postForm(wbWizURL+"?action=import&import="+id, url.Values{
		"_wpnonce": {nonce[1]}, "kit": {kit}, "status": {"publish"},
	})
	rec := srv.wbWizRecords()[id]
	if code != http.StatusSeeOther || rec == nil || len(rec.Pages) < 2 || srv.wbKitList()[kit] == nil {
		t.Fatalf("import: %d, record=%+v, %s", code, rec, wbSnippet(body, "notice"))
	}
	blocked := rec.Pages[1]
	const claim = "audit-held"
	if err := srv.wbSiteSetClaims(t.Context(), kit, map[string]int64{claim: blocked}); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := srv.store.Pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE undo_post_guard (post_id bigint REFERENCES posts(id))`)
	t.Cleanup(func() {
		_, _ = srv.store.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS undo_post_guard`)
	})
	exec(`INSERT INTO undo_post_guard (post_id) VALUES ($1)`, blocked)

	target := wbWizURL + "?action=undo&import=" + id
	undo := func() string {
		t.Helper()
		code, form, _ := w.get(target)
		nonce := wizHiddenRE.FindStringSubmatch(form)
		if code != http.StatusOK || len(nonce) != 2 {
			t.Fatalf("undo form: %d %s", code, wbSnippet(form, "notice"))
		}
		code, result, _ := w.postForm(target, url.Values{"_wpnonce": {nonce[1]}, "confirm": {"1"}})
		if code != http.StatusOK {
			t.Fatalf("undo: %d %s", code, wbSnippet(result, "notice"))
		}
		return result
	}
	if body := undo(); !strings.Contains(body, "Retry the removal") || !strings.Contains(body, "Delete permanently") {
		t.Fatalf("missing retry form: %s", wbSnippet(body, "notice"))
	}
	if srv.wbWizRecords()[id] == nil || srv.wbKitList()[kit] == nil || srv.wbSiteClaims()[claim].ID != blocked {
		t.Fatal("failed undo removed the record, kit or claim")
	}
	if _, err := os.Stat(srv.wbWizDir(id)); err != nil {
		t.Fatalf("failed undo removed the staged upload: %v", err)
	}
	exec(`DROP TABLE undo_post_guard`)
	if body := undo(); !strings.Contains(body, "and its design kit") {
		t.Fatalf("retry did not finish: %s", wbSnippet(body, "notice"))
	}
	if srv.wbWizRecords()[id] != nil || srv.wbKitList()[kit] != nil || srv.wbSiteClaims()[claim].ID != 0 {
		t.Fatal("completed undo left the record, kit or claim")
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE meta->>$1 = $2`, wbSiteMetaKit, kit); n != 0 {
		t.Fatalf("completed undo left %d posts", n)
	}
}

func TestWBImportWizardUndoCanRetryAfterMenuDeleteFailure(t *testing.T) {
	srv, _, _, w := wizStart(t)
	const kit = "navigation-import"
	menu, err := srv.store.CreateMenu(t.Context(), wbSiteMenuName(kit))
	if err != nil {
		t.Fatal(err)
	}
	id := wbWizNewID()
	rec := &wbWizRecord{ID: id, Kit: kit, Menu: menu}
	if err := srv.settings.Set(t.Context(), map[string]any{wbWizImportsOption: map[string]*wbWizRecord{id: rec}}); err != nil {
		t.Fatal(err)
	}
	staged := srv.wbWizDir(id)
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := srv.store.Pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE undo_menu_guard (menu_id bigint REFERENCES menus(id))`)
	t.Cleanup(func() {
		_, _ = srv.store.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS undo_menu_guard`)
	})
	exec(`INSERT INTO undo_menu_guard (menu_id) VALUES ($1)`, menu)

	undo := func() string {
		t.Helper()
		target := wbWizURL + "?action=undo&import=" + id
		code, form, _ := w.get(target)
		nonce := wizHiddenRE.FindStringSubmatch(form)
		if code != http.StatusOK || len(nonce) != 2 {
			t.Fatalf("undo form: %d %s", code, wbSnippet(form, "notice"))
		}
		code, body, _ := w.postForm(target, url.Values{"_wpnonce": {nonce[1]}, "confirm": {"1"}})
		if code != http.StatusOK {
			t.Fatalf("undo: %d %s", code, wbSnippet(body, "notice"))
		}
		return body
	}
	if body := undo(); !strings.Contains(body, "Retry the removal") || srv.wbWizRecords()[id] == nil {
		t.Fatalf("failed undo lost its retry: %s", wbSnippet(body, "notice"))
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staging after failure: %v", err)
	}
	exec(`DROP TABLE undo_menu_guard`)
	if body := undo(); !strings.Contains(body, "Removed 1 pages and templates") {
		t.Fatalf("retry did not finish: %s", wbSnippet(body, "notice"))
	}
	if n := wizCount(t, srv, `SELECT count(*) FROM menus WHERE id = $1`, menu); n != 0 || srv.wbWizRecords()[id] != nil {
		t.Fatal("retry left the menu or import record")
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging after successful retry: %v", err)
	}
}

// A file the pages use that is too large for a design kit (a hero video) goes to the Media Library:
// the analysis says so, the import puts it there and points the page at it, and "Remove this
// import" removes it again.
func TestWBImportWizardMediaLibrary(t *testing.T) {
	srv, base, _, w := wizStart(t)
	ctx := context.Background()
	saved := wbZipDefaultLimits
	wbZipDefaultLimits.File = 64 // a kit file: 64 bytes in this test
	t.Cleanup(func() { wbZipDefaultLimits = saved })
	page := `<!doctype html><html><head><title>Home</title><link rel="stylesheet" href="css/a.css"></head><body><main><section><h1>Home</h1>` +
		`<video class="bg" autoplay muted><source data-nectar-video-src="media/film.mp4" type="video/mp4"></video></section></main></body></html>`
	code, _, loc := w.upload(wizZipBytes(t, []wizEntry{{"index.html", page, 0}, {"css/a.css", "h1{color:red}", 0},
		{"media/film.mp4", strings.Repeat("v", 200), 0}, {"media/unused.mp4", strings.Repeat("u", 10), 0}}), "film.zip", "")
	if code != http.StatusFound || !wizIDRE.MatchString(loc) {
		t.Fatalf("upload: %d %q", code, loc)
	}
	id := wizIDRE.FindStringSubmatch(loc)[1]
	_, an, _ := w.get("/wp-admin/web-builder-import?import=" + id)
	if !strings.Contains(an, `id="wb-import-media"`) || !strings.Contains(an, "media/film.mp4") || !strings.Contains(an, "media/unused.mp4") {
		t.Fatalf("analysis: %s", wbSnippet(an, "wb-import-media"))
	}
	if strings.Contains(an, "<code>media/film.mp4</code>") { // never reported missing
		t.Errorf("the video is reported missing")
	}
	nonce := wizHiddenRE.FindStringSubmatch(an)[1]
	code, _, loc = w.postForm("/wp-admin/web-builder-import?action=import&import="+id, url.Values{
		"_wpnonce": {nonce}, "kit": {"film"}, "name": {"Film"}, "templates": {"1"}, "front": {"1"}, "status": {"publish"}})
	if code != http.StatusSeeOther {
		t.Fatalf("import: %d", code)
	}
	var mediaPath string
	if err := srv.store.Pool.QueryRow(ctx, `SELECT file_path FROM media ORDER BY id DESC LIMIT 1`).Scan(&mediaPath); err != nil {
		t.Fatalf("no media item: %v", err)
	}
	if !strings.HasSuffix(mediaPath, "film.mp4") {
		t.Fatalf("media item: %q", mediaPath)
	}
	if _, err := os.Stat(filepath.Join(srv.wbKitRoot(), "film", "media", "film.mp4")); err == nil {
		t.Errorf("the video went into the kit too")
	}
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `data-nectar-video-src="/wp-content/uploads/`+mediaPath+`"`) {
		t.Errorf("the page does not point at the Media Library: %s", wbSnippet(string(b), "nectar-video-src"))
	}
	rec := srv.wbWizRecords()[id]
	if rec == nil || len(rec.Media) != 1 {
		t.Fatalf("record: %+v", rec)
	}
	code, _, loc = w.postForm("/wp-admin/web-builder-import?action=import&import="+id, url.Values{
		"_wpnonce": {nonce}, "kit": {"film"}, "name": {"Film"}, "templates": {"1"}, "front": {"1"}, "status": {"publish"}, "replace": {"1"}})
	if code != http.StatusSeeOther {
		t.Fatalf("repeat import: %d", code)
	}
	_, report, _ := w.get(loc)
	var copies int
	err = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM media WHERE title = 'film'`).Scan(&copies)
	again := srv.wbWizRecords()[id]
	if err != nil || copies != 1 || again == nil || !slices.Equal(again.Media, rec.Media) ||
		!strings.Contains(report, "/wp-content/uploads/"+mediaPath) {
		t.Fatalf("repeat import: rows=%d prior=%+v current=%+v err=%v", copies, rec, again, err)
	}
	_, undo, _ := w.get("/wp-admin/web-builder-import?action=undo&import=" + id)
	if !strings.Contains(undo, "<h3>Media Library</h3>") || !strings.Contains(undo, "<li>film</li>") {
		t.Fatal("undo confirmation must disclose the imported attachment")
	}
	un := wizHiddenRE.FindStringSubmatch(undo)[1]
	if code, done, _ := w.postForm("/wp-admin/web-builder-import?action=undo&import="+id, url.Values{"_wpnonce": {un}, "confirm": {"1"}}); code != http.StatusOK || !strings.Contains(done, "Removed") {
		t.Fatalf("undo: %d", code)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.UploadsDir, filepath.FromSlash(mediaPath))); err == nil {
		t.Errorf("the media file is left after the undo")
	}
}

func TestWBImportWizardUndoKeepsFilesWhenMediaDeleteFails(t *testing.T) {
	srv, _, _, w := wizStart(t)
	const contents = "imported media bytes"
	a, err := srv.insertAttachmentFile(t.Context(), "imported.mp4", strings.NewReader(contents), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	fake := s3fake.New(gocdnTestKey, gocdnTestSecret, "auto", "media")
	t.Cleanup(fake.Close)
	gocdnAllowPrivate.Store(true) // the fake storage listens on 127.0.0.1
	t.Cleanup(func() { gocdnAllowPrivate.Store(false) })
	sub := filepath.ToSlash(filepath.Dir(a.FilePath))
	if sub == "." {
		sub = ""
	}
	reservedDuringDelete := make(chan bool, 1)
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			names := srv.uploadReserved(r.Context())(sub)
			reservedDuringDelete <- slices.Contains(names, filepath.Base(a.FilePath))
		}
		fake.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(storage.Close)
	settings := gocdnSettings{Provider: "minio", Endpoint: storage.URL, Region: "auto", Bucket: "media",
		AccessKey: gocdnTestKey, SecretKey: gocdnTestSecret, PathStyle: true}
	if err := srv.gocdnSave(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	const key = "imported-copy.mp4"
	fake.PutObject("media", key, []byte(contents), a.Mime)
	if err := srv.gocdnSaveItem(t.Context(), &gocdnItem{
		MediaID: a.ID, Path: a.FilePath, Provider: settings.Provider, Region: settings.Region,
		Bucket: settings.Bucket, Key: key, Endpoint: storage.URL, Size: a.FileSize, Mime: a.Mime,
	}); err != nil {
		t.Fatal(err)
	}
	id := wbWizNewID()
	rec := &wbWizRecord{ID: id, Kit: "test-import", Media: []int64{a.ID}}
	if err := srv.settings.Set(t.Context(), map[string]any{wbWizImportsOption: map[string]*wbWizRecord{id: rec}}); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := srv.store.Pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE undo_media_guard (media_id bigint REFERENCES media(id))`)
	t.Cleanup(func() {
		_, _ = srv.store.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS undo_media_guard`)
	})
	exec(`INSERT INTO undo_media_guard (media_id) VALUES ($1)`, a.ID)

	undo := func() string {
		t.Helper()
		target := wbWizURL + "?action=undo&import=" + id
		code, form, _ := w.get(target)
		nonce := wizHiddenRE.FindStringSubmatch(form)
		if code != http.StatusOK || len(nonce) != 2 {
			t.Fatalf("undo form: %d %s", code, wbSnippet(form, "notice"))
		}
		code, body, _ := w.postForm(target, url.Values{"_wpnonce": {nonce[1]}, "confirm": {"1"}})
		if code != http.StatusOK {
			t.Fatalf("undo: %d %s", code, wbSnippet(body, "notice"))
		}
		return body
	}
	if body := undo(); !strings.Contains(body, "Retry the removal") {
		t.Fatalf("missing retry notice: %s", wbSnippet(body, "notice"))
	}
	local := srv.gocdnLocalPath(a.FilePath)
	if data, err := os.ReadFile(local); err != nil || string(data) != contents {
		t.Fatalf("failed undo changed the local media: %q, %v", data, err)
	}
	if fake.Object("media", key) == nil || srv.gocdnItemByPath(t.Context(), a.FilePath) == nil || srv.wbWizRecords()[id] == nil {
		t.Fatal("failed undo removed the offloaded object, its mapping or the import record")
	}

	srv.gocdnRemoveLocal(t.Context(), a.ID)
	if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("could not make the fixture remote-only: %v", err)
	}
	exec(`DROP TABLE undo_media_guard`)
	if body := undo(); !strings.Contains(body, "Removed 1 pages and templates") {
		t.Fatalf("retry did not complete: %s", wbSnippet(body, "notice"))
	}
	if _, err := srv.store.GetAttachment(t.Context(), a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("attachment after retry: %v", err)
	}
	if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local media after retry: %v", err)
	}
	select {
	case reserved := <-reservedDuringDelete:
		if !reserved {
			t.Fatal("upload name was released before the offloaded object was deleted")
		}
	default:
		t.Fatal("retry did not reach the object store")
	}
	if fake.Object("media", key) != nil || srv.gocdnItemByPath(t.Context(), a.FilePath) != nil || srv.wbWizRecords()[id] != nil {
		t.Fatal("retry left the offloaded object, its mapping or the import record")
	}
}

func TestWBImportWizardUndoCanRetryAfterKitDeleteFailure(t *testing.T) {
	srv, _, _, w := wizStart(t)
	const kit = "retry-kit"
	id := wbWizNewID()
	rec := &wbWizRecord{ID: id, Kit: kit, KitCreated: true}
	if err := srv.settings.Set(t.Context(), map[string]any{
		wbWizImportsOption: map[string]*wbWizRecord{id: rec},
		wbKitsOption:       map[string]*wbKit{kit: {Slug: kit, Name: "Retry kit", Version: 1, Root: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	kitDir, staged := filepath.Join(srv.wbKitRoot(), kit), srv.wbWizDir(id)
	for _, dir := range []string{kitDir, staged} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(kitDir, "site.css"), []byte("body{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := func(query string) {
		t.Helper()
		if _, err := srv.store.Pool.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	exec(`ALTER TABLE settings ADD CONSTRAINT undo_kit_guard CHECK (key <> 'wb_design_kits' OR value ? 'retry-kit')`)
	undo := func() string {
		t.Helper()
		target := wbWizURL + "?action=undo&import=" + id
		code, form, _ := w.get(target)
		nonce := wizHiddenRE.FindStringSubmatch(form)
		if code != http.StatusOK || len(nonce) != 2 {
			t.Fatalf("undo form: %d %s", code, wbSnippet(form, "notice"))
		}
		code, body, _ := w.postForm(target, url.Values{"_wpnonce": {nonce[1]}, "confirm": {"1"}})
		if code != http.StatusOK {
			t.Fatalf("undo: %d %s", code, wbSnippet(body, "notice"))
		}
		return body
	}
	if body := undo(); !strings.Contains(body, "Retry the removal") || srv.wbWizRecords()[id] == nil {
		t.Fatalf("missing retry: %s", wbSnippet(body, "notice"))
	}
	if k := srv.wbKitList()[kit]; k == nil || k.Root != 1 {
		t.Fatalf("undo changed the kit root before deleting the kit: %+v", k)
	}
	for _, name := range []string{filepath.Join(kitDir, "site.css"), staged} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("failed undo removed %q: %v", name, err)
		}
	}
	exec(`ALTER TABLE settings DROP CONSTRAINT undo_kit_guard`)
	if body := undo(); !strings.Contains(body, "and its design kit") {
		t.Fatalf("retry did not finish: %s", wbSnippet(body, "notice"))
	}
	if srv.wbKitList()[kit] != nil || srv.wbWizRecords()[id] != nil {
		t.Fatal("retry left the kit or import record")
	}
	for _, name := range []string{kitDir, staged} {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retry left %q: %v", name, err)
		}
	}
}

func TestWBImportWizardCompletedPublishKeepsLaterVisibilityChange(t *testing.T) {
	srv, _, _, w := wizStart(t)
	_, _, loc := w.upload(wizSite(t), "site.zip", "publish-once")
	id := wizIDRE.FindStringSubmatch(loc)[1]
	_, analysis, _ := w.get(loc)
	at := wbWizURL + "?import=" + id
	code, _, result := w.postForm(at+"&action=import", url.Values{
		"_wpnonce": {wizHiddenRE.FindStringSubmatch(analysis)[1]},
		"kit":      {"publish-once"},
		"status":   {"draft"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("draft import: %d", code)
	}
	_, report, _ := w.get(result)
	form := url.Values{"_wpnonce": {wizHiddenRE.FindStringSubmatch(report)[1]}}
	publish := at + "&action=publish"
	if code, body, _ := w.postForm(publish, form); code != http.StatusOK || strings.Contains(body, "Publish the pages") {
		t.Fatalf("initial publish: %d %s", code, wbSnippet(body, "notice"))
	}
	m := srv.wbWizLoad(id)
	if m == nil || m.Result == nil || !m.Result.Published || len(m.Result.Pages) == 0 {
		t.Fatal("initial publication did not save its result")
	}
	pageID := m.Result.Pages[0].PostID
	if _, err := srv.store.Pool.Exec(t.Context(), `UPDATE posts SET status = 'private' WHERE id = $1`, pageID); err != nil {
		t.Fatal(err)
	}
	if code, _, loc := w.postForm(publish, form); code != http.StatusSeeOther || loc != result {
		t.Fatalf("completed publish: %d %q, want redirect to %q", code, loc, result)
	}
	post, err := srv.store.GetPost(t.Context(), pageID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != "private" {
		t.Fatalf("status after repeat publish = %q, want private", post.Status)
	}
}

func TestWBImportWizardRetriesPreserveLaterVisibilityChanges(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "partial publication"
		if direct {
			name = "replaced draft"
		}
		t.Run(name, func(t *testing.T) {
			srv, _, _, w := wizStart(t)
			code, _, loc := w.upload(wizSite(t), "site.zip", "visibility-retry")
			parts := wizIDRE.FindStringSubmatch(loc)
			if code != http.StatusFound || len(parts) != 2 {
				t.Fatalf("upload: %d %q", code, loc)
			}
			at := wbWizURL + "?import=" + parts[1]
			_, analysis, _ := w.get(loc)
			importForm := url.Values{"_wpnonce": {wizHiddenRE.FindStringSubmatch(analysis)[1]}, "kit": {"visibility-retry"}, "status": {"draft"}}
			code, _, result := w.postForm(at+"&action=import", importForm)
			if code != http.StatusSeeOther {
				t.Fatalf("import drafts: %d", code)
			}
			_, report, _ := w.get(result)
			publishForm := url.Values{"_wpnonce": {wizHiddenRE.FindStringSubmatch(report)[1]}}
			publish := at + "&action=publish"
			m := srv.wbWizLoad(parts[1])
			if m == nil || m.Result == nil || len(m.Result.Pages) < 2 {
				t.Fatal("missing drafts in report")
			}
			protected, pending := m.Result.Pages[0].PostID, m.Result.Pages[1].PostID
			wantResponse := http.StatusOK
			if direct {
				importForm.Set("replace", "1")
				importForm.Set("status", "publish")
				if code, _, _ := w.postForm(at+"&action=import", importForm); code != http.StatusSeeOther {
					t.Fatalf("direct replacement: %d", code)
				}
				wantResponse = http.StatusSeeOther
			} else {
				if _, err := srv.store.Pool.Exec(t.Context(), fmt.Sprintf(
					`ALTER TABLE posts ADD CONSTRAINT reject_retry_post CHECK (id <> %d OR status <> 'publish')`, pending)); err != nil {
					t.Fatal(err)
				}
				if code, _, _ := w.postForm(publish, publishForm); code != http.StatusOK {
					t.Fatalf("partial publication: %d", code)
				}
				if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE id=$1 AND status='draft'`, pending); n != 1 {
					t.Fatal("fixture did not leave a draft to retry")
				}
				if _, err := srv.store.Pool.Exec(t.Context(), `ALTER TABLE posts DROP CONSTRAINT reject_retry_post`); err != nil {
					t.Fatal(err)
				}
			}
			if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE id=$1 AND status='publish'`, protected); n != 1 {
				t.Fatal("fixture did not publish the protected page")
			}
			if _, err := srv.store.Pool.Exec(t.Context(), `UPDATE posts SET status='private' WHERE id=$1`, protected); err != nil {
				t.Fatal(err)
			}
			if code, _, _ := w.postForm(publish, publishForm); code != wantResponse {
				t.Fatalf("repeated publication: %d, want %d", code, wantResponse)
			}
			if n := wizCount(t, srv, `SELECT count(*) FROM posts WHERE (id=$1 AND status='private') OR (id=$2 AND status='publish')`, protected, pending); n != 2 {
				t.Fatal("repeated publication did not preserve the private page and publish the pending page")
			}
		})
	}
}
