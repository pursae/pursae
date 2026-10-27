package server

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"wpgo/internal/store"
)

// TestWXRExport covers export.php: the filter form, the full export (menus as nav_menu terms and
// nav_menu_item items, wp_block patterns, postmeta) and the posts export filtered by status, author and date.
func TestWXRExport(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Export Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK || !strings.Contains(body, "Success!") {
		t.Fatalf("install: %d", code)
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	ctx := context.Background()
	admin, err := srv.store.UserByLoginOrEmail(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 15, 12, 0, 0, 0, time.UTC)
	for _, p := range []*store.Post{
		{Type: "post", AuthorID: admin.ID, Status: "publish", Title: "Old published", Content: "<p>old</p>", PublishedAt: &old, Meta: map[string]any{"my_key": "my value"}},
		{Type: "post", AuthorID: admin.ID, Status: "draft", Title: "Fresh draft", Content: "<p>draft</p>"},
		{Type: "wp_block", AuthorID: admin.ID, Status: "publish", Title: "Signature pattern", Content: "<!-- wp:paragraph --><p>Sig</p><!-- /wp:paragraph -->", Meta: map[string]any{"wp_pattern_sync_status": "unsynced"}},
	} {
		if err := srv.store.InsertPost(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	menuID, err := srv.store.CreateMenu(ctx, "Primary")
	if err != nil {
		t.Fatal(err)
	}
	parent := &store.EditItem{MenuID: menuID, Kind: "custom", Object: "custom", URL: "https://example.com/", Label: "Example", Classes: "one two", Order: 1}
	if err := srv.store.AddMenuItem(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := &store.EditItem{MenuID: menuID, ParentID: parent.ID, Kind: "custom", Object: "custom", URL: "/about/", Label: "About", Order: 2, Target: "_blank", XFN: "me"}
	if err := srv.store.AddMenuItem(ctx, child); err != nil {
		t.Fatal(err)
	}

	// The form: export.php's filter lists for posts, pages and media.
	code, body, _ := get(t, c, base+"/wp-admin/export")
	if code != http.StatusOK {
		t.Fatalf("export screen: %d", code)
	}
	for _, want := range []string{`<ul id="post-filters" class="export-filters">`, `<ul id="page-filters" class="export-filters">`, `<ul id="attachment-filters" class="export-filters">`,
		`<select name="cat" id="cat" class="postform">`, `<select name="post_author" id="post_author">`, `<option value="` + itoa64(admin.ID) + `">admin (admin)</option>`,
		`<select name="post_start_date" id="post-start-date">`, `<option value="2020-01">January 2020</option>`, `<select name="post_status" id="post-status">`,
		`<option value="publish">Published</option>`, `<option value="future">Scheduled</option>`, `<select name="page_author" id="page_author">`,
		`<select name="page_end_date" id="page-end-date">`, `<select name="attachment_start_date" id="attachment-start-date">`,
		`<input type="radio" name="content" value="posts" />`, `$('#' + $(this).val() + '-filters').show();`} {
		if !strings.Contains(body, want) {
			t.Errorf("export form misses %q", want)
		}
	}

	// All content.
	code, xml, _ := get(t, c, base+"/wp-admin/export?download=true&content=all")
	if code != http.StatusOK {
		t.Fatalf("export: %d", code)
	}
	for _, want := range []string{
		"<wp:term><wp:term_id>" + itoa64(menuID) + "</wp:term_id><wp:term_taxonomy><![CDATA[nav_menu]]></wp:term_taxonomy><wp:term_slug><![CDATA[primary]]></wp:term_slug><wp:term_parent><![CDATA[]]></wp:term_parent><wp:term_name><![CDATA[Primary]]></wp:term_name></wp:term>",
		"<wp:post_type>nav_menu_item</wp:post_type>", `<category domain="nav_menu" nicename="primary"><![CDATA[Primary]]></category>`,
		"<wp:post_id>" + itoa64(wxrMenuItemBase+parent.ID) + "</wp:post_id>",
		"<wp:meta_key><![CDATA[_menu_item_menu_item_parent]]></wp:meta_key>\n\t\t\t<wp:meta_value><![CDATA[" + itoa64(wxrMenuItemBase+parent.ID) + "]]></wp:meta_value>",
		`<wp:meta_key><![CDATA[_menu_item_classes]]></wp:meta_key>` + "\n\t\t\t" + `<wp:meta_value><![CDATA[a:2:{i:0;s:3:"one";i:1;s:3:"two";}]]></wp:meta_value>`,
		`<wp:meta_value><![CDATA[a:1:{i:0;s:0:"";}]]></wp:meta_value>`,
		"<wp:meta_key><![CDATA[_menu_item_url]]></wp:meta_key>\n\t\t\t<wp:meta_value><![CDATA[https://example.com/]]></wp:meta_value>",
		"<wp:meta_key><![CDATA[_menu_item_target]]></wp:meta_key>\n\t\t\t<wp:meta_value><![CDATA[_blank]]></wp:meta_value>",
		"<wp:meta_key><![CDATA[_menu_item_xfn]]></wp:meta_key>\n\t\t\t<wp:meta_value><![CDATA[me]]></wp:meta_value>",
		"<wp:post_type><![CDATA[wp_block]]></wp:post_type>", "<title><![CDATA[Signature pattern]]></title>",
		"<wp:meta_key><![CDATA[wp_pattern_sync_status]]></wp:meta_key>\n\t\t\t<wp:meta_value><![CDATA[unsynced]]></wp:meta_value>",
		"<wp:meta_key><![CDATA[my_key]]></wp:meta_key>\n\t\t\t<wp:meta_value><![CDATA[my value]]></wp:meta_value>",
		"<title><![CDATA[Old published]]></title>", "<title><![CDATA[Fresh draft]]></title>", "<title><![CDATA[Hello world!]]></title>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("all export misses %q", want)
		}
	}
	if strings.Count(xml, "<wp:post_type>nav_menu_item</wp:post_type>") != 2 {
		t.Errorf("expected 2 menu items in the export")
	}

	// Posts filtered by status, author and date range like export.php's filters.
	has := func(xml string, titles ...string) {
		t.Helper()
		for _, want := range titles {
			if !strings.Contains(xml, "<title><![CDATA["+want+"]]></title>") {
				t.Errorf("export misses %q", want)
			}
		}
	}
	hasNot := func(xml string, titles ...string) {
		t.Helper()
		for _, want := range titles {
			if strings.Contains(xml, "<title><![CDATA["+want+"]]></title>") {
				t.Errorf("export should not contain %q", want)
			}
		}
	}
	_, xml, _ = get(t, c, base+"/wp-admin/export?download=true&content=posts&post_status=draft")
	has(xml, "Fresh draft")
	hasNot(xml, "Old published", "Hello world!", "Signature pattern", "Sample Page")
	if strings.Contains(xml, "nav_menu_item") {
		t.Errorf("a posts export must not carry menu items")
	}
	_, xml, _ = get(t, c, base+"/wp-admin/export?download=true&content=posts&post_author="+itoa64(admin.ID)+"&post_status=0")
	has(xml, "Fresh draft", "Old published", "Hello world!")
	_, xml, _ = get(t, c, base+"/wp-admin/export?download=true&content=posts&post_author=424242")
	hasNot(xml, "Fresh draft", "Old published", "Hello world!")
	_, xml, _ = get(t, c, base+"/wp-admin/export?download=true&content=posts&post_start_date=2019-12&post_end_date=2020-01")
	has(xml, "Old published")
	hasNot(xml, "Fresh draft", "Hello world!")
	_, xml, _ = get(t, c, base+"/wp-admin/export?download=true&content=posts&post_start_date=2020-02&post_end_date=0")
	has(xml, "Fresh draft", "Hello world!")
	hasNot(xml, "Old published")
	_, xml, _ = get(t, c, base+"/wp-admin/export?download=true&content=pages&page_status=publish")
	has(xml, "Sample Page")
	hasNot(xml, "Old published")
}

const wxrFixture = `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0"
	xmlns:excerpt="http://wordpress.org/export/1.2/excerpt/"
	xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:wfw="http://wellformedweb.org/CommentAPI/"
	xmlns:dc="http://purl.org/dc/elements/1.1/"
	xmlns:wp="http://wordpress.org/export/1.2/"
>
<channel>
	<title>Old Site</title>
	<link>http://old.example.test</link>
	<wp:wxr_version>1.2</wp:wxr_version>
	<wp:author><wp:author_id>1</wp:author_id><wp:author_login><![CDATA[admin]]></wp:author_login><wp:author_email><![CDATA[admin@example.test]]></wp:author_email><wp:author_display_name><![CDATA[admin]]></wp:author_display_name></wp:author>
	<wp:category><wp:term_id>7</wp:term_id><wp:category_nicename><![CDATA[news]]></wp:category_nicename><wp:category_parent><![CDATA[]]></wp:category_parent><wp:cat_name><![CDATA[News]]></wp:cat_name></wp:category>
	<wp:term><wp:term_id>12</wp:term_id><wp:term_taxonomy><![CDATA[nav_menu]]></wp:term_taxonomy><wp:term_slug><![CDATA[main-menu]]></wp:term_slug><wp:term_parent></wp:term_parent><wp:term_name><![CDATA[Main Menu]]></wp:term_name></wp:term>
	<item>
		<title><![CDATA[Imported photo]]></title>
		<link>http://old.example.test/photo/</link>
		<dc:creator><![CDATA[admin]]></dc:creator>
		<content:encoded><![CDATA[A description]]></content:encoded>
		<excerpt:encoded><![CDATA[A caption]]></excerpt:encoded>
		<wp:post_id>50</wp:post_id>
		<wp:post_date><![CDATA[2021-03-04 10:11:12]]></wp:post_date>
		<wp:post_name><![CDATA[photo]]></wp:post_name>
		<wp:status><![CDATA[inherit]]></wp:status>
		<wp:post_parent>10</wp:post_parent>
		<wp:post_type><![CDATA[attachment]]></wp:post_type>
		<wp:attachment_url><![CDATA[http://files.example.test/2021/03/photo.png]]></wp:attachment_url>
		<wp:postmeta><wp:meta_key><![CDATA[_wp_attachment_image_alt]]></wp:meta_key><wp:meta_value><![CDATA[Alt text]]></wp:meta_value></wp:postmeta>
	</item>
	<item>
		<title><![CDATA[Post with picture]]></title>
		<dc:creator><![CDATA[admin]]></dc:creator>
		<content:encoded><![CDATA[<!-- wp:image --><figure class="wp-block-image"><img src="http://files.example.test/2021/03/photo.png" /></figure><!-- /wp:image --><!-- wp:paragraph --><p>Body</p><!-- /wp:paragraph -->]]></content:encoded>
		<excerpt:encoded><![CDATA[Short one]]></excerpt:encoded>
		<wp:post_id>10</wp:post_id>
		<wp:post_date><![CDATA[2021-03-05 09:00:00]]></wp:post_date>
		<wp:comment_status><![CDATA[open]]></wp:comment_status>
		<wp:post_name><![CDATA[post-with-picture]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_parent>0</wp:post_parent>
		<wp:post_type><![CDATA[post]]></wp:post_type>
		<category domain="category" nicename="news"><![CDATA[News]]></category>
		<wp:postmeta><wp:meta_key><![CDATA[_edit_lock]]></wp:meta_key><wp:meta_value><![CDATA[1:1]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_thumbnail_id]]></wp:meta_key><wp:meta_value><![CDATA[50]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[my_key]]></wp:meta_key><wp:meta_value><![CDATA[my value]]></wp:meta_value></wp:postmeta>
	</item>
	<item>
		<title><![CDATA[Reusable]]></title>
		<dc:creator><![CDATA[admin]]></dc:creator>
		<content:encoded><![CDATA[<!-- wp:paragraph --><p>Pattern</p><!-- /wp:paragraph -->]]></content:encoded>
		<excerpt:encoded><![CDATA[]]></excerpt:encoded>
		<wp:post_id>20</wp:post_id>
		<wp:post_date><![CDATA[2021-03-06 09:00:00]]></wp:post_date>
		<wp:post_name><![CDATA[reusable]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_type><![CDATA[wp_block]]></wp:post_type>
		<wp:postmeta><wp:meta_key><![CDATA[wp_pattern_sync_status]]></wp:meta_key><wp:meta_value><![CDATA[unsynced]]></wp:meta_value></wp:postmeta>
	</item>
	<item>
		<title><![CDATA[Child first]]></title>
		<dc:creator><![CDATA[admin]]></dc:creator>
		<content:encoded><![CDATA[Item description]]></content:encoded>
		<excerpt:encoded><![CDATA[Item title attribute]]></excerpt:encoded>
		<wp:post_id>31</wp:post_id>
		<wp:post_date><![CDATA[2021-03-07 09:00:00]]></wp:post_date>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:menu_order>2</wp:menu_order>
		<wp:post_type><![CDATA[nav_menu_item]]></wp:post_type>
		<category domain="nav_menu" nicename="main-menu"><![CDATA[Main Menu]]></category>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_type]]></wp:meta_key><wp:meta_value><![CDATA[custom]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_menu_item_parent]]></wp:meta_key><wp:meta_value><![CDATA[30]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_object_id]]></wp:meta_key><wp:meta_value><![CDATA[31]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_object]]></wp:meta_key><wp:meta_value><![CDATA[custom]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_target]]></wp:meta_key><wp:meta_value><![CDATA[_blank]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_classes]]></wp:meta_key><wp:meta_value><![CDATA[a:2:{i:0;s:3:"red";i:1;s:4:"bold";}]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_xfn]]></wp:meta_key><wp:meta_value><![CDATA[]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_url]]></wp:meta_key><wp:meta_value><![CDATA[https://example.org/child]]></wp:meta_value></wp:postmeta>
	</item>
	<item>
		<title><![CDATA[Picture post link]]></title>
		<dc:creator><![CDATA[admin]]></dc:creator>
		<content:encoded><![CDATA[]]></content:encoded>
		<excerpt:encoded><![CDATA[]]></excerpt:encoded>
		<wp:post_id>30</wp:post_id>
		<wp:post_date><![CDATA[2021-03-07 09:00:00]]></wp:post_date>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:menu_order>1</wp:menu_order>
		<wp:post_type><![CDATA[nav_menu_item]]></wp:post_type>
		<category domain="nav_menu" nicename="main-menu"><![CDATA[Main Menu]]></category>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_type]]></wp:meta_key><wp:meta_value><![CDATA[post_type]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_menu_item_parent]]></wp:meta_key><wp:meta_value><![CDATA[0]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_object_id]]></wp:meta_key><wp:meta_value><![CDATA[10]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_object]]></wp:meta_key><wp:meta_value><![CDATA[post]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_target]]></wp:meta_key><wp:meta_value><![CDATA[]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_classes]]></wp:meta_key><wp:meta_value><![CDATA[a:1:{i:0;s:0:"";}]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_xfn]]></wp:meta_key><wp:meta_value><![CDATA[]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_url]]></wp:meta_key><wp:meta_value><![CDATA[]]></wp:meta_value></wp:postmeta>
	</item>
	<item>
		<title><![CDATA[Points nowhere]]></title>
		<dc:creator><![CDATA[admin]]></dc:creator>
		<content:encoded><![CDATA[]]></content:encoded>
		<excerpt:encoded><![CDATA[]]></excerpt:encoded>
		<wp:post_id>32</wp:post_id>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:menu_order>3</wp:menu_order>
		<wp:post_type><![CDATA[nav_menu_item]]></wp:post_type>
		<category domain="nav_menu" nicename="main-menu"><![CDATA[Main Menu]]></category>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_type]]></wp:meta_key><wp:meta_value><![CDATA[post_type]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_object_id]]></wp:meta_key><wp:meta_value><![CDATA[999]]></wp:meta_value></wp:postmeta>
		<wp:postmeta><wp:meta_key><![CDATA[_menu_item_object]]></wp:meta_key><wp:meta_value><![CDATA[post]]></wp:meta_value></wp:postmeta>
	</item>
</channel>
</rss>
`

// TestWXRImport covers the importer: attachments are downloaded through the upload pipeline (with the
// remote fetch pointed at an httptest server), _thumbnail_id and other postmeta are kept, attachment
// URLs in content are rewritten, nav_menu terms and nav_menu_item posts become menus and items, and
// wp_block posts are imported as patterns.
func TestWXRImport(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Import Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK || !strings.Contains(body, "Success!") {
		t.Fatalf("install: %d", code)
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	ctx := context.Background()
	pool := getPool(t)

	// Without the transport hook, the sideloader never talks to a loopback address.
	if _, err := srv.sideloadURL(ctx, "http://127.0.0.1:1/x.png", 1, 0); err == nil {
		t.Errorf("sideload of a loopback URL must fail")
	}
	if _, err := srv.sideloadURL(ctx, "ftp://files.example.test/x.png", 1, 0); err == nil {
		t.Errorf("sideload of a non-http URL must fail")
	}
	if _, err := srv.sideloadURL(ctx, "http://user:pw@files.example.test/x.png", 1, 0); err == nil {
		t.Errorf("sideload of a URL with credentials must fail")
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM media WHERE file_path LIKE '%x.png'`).Scan(&n)
	if n != 0 {
		t.Errorf("refused sideloads must not insert attachments")
	}

	// The "remote" file server.
	img := image.NewRGBA(image.Rect(0, 0, 20, 10))
	for x := 0; x < 20; x++ {
		for y := 0; y < 10; y++ {
			img.Set(x, y, color.RGBA{R: 200, G: 30, B: 30, A: 255})
		}
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/2021/03/photo.png" && r.Host == "files.example.test" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBuf.Bytes())
			return
		}
		http.NotFound(w, r)
	}))
	defer files.Close()
	old := wxrFetchTransport
	wxrFetchTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme = "http"
		r2.URL.Host = files.Listener.Addr().String()
		r2.Host = r.URL.Host
		return http.DefaultTransport.RoundTrip(r2)
	})
	t.Cleanup(func() { wxrFetchTransport = old })

	_, form, _ := get(t, c, base+"/wp-admin/import")
	nonce := formNonce(t, form)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("_wpnonce", nonce)
	fw, _ := mw.CreateFormFile("import", "old-site.xml")
	_, _ = io.WriteString(fw, wxrFixture)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, base+"/wp-admin/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "All done.") {
		t.Fatalf("import: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Imported 1 posts, 0 pages, 1 patterns, 2 menu items, 1 media files, 0 comments and 1 new categories and tags.") {
		t.Errorf("report: %s", regexp.MustCompile(`<p>Imported[^<]*</p>`).FindString(string(body)))
	}
	if !strings.Contains(string(body), "Menu item “Points nowhere” skipped due to missing post.") {
		t.Errorf("a menu item whose post is missing must be reported as skipped")
	}

	// The attachment: downloaded into the uploads dir, with image sizes and the file's metadata.
	var attID int64
	var filePath, alt, caption, description, title string
	var width int
	var created time.Time
	if err := pool.QueryRow(ctx, `SELECT id, file_path, alt_text, caption, description, title, coalesce(width,0), created_at FROM media WHERE file_path LIKE '%photo%'`).
		Scan(&attID, &filePath, &alt, &caption, &description, &title, &width, &created); err != nil {
		t.Fatalf("attachment not imported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.UploadsDir, filepath.FromSlash(filePath))); err != nil {
		t.Errorf("downloaded file missing: %v", err)
	}
	if alt != "Alt text" || caption != "A caption" || description != "A description" || title != "Imported photo" || width != 20 {
		t.Errorf("attachment fields: alt=%q caption=%q description=%q title=%q width=%d", alt, caption, description, title, width)
	}
	if created.Year() != 2021 {
		t.Errorf("attachment date not taken from the file: %v", created)
	}

	// The post: featured image remapped, meta kept (editor locks dropped), content URL rewritten, parent of the attachment.
	var postID, featured int64
	var content, myKey, editLock, status string
	if err := pool.QueryRow(ctx, `SELECT id, coalesce(featured_media_id,0), content, coalesce(meta->>'my_key',''), coalesce(meta->>'_edit_lock',''), status FROM posts WHERE title = 'Post with picture'`).
		Scan(&postID, &featured, &content, &myKey, &editLock, &status); err != nil {
		t.Fatalf("post not imported: %v", err)
	}
	if featured != attID {
		t.Errorf("featured_media_id = %d, want %d", featured, attID)
	}
	if myKey != "my value" || editLock != "" || status != "publish" {
		t.Errorf("post meta/status: my_key=%q _edit_lock=%q status=%q", myKey, editLock, status)
	}
	newURL := srv.uploadURL(ctx, filePath)
	if !strings.Contains(content, `src="`+newURL+`"`) || strings.Contains(content, "files.example.test") {
		t.Errorf("attachment URL not rewritten in content: %s", content)
	}
	var attParent int64
	_ = pool.QueryRow(ctx, `SELECT coalesce(post_id,0) FROM media WHERE id = $1`, attID).Scan(&attParent)
	if attParent != postID {
		t.Errorf("attachment parent = %d, want %d", attParent, postID)
	}
	var cat string
	_ = pool.QueryRow(ctx, `SELECT t.slug FROM post_terms pt JOIN terms t ON t.id = pt.term_id WHERE pt.post_id = $1 AND t.taxonomy = 'category'`, postID).Scan(&cat)
	if cat != "news" {
		t.Errorf("post category = %q", cat)
	}

	// The pattern.
	var blockMeta string
	if err := pool.QueryRow(ctx, `SELECT coalesce(meta->>'wp_pattern_sync_status','') FROM posts WHERE type = 'wp_block' AND title = 'Reusable' AND status = 'publish'`).Scan(&blockMeta); err != nil {
		t.Errorf("wp_block not imported: %v", err)
	} else if blockMeta != "unsynced" {
		t.Errorf("wp_block meta = %q", blockMeta)
	}

	// The menu: created from the nav_menu term, two items, the child (listed first in the file) parented to the post link.
	var menuID int64
	var menuName string
	if err := pool.QueryRow(ctx, `SELECT id, name FROM menus WHERE slug = 'main-menu'`).Scan(&menuID, &menuName); err != nil {
		t.Fatalf("menu not created: %v", err)
	}
	if menuName != "Main Menu" {
		t.Errorf("menu name = %q", menuName)
	}
	items, err := srv.store.MenuItems(ctx, menuID)
	if err != nil || len(items) != 2 {
		t.Fatalf("menu items = %d (%v)", len(items), err)
	}
	link, child := items[0], items[1]
	if link.Label != "Picture post link" || link.Kind != "post_type" || link.Object != "post" || link.TargetID != postID || link.ParentID != 0 || link.Order != 1 {
		t.Errorf("post link item: %+v", link)
	}
	if child.Label != "Child first" || child.Kind != "custom" || child.URL != "https://example.org/child" || child.ParentID != link.ID || child.Target != "_blank" ||
		child.Classes != "red bold" || child.AttrTitle != "Item title attribute" || child.Description != "Item description" || child.Order != 2 || child.Status != "publish" {
		t.Errorf("child item: %+v", child)
	}
	if strings.Contains(string(body), "Template error") {
		t.Error("template error on the import screen")
	}
}
