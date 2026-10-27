package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"wpgo/internal/store"
)

// installFrontSite installs the site and logs the admin in; it returns the admin user id.
func installFrontSite(t *testing.T, base string, c *http.Client, srv *Server) int64 {
	t.Helper()
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Front Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK {
		t.Fatalf("install: %d %s", code, body)
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	u, err := srv.store.UserByLoginOrEmail(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func mustInsertPost(t *testing.T, srv *Server, p *store.Post) *store.Post {
	t.Helper()
	if p.Status == "" {
		p.Status = "publish"
	}
	if err := srv.store.InsertPost(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	srv.purgeTags("home", "widgets", "feed")
	return p
}

// getPage is get() for front-end URLs written with query vars (?p=, ?page_id=, ?cat=): with pretty
// permalinks redirect_canonical() answers 301, so the canonical URL is fetched in turn.
func getPage(t *testing.T, c *http.Client, u string) (int, string, string) {
	t.Helper()
	code, body, loc := get(t, c, u)
	if code == http.StatusMovedPermanently && loc != "" {
		return get(t, c, loc)
	}
	return code, body, loc
}

func pathOf(t *testing.T, base, absolute string) string {
	t.Helper()
	if !strings.HasPrefix(absolute, base) {
		t.Fatalf("%s is not under %s", absolute, base)
	}
	return strings.TrimPrefix(absolute, base)
}

func TestFrontFeedsAndRedirects(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	adminID := installFrontSite(t, base, c, srv)
	anon := &http.Client{CheckRedirect: c.CheckRedirect}
	lb := srv.links(ctx)

	hello, err := srv.store.PostBySlug(ctx, "post", "hello-world")
	if err != nil {
		t.Fatal(err)
	}
	helloPath := pathOf(t, base, lb.Permalink(hello))

	// Atom posts feed (feed-atom.php).
	code, body, _ := get(t, anon, base+"/feed/atom/")
	if code != http.StatusOK {
		t.Fatalf("/feed/atom/: %d", code)
	}
	for _, want := range []string{`<feed`, `xmlns:thr="http://purl.org/syndication/thread/1.0"`, `<title type="text">Front Site</title>`, `<id>` + base + `/feed/atom/</id>`,
		`<title type="html"><![CDATA[Hello world!]]></title>`, `<link rel="alternate" type="text/html" href="` + base + helloPath + `" />`, `<thr:total>1</thr:total>`,
		`<link rel="replies" type="application/atom+xml" href="` + base + helloPath + `feed/atom/" thr:count="1" />`, `<content type="html" xml:base="`} {
		if !strings.Contains(body, want) {
			t.Errorf("atom feed misses %q", want)
		}
	}
	resp, _ := anon.Get(base + "/feed/atom/")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/atom+xml") {
		t.Errorf("atom content type = %s", ct)
	}
	resp.Body.Close()

	// A post's comment feed lists that post's comments, in RSS2 and Atom.
	_, body, _ = get(t, anon, base+helloPath+"feed/")
	for _, want := range []string{`<title>Comments on: Hello world!</title>`, `<link>` + base + helloPath + `</link>`, `<title>By: A WordPress Go Commenter</title>`, `<dc:creator><![CDATA[A WordPress Go Commenter]]></dc:creator>`} {
		if !strings.Contains(body, want) {
			t.Errorf("post comments feed misses %q in %s", want, body)
		}
	}
	if strings.Contains(body, "<content:encoded><![CDATA[<p>Welcome to WordPress") {
		t.Error("post comment feed shows the post content instead of its comments")
	}
	_, body, _ = get(t, anon, base+helloPath+"feed/atom/")
	for _, want := range []string{`<feed`, `<title type="text">Comments on: Hello world!</title>`, `<link rel="self" type="application/atom+xml" href="` + base + helloPath + `feed/atom/" />`, `<title>By: A WordPress Go Commenter</title>`, `<thr:in-reply-to ref="`} {
		if !strings.Contains(body, want) {
			t.Errorf("post atom comments feed misses %q", want)
		}
	}
	_, body, _ = get(t, anon, base+"/comments/feed/atom/")
	if !strings.Contains(body, `<title type="text">Comments for Front Site</title>`) || !strings.Contains(body, `<title>Comment on Hello world! by A WordPress Go Commenter</title>`) {
		t.Errorf("site atom comments feed = %s", body)
	}
	_, body, _ = get(t, anon, base+"/comments/feed/")
	if !strings.Contains(body, `<title>Comments for Front Site</title>`) || !strings.Contains(body, `<guid isPermaLink="false">`+base+helloPath+`#comment-`) {
		t.Errorf("site comments feed = %s", body)
	}

	// Search and date archive feeds filter their posts; archive feeds carry the document title.
	if _, body, _ = get(t, anon, base+"/?s=zzzz-nothing&feed=rss2"); strings.Contains(body, "<item>") {
		t.Error("search feed for a missing term has items")
	}
	if _, body, _ = get(t, anon, base+"/?s=Hello&feed=rss2"); !strings.Contains(body, "<title>Hello world!</title>") || !strings.Contains(body, "Search Results for &#8220;Hello&#8221; &#8211; Front Site") {
		t.Errorf("search feed = %s", body)
	}
	if _, body, _ = get(t, anon, base+"/2001/feed/"); strings.Contains(body, "<item>") {
		t.Error("date feed for 2001 has items")
	}
	year := time.Now().In(srv.loc()).Format("2006")
	if _, body, _ = get(t, anon, base+"/"+year+"/feed/"); !strings.Contains(body, "<title>Hello world!</title>") || !strings.Contains(body, "<title>"+year+" &#8211; Front Site</title>") {
		t.Errorf("date feed = %s", body)
	}
	if _, body, _ = get(t, anon, base+"/category/uncategorized/feed/"); !strings.Contains(body, "<title>Uncategorized &#8211; Front Site</title>") {
		t.Errorf("category feed title missing: %s", body[:300])
	}
	// The RSS2 item follows feed-rss2.php's element order and comment links.
	_, body, _ = get(t, anon, base+"/feed/")
	item := body[strings.Index(body, "<item>"):]
	if !strings.Contains(item, "<comments>"+base+helloPath+"#comments</comments>") || strings.Index(item, "<guid") > strings.Index(item, "<description>") || strings.Index(item, "<comments>") < strings.Index(item, "<content:encoded>") {
		t.Errorf("rss2 item order/comments link wrong: %s", item)
	}
	// The post's head advertises its comment feed.
	if _, body, _ = get(t, anon, base+helloPath); !strings.Contains(body, `Hello world! Comments Feed" href="`+base+helloPath+`feed/" />`) {
		t.Error("post head lacks the comments feed link")
	}

	// redirect_canonical(): query-var URLs and missing trailing slashes go to the pretty permalink.
	for from, want := range map[string]string{
		"/?p=" + itoa64(hello.ID):                base + helloPath,
		"/?p=" + itoa64(hello.ID) + "&x=1":       base + helloPath + "?x=1",
		strings.TrimSuffix(helloPath, "/"):       base + helloPath,
		"/?feed=rss2":                            base + "/feed/",
		"/?cat=1":                                base + "/category/uncategorized/",
		"/?p=" + itoa64(hello.ID) + "&feed=rss2": base + helloPath + "feed/",
		"/category/uncategorized":                base + "/category/uncategorized/",
	} {
		if code, _, loc := get(t, anon, base+from); code != http.StatusMovedPermanently || loc != want {
			t.Errorf("%s: %d %s, want 301 %s", from, code, loc, want)
		}
	}
	if code, _, _ := get(t, anon, base+helloPath); code != http.StatusOK {
		t.Errorf("canonical URL itself redirected: %d", code)
	}
	if code, _, _ := get(t, anon, base+"/?s=Hello"); code != http.StatusOK {
		t.Errorf("search redirected: %d", code)
	}
	if code, _, _ := get(t, anon, base+"/?p="+itoa64(hello.ID)+"&preview=true"); code == http.StatusMovedPermanently {
		t.Error("preview URL must not be redirected")
	}

	// wp_old_slug_redirect(): a renamed post keeps answering at its old slug.
	renamed := mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: adminID, Title: "Renamed", Slug: "first-slug", Content: "<p>Renamed body</p>"})
	oldPath := pathOf(t, base, lb.Permalink(renamed))
	renamed.Slug = "second-slug"
	if err := srv.store.UpdatePost(ctx, renamed, adminID); err != nil {
		t.Fatal(err)
	}
	newPath := pathOf(t, base, lb.Permalink(renamed))
	if !strings.Contains(newPath, "second-slug") {
		t.Fatalf("new permalink = %s", newPath)
	}
	if code, _, loc := get(t, anon, base+oldPath); code != http.StatusMovedPermanently || loc != base+newPath {
		t.Errorf("old slug: %d %s, want 301 %s", code, loc, base+newPath)
	}
	if code, _, _ := get(t, anon, base+"/nope-never-existed/"); code != http.StatusNotFound {
		t.Errorf("unknown slug: %d", code)
	}
}

func TestFrontNextpageImagesSmilies(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	adminID := installFrontSite(t, base, c, srv)
	anon := &http.Client{CheckRedirect: c.CheckRedirect}
	lb := srv.links(ctx)

	// <!--nextpage--> splits the post into pages with wp_link_pages() markup.
	paged := mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: adminID, Title: "Paged", Content: "<!-- wp:paragraph -->\n<p>Page one text</p>\n<!-- /wp:paragraph -->\n\n<!-- wp:nextpage -->\n<!--nextpage-->\n<!-- /wp:nextpage -->\n\n<!-- wp:paragraph -->\n<p>Page two text</p>\n<!-- /wp:paragraph -->"})
	pagedPath := pathOf(t, base, lb.Permalink(paged))
	_, body, _ := get(t, anon, base+pagedPath)
	if !strings.Contains(body, "Page one text") || strings.Contains(body, "Page two text") {
		t.Error("page 1 shows the wrong content")
	}
	for _, want := range []string{`<div class="page-links"><span class="page-links-title">Pages:</span>`, `<span class="post-page-numbers current" aria-current="page"><span><span class="screen-reader-text">Page </span>1</span></span>`,
		`<a href="` + base + pagedPath + `2/" class="post-page-numbers"><span><span class="screen-reader-text">Page </span>2</span></a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("page links miss %q", want)
		}
	}
	if _, body, _ = get(t, anon, base+pagedPath+"2/"); !strings.Contains(body, "Page two text") || strings.Contains(body, "Page one text") || !strings.Contains(body, `aria-current="page"><span><span class="screen-reader-text">Page </span>2</span>`) {
		t.Error("page 2 shows the wrong content")
	}
	if code, _, loc := get(t, anon, base+pagedPath+"9/"); code != http.StatusMovedPermanently || loc != base+pagedPath {
		t.Errorf("page past the end: %d %s", code, loc)
	}
	if code, _, loc := get(t, anon, base+"/?p="+itoa64(paged.ID)+"&page=2"); code != http.StatusMovedPermanently || loc != base+pagedPath+"2/" {
		t.Errorf("?page=2: %d %s", code, loc)
	}
	if _, body, _ = get(t, anon, base+"/feed/"); !strings.Contains(body, "Page one text") || strings.Contains(body, "page-links") {
		t.Error("feed should show page one without page links")
	}

	// wp_filter_content_tags(): library images get width/height, srcset/sizes and loading attributes.
	att := &store.Attachment{UploaderID: adminID, FilePath: "2026/10/pic.jpg", Mime: "image/jpeg", Width: 2000, Height: 1500, Alt: "A pic",
		Variants: []store.AttachmentVariant{
			{Name: "thumbnail", FilePath: "2026/10/pic-150x150.jpg", Width: 150, Height: 150, Mime: "image/jpeg"},
			{Name: "medium", FilePath: "2026/10/pic-300x225.jpg", Width: 300, Height: 225, Mime: "image/jpeg"},
			{Name: "large", FilePath: "2026/10/pic-1024x768.jpg", Width: 1024, Height: 768, Mime: "image/jpeg"},
		}}
	if err := srv.store.InsertAttachment(ctx, att); err != nil {
		t.Fatal(err)
	}
	up := base + "/wp-content/uploads/2026/10/"
	img := `<!-- wp:image {"id":` + itoa64(att.ID) + `,"sizeSlug":"large"} -->
<figure class="wp-block-image size-large"><img src="` + up + `pic-1024x768.jpg" alt="" class="wp-image-` + itoa64(att.ID) + `"/></figure>
<!-- /wp:image -->
`
	imgPost := mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: adminID, Title: "Pictures", Content: img + img + img + img})
	_, body, _ = get(t, anon, base+pathOf(t, base, lb.Permalink(imgPost)))
	want := `<img decoding="async" fetchpriority="high" width="1024" height="768" src="` + up + `pic-1024x768.jpg" alt="" class="wp-image-` + itoa64(att.ID) + `" srcset="` + up + `pic-1024x768.jpg 1024w, ` + up + `pic-300x225.jpg 300w, ` + up + `pic.jpg 2000w" sizes="(max-width: 1024px) 100vw, 1024px" />`
	if !strings.Contains(body, want) {
		t.Errorf("first content image: want %s in %s", want, body)
	}
	if n := strings.Count(body, `loading="lazy"`); n < 1 {
		t.Errorf("fourth image should be lazy (lazy count %d)", n)
	}
	if !strings.Contains(body, `<img decoding="async" loading="lazy" width="1024" height="768" src="`+up+`pic-1024x768.jpg" alt="" class="wp-image-`+itoa64(att.ID)+`" srcset="`) || !strings.Contains(body, `sizes="auto, (max-width: 1024px) 100vw, 1024px"`) {
		t.Error("lazy image misses auto sizes")
	}
	if strings.Count(body, `fetchpriority="high"`) != 1 {
		t.Errorf("fetchpriority high printed %d times", strings.Count(body, `fetchpriority="high"`))
	}
	_, body, _ = get(t, anon, base+"/feed/")
	if !strings.Contains(body, `srcset="`+up+`pic-1024x768.jpg 1024w`) || strings.Contains(body, `loading="lazy"`) {
		t.Error("feed images should carry srcset but no lazy loading")
	}

	// convert_smilies() follows use_smilies.
	smiley := mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: adminID, Title: "Smiles", Content: "<!-- wp:paragraph -->\n<p>Happy :) and :mrgreen: but <code>:)</code> stays</p>\n<!-- /wp:paragraph -->"})
	smileyPath := pathOf(t, base, lb.Permalink(smiley))
	_, body, _ = get(t, anon, base+smileyPath)
	if !strings.Contains(body, "Happy \U0001f642 and ") || !strings.Contains(body, `<img src="`+base+`/wp-includes/images/smilies/mrgreen.png" alt=":mrgreen:" class="wp-smiley"`) || !strings.Contains(body, "<code>:)</code>") {
		t.Errorf("smilies not converted: %s", body)
	}
	if err := srv.settings.Set(ctx, map[string]any{"use_smilies": false}); err != nil {
		t.Fatal(err)
	}
	srv.purgeTags("post:" + itoa64(smiley.ID))
	if _, body, _ = get(t, anon, base+smileyPath); strings.Contains(body, "\U0001f642") {
		t.Error("smilies converted with use_smilies off")
	}
}

func TestFrontDynamicBlocks(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	adminID := installFrontSite(t, base, c, srv)
	anon := &http.Client{CheckRedirect: c.CheckRedirect}
	lb := srv.links(ctx)

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Remote</title>
<item><title>First remote &amp; item</title><link>https://example.com/one</link><description>Hello remote world from far away</description><pubDate>Mon, 02 Jan 2006 15:04:05 +0000</pubDate></item>
<item><title>Second</title><link>https://example.com/two</link></item></channel></rss>`))
	}))
	defer feedSrv.Close()
	allowPrivateFeedHosts = true
	defer func() { allowPrivateFeedHosts = false }()

	tag := &store.Term{Taxonomy: "post_tag", Name: "golang"}
	if err := srv.store.InsertTerm(ctx, tag); err != nil {
		t.Fatal(err)
	}
	hello, _ := srv.store.PostBySlug(ctx, "post", "hello-world")
	if err := srv.store.SetPostTerms(ctx, hello.ID, "post_tag", []int64{tag.ID}); err != nil {
		t.Fatal(err)
	}
	sample, _ := srv.store.PostBySlug(ctx, "page", "sample-page")
	child := mustInsertPost(t, srv, &store.Post{Type: "page", AuthorID: adminID, ParentID: sample.ID, Title: "Child Page", Content: "<p>child</p>"})

	content := `<!-- wp:calendar /-->
<!-- wp:tag-cloud {"showTagCounts":true} /-->
<!-- wp:page-list /-->
<!-- wp:loginout /-->
<!-- wp:social-links {"showLabels":true} -->
<ul class="wp-block-social-links"><!-- wp:social-link {"url":"https://wordpress.org","service":"wordpress"} /-->

<!-- wp:social-link {"url":"https://x.com/wp","service":"x"} /--></ul>
<!-- /wp:social-links -->
<!-- wp:avatar {"size":48} /-->
<!-- wp:rss {"feedURL":"` + feedSrv.URL + `/feed","displayExcerpt":true,"displayDate":true,"excerptLength":3} /-->
<!-- wp:footnotes /-->`
	blocksPost := mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: adminID, Title: "Blocks", Content: content,
		Meta: map[string]any{"footnotes": `[{"id":"abc","content":"A note"}]`}})
	postPath := pathOf(t, base, lb.Permalink(blocksPost))
	now := time.Now().In(srv.loc())

	code, body, _ := get(t, anon, base+postPath)
	if code != http.StatusOK {
		t.Fatalf("blocks post: %d", code)
	}
	day := now.Format("January 2, 2006")
	for _, want := range []string{
		// calendar
		`<div class="wp-block-calendar"><table id="wp-calendar" class="wp-calendar-table">`, `<caption>` + now.Format("January 2006") + `</caption>`,
		`<th scope="col" aria-label="Monday">M</th>`, `<td id="today"><a href="` + lb.DayLink(now.Year(), int(now.Month()), now.Day()) + `" aria-label="Posts published on ` + day + `">`,
		`<nav aria-label="Previous and next months" class="wp-calendar-nav">`, `<span class="wp-calendar-nav-prev">&nbsp;</span>`,
		// tag cloud
		`<p class="wp-block-tag-cloud"><a href="` + lb.TermLink(tag) + `" class="tag-cloud-link tag-link-` + itoa64(tag.ID) + ` tag-link-position-1" style="font-size: 8pt;" aria-label="golang (1 item)">golang<span class="tag-link-count"> (1)</span></a></p>`,
		// page list
		`<ul class="wp-block-page-list"><li class="wp-block-pages-list__item has-child"><a class="wp-block-pages-list__item__link" href="` + lb.Permalink(sample) + `">Sample Page</a><ul class="wp-block-navigation__submenu-container"><li class="wp-block-pages-list__item"><a class="wp-block-pages-list__item__link" href="` + lb.Permalink(child) + `">Child Page</a></li></ul></li></ul>`,
		// loginout
		`<div class="logged-out wp-block-loginout"><a href="/wp-login?redirect_to=` + url.QueryEscape(base+postPath) + `">Log in</a></div>`,
		// social links
		`<li class="wp-social-link wp-social-link-wordpress wp-block-social-link"><a href="https://wordpress.org" class="wp-block-social-link-anchor"><svg width="24" height="24" viewBox="0 0 24 24"`,
		`<span class="wp-block-social-link-label">WordPress</span></a></li>`, `<li class="wp-social-link wp-social-link-x wp-block-social-link">`, `wp-block-social-links-is-layout-flex`,
		// avatar
		`<div class="wp-block-avatar"><img alt='admin Avatar' src='`, `class='avatar avatar-48 photo wp-block-avatar__image' height='48' width='48'`,
		// rss
		`<ul class="has-dates has-excerpts wp-block-rss"><li class='wp-block-rss__item'><div class='wp-block-rss__item-title'><a href='https://example.com/one'>First remote &amp; item</a></div><time datetime="2006-01-02T15:04:05Z" class="wp-block-rss__item-publish-date">January 2, 2006</time> <div class="wp-block-rss__item-excerpt">Hello remote world [&hellip;]</div></li>`,
		`<div class='wp-block-rss__item-title'><a href='https://example.com/two'>Second</a></div><div class="wp-block-rss__item-excerpt"></div></li></ul>`,
		// footnotes
		`<ol class="wp-block-footnotes"><li id="abc">A note <a href="#abc-link" aria-label="Jump to footnote reference 1">↩︎</a></li></ol>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("blocks post misses %q", want)
		}
	}
	if t.Failed() {
		t.Logf("body: %s", body)
	}
	var cached int
	_ = getPool(t).QueryRow(ctx, `SELECT count(*) FROM transients WHERE key LIKE 'feed_%' AND expires_at > now() + interval '11 hours'`).Scan(&cached)
	if cached != 1 {
		t.Errorf("feed transient rows = %d", cached)
	}
	// A logged-in visitor sees the logout link.
	if _, body, _ = get(t, c, base+postPath); !strings.Contains(body, `<div class="logged-in wp-block-loginout"><a href="/wp-login?action=logout&amp;redirect_to=`) || !strings.Contains(body, `_wpnonce=`) || !strings.Contains(body, `">Log out</a></div>`) {
		t.Error("logged-in loginout block wrong")
	}
	// Private feed hosts are refused unless a test allows them; the site's own home URL is refused outright.
	allowPrivateFeedHosts = false
	srv.purgeTags("post:" + itoa64(blocksPost.ID))
	loop := mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: adminID, Title: "Loop", Content: `<!-- wp:rss {"feedURL":"` + base + `/"} /--><!-- wp:rss {"feedURL":"http://127.0.0.1:1/x"} /--><!-- wp:rss {"feedURL":"ftp://example.com/x"} /-->`})
	_, body, _ = get(t, anon, base+pathOf(t, base, lb.Permalink(loop)))
	if !strings.Contains(body, "homepage is not supported") || strings.Count(body, "<strong>RSS Error:</strong>") != 2 || !strings.Contains(body, "A valid URL was not provided.") {
		t.Errorf("rss block errors: %s", body)
	}

	// The day archive linked from the calendar works, and the login form variant renders wp_login_form().
	if code, body, _ := get(t, anon, lb.DayLink(now.Year(), int(now.Month()), now.Day())); code != http.StatusOK || !strings.Contains(body, "Blocks") {
		t.Errorf("day archive: %d", code)
	}
	form := mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: adminID, Title: "Form", Content: `<!-- wp:loginout {"displayLoginAsForm":true,"redirectToCurrent":false} /-->`})
	if _, body, _ := get(t, anon, base+pathOf(t, base, lb.Permalink(form))); !strings.Contains(body, `<div class="logged-out has-login-form wp-block-loginout"><form name="loginform" id="loginform" action="/wp-login" method="post"><p class="login-username"><label for="user_login">Username or Email Address</label><input type="text" name="log" id="user_login"`) {
		t.Errorf("login form block: %s", body)
	}
}

func TestFrontCommentPagingFloodAndClosing(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	adminID := installFrontSite(t, base, c, srv)
	anon := &http.Client{CheckRedirect: c.CheckRedirect}
	lb := srv.links(ctx)

	hello, _ := srv.store.PostBySlug(ctx, "post", "hello-world")
	helloPath := pathOf(t, base, lb.Permalink(hello))
	for i, text := range []string{"Second comment", "Third comment", "Fourth comment"} {
		cm := &store.Comment{PostID: hello.ID, AuthorName: "Reader", AuthorEmail: "reader@example.test", Content: text, Status: "approved", AuthorIP: "10.0.0." + itoa(i)}
		if err := srv.store.InsertComment(ctx, cm); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := srv.settings.Set(ctx, map[string]any{"page_comments": true, "comments_per_page": 2, "default_comments_page": "newest"}); err != nil {
		t.Fatal(err)
	}
	srv.purgeTags("post:" + itoa64(hello.ID))

	// Newest page first: comments 3 and 4, with a link to the older page.
	_, body, _ := get(t, anon, base+helloPath)
	if !strings.Contains(body, "Third comment") || !strings.Contains(body, "Fourth comment") || strings.Contains(body, "Second comment") {
		t.Errorf("newest comment page wrong: %s", body)
	}
	nav := `<nav class="navigation comments-navigation" aria-label="Comments">
		<h2 class="screen-reader-text">Comments navigation</h2>
		<div class="nav-links"><div class="nav-previous"><a href="` + base + helloPath + `comment-page-1/#comments" >Older comments</a></div></div>
	</nav>`
	if !strings.Contains(body, nav) {
		t.Errorf("comments navigation missing: %s", body)
	}
	if strings.Count(body, "comments-navigation") != 2 {
		t.Errorf("the_comments_navigation printed %d times", strings.Count(body, "comments-navigation"))
	}
	if !strings.Contains(body, `<a href="`+base+helloPath+`comment-page-2/#comment-`) {
		t.Error("comment permalinks should carry comment-page-2")
	}
	_, body, _ = get(t, anon, base+helloPath+"comment-page-1/")
	if !strings.Contains(body, "Second comment") || strings.Contains(body, "Third comment") || !strings.Contains(body, `<div class="nav-next"><a href="`+base+helloPath+`#comments" >Newer comments</a></div>`) || strings.Contains(body, "nav-previous") {
		t.Errorf("comment page 1 wrong: %s", body)
	}
	if code, _, loc := get(t, anon, base+"/?p="+itoa64(hello.ID)+"&cpage=1"); code != http.StatusMovedPermanently || loc != base+helloPath+"comment-page-1/" {
		t.Errorf("?cpage=1: %d %s", code, loc)
	}
	// Oldest-first paging and descending order within the page.
	if err := srv.settings.Set(ctx, map[string]any{"default_comments_page": "oldest", "comment_order": "desc"}); err != nil {
		t.Fatal(err)
	}
	srv.purgeTags("post:" + itoa64(hello.ID))
	_, body, _ = get(t, anon, base+helloPath)
	if !strings.Contains(body, "Second comment") || strings.Contains(body, "Third comment") || strings.Index(body, "Second comment") > strings.Index(body, "A WordPress Go Commenter") {
		t.Errorf("oldest/desc page wrong: %s", body)
	}
	if !strings.Contains(body, `<div class="nav-next"><a href="`+base+helloPath+`comment-page-2/#comments" >Newer comments</a></div>`) {
		t.Error("oldest-first navigation wrong")
	}

	// wp_check_comment_flood(): a second comment within 15 seconds is refused.
	code, _, loc := post(t, anon, base+"/wp-comments-post", url.Values{"comment_post_ID": {itoa64(hello.ID)}, "comment": {"Quick one"}, "author": {"Fast"}, "email": {"fast@example.test"}})
	if code != http.StatusFound || !strings.Contains(loc, "#comment-") {
		t.Fatalf("first comment: %d %s", code, loc)
	}
	code, body, _ = post(t, anon, base+"/wp-comments-post", url.Values{"comment_post_ID": {itoa64(hello.ID)}, "comment": {"Quick two"}, "author": {"Fast"}, "email": {"fast@example.test"}})
	if code != http.StatusTooManyRequests || !strings.Contains(body, "You are posting comments too quickly. Slow down.") {
		t.Errorf("flood: %d %s", code, body)
	}
	// Moderators are not throttled.
	if code, _, _ := post(t, c, base+"/wp-comments-post", url.Values{"comment_post_ID": {itoa64(hello.ID)}, "comment": {"Admin one"}}); code != http.StatusFound {
		t.Errorf("admin comment 1: %d", code)
	}
	if code, _, _ := post(t, c, base+"/wp-comments-post", url.Values{"comment_post_ID": {itoa64(hello.ID)}, "comment": {"Admin two"}}); code != http.StatusFound {
		t.Errorf("admin comment 2: %d", code)
	}

	// close_comments_for_old_posts closes comments on posts older than close_comments_days_old.
	if err := srv.settings.Set(ctx, map[string]any{"close_comments_for_old_posts": true, "close_comments_days_old": 2}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * 24 * time.Hour)
	oldPost := mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: adminID, Title: "Old news", Content: "<p>old</p>", PublishedAt: &old, CommentStatus: "open"})
	_, body, _ = get(t, anon, base+pathOf(t, base, lb.Permalink(oldPost)))
	if strings.Contains(body, `id="commentform"`) || strings.Contains(body, "comment-reply.min.js") {
		t.Error("old post still shows the comment form")
	}
	if code, body, _ := post(t, anon, base+"/wp-comments-post", url.Values{"comment_post_ID": {itoa64(oldPost.ID)}, "comment": {"Late"}, "author": {"Late"}, "email": {"late@example.test"}}); code != http.StatusForbidden || !strings.Contains(body, "comments are closed") {
		t.Errorf("comment on old post: %d %s", code, body)
	}
	fresh := mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: adminID, Title: "Fresh news", Content: "<p>new</p>", CommentStatus: "open"})
	if _, body, _ := get(t, anon, base+pathOf(t, base, lb.Permalink(fresh))); !strings.Contains(body, `id="commentform"`) {
		t.Error("fresh post lost its comment form")
	}
	// Pages are never auto-closed.
	sample, _ := srv.store.PostBySlug(ctx, "page", "sample-page")
	sample.PublishedAt = &old
	sample.CommentStatus = "open"
	if err := srv.store.UpdatePost(ctx, sample, adminID); err != nil {
		t.Fatal(err)
	}
	srv.purgeTags("post:" + itoa64(sample.ID))
	if _, body, _ := get(t, anon, base+pathOf(t, base, lb.Permalink(sample))); !strings.Contains(body, `id="commentform"`) {
		t.Error("old page lost its comment form")
	}
}
