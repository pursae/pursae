package server

import (
	"strings"
	"testing"

	"wpgo/internal/blocks"
	"wpgo/internal/links"
	"wpgo/internal/store"
)

func TestSplitPages(t *testing.T) {
	content := "<!-- wp:paragraph --><p>One</p><!-- /wp:paragraph -->\n<!-- wp:nextpage -->\n<!--nextpage-->\n<!-- /wp:nextpage -->\n<!-- wp:paragraph --><p>Two</p><!-- /wp:paragraph --><!--nextpage--><p>Three</p>"
	pages := splitPages(content)
	if len(pages) != 3 || !strings.Contains(pages[0], "One") || strings.Contains(pages[0], "Two") || !strings.Contains(pages[1], "Two") || pages[2] != "<p>Three</p>" {
		t.Errorf("pages = %q", pages)
	}
	if strings.Contains(pages[1], "wp:nextpage") {
		t.Errorf("block delimiters left in %q", pages[1])
	}
	if got := splitPages("<!--nextpage--><p>Only</p>"); len(got) != 1 {
		t.Errorf("leading nextpage: %q", got)
	}
	if got := splitPages("plain"); len(got) != 1 || got[0] != "plain" {
		t.Errorf("plain: %q", got)
	}
}

func TestImgLoadingAttrs(t *testing.T) {
	rc := &renderCtx{mainLoop: true}
	img := `<img width="1024" height="768" src="https://example.test/a.jpg" alt="" class="wp-image-5" />`
	// The first large image of the main loop is in the viewport: fetchpriority high, no lazy loading.
	got := imgAddLoadingAttrs(rc, img)
	if !strings.HasPrefix(got, `<img decoding="async" fetchpriority="high" width="1024"`) || strings.Contains(got, "lazy") {
		t.Errorf("first image = %s", got)
	}
	// Images 2 and 3 stay eager (wp_omit_loading_attr_threshold), without a second fetchpriority.
	for i := 0; i < 2; i++ {
		got = imgAddLoadingAttrs(rc, img)
		if strings.Contains(got, "lazy") || strings.Contains(got, "fetchpriority") {
			t.Errorf("image %d = %s", i+2, got)
		}
	}
	// The fourth is lazy, and sizes gets the "auto" prefix.
	got = imgAddAutoSizes(imgAddLoadingAttrs(rc, img+` sizes="(max-width: 1024px) 100vw, 1024px"`))
	if !strings.Contains(got, `loading="lazy"`) || !strings.Contains(got, `sizes="auto, (max-width: 1024px) 100vw, 1024px"`) {
		t.Errorf("fourth image = %s", got)
	}
	// Outside the loop (widgets) images are lazy; in feeds they are not.
	if got := imgAddLoadingAttrs(&renderCtx{}, img); !strings.Contains(got, `loading="lazy"`) {
		t.Errorf("widget image = %s", got)
	}
	if got := imgAddLoadingAttrs(&renderCtx{feed: true}, img); strings.Contains(got, "loading=") || !strings.Contains(got, `decoding="async"`) {
		t.Errorf("feed image = %s", got)
	}
	// Without dimensions only decoding is added; explicit attributes are kept.
	if got := imgAddLoadingAttrs(rc, `<img src="x.jpg" />`); got != `<img decoding="async" src="x.jpg" />` {
		t.Errorf("no dimensions = %s", got)
	}
	if got := imgAddLoadingAttrs(rc, `<img loading="eager" decoding="sync" width="10" height="10" src="x.jpg" />`); got != `<img loading="eager" decoding="sync" width="10" height="10" src="x.jpg" />` {
		t.Errorf("explicit attrs = %s", got)
	}
}

func TestImageMatchesRatio(t *testing.T) {
	if !imageMatchesRatio(1024, 768, 300, 225) || !imageMatchesRatio(2000, 1500, 1024, 768) {
		t.Error("4:3 sizes should match")
	}
	if imageMatchesRatio(1024, 768, 150, 150) {
		t.Error("cropped thumbnail should not match")
	}
}

func TestLinksPaging(t *testing.T) {
	b := &links.Builder{Home: "http://example.test", Structure: "/%postname%/"}
	p := &store.Post{ID: 7, Type: "post", Slug: "hello", Status: "publish"}
	if got := b.PostPageLink(p, 1); got != "http://example.test/hello/" {
		t.Errorf("page 1 = %s", got)
	}
	if got := b.PostPageLink(p, 3); got != "http://example.test/hello/3/" {
		t.Errorf("page 3 = %s", got)
	}
	if got := b.CommentPageLink(p, 2, 3, true); got != "http://example.test/hello/comment-page-2/" {
		t.Errorf("comment page 2 = %s", got)
	}
	if got := b.CommentPageLink(p, 3, 3, true); got != "http://example.test/hello/" {
		t.Errorf("newest last page = %s", got)
	}
	if got := b.CommentPageLink(p, 1, 3, false); got != "http://example.test/hello/" {
		t.Errorf("oldest page 1 = %s", got)
	}
	if got := b.PostCommentsFeedFormat(p, "atom"); got != "http://example.test/hello/feed/atom/" {
		t.Errorf("atom comments feed = %s", got)
	}
	if got := b.CommentsFeedLink("atom"); got != "http://example.test/comments/feed/atom/" {
		t.Errorf("site atom comments feed = %s", got)
	}
	plain := &links.Builder{Home: "http://example.test"}
	if got := plain.PostPageLink(p, 2); got != "http://example.test/?p=7&page=2" {
		t.Errorf("plain page 2 = %s", got)
	}
	if got := plain.CommentPageLink(p, 2, 3, false); got != "http://example.test/?cpage=2&p=7" {
		t.Errorf("plain cpage = %s", got)
	}
}

func TestSocialLinkMarkup(t *testing.T) {
	b := parseOneBlock(t, `<!-- wp:social-link {"url":"wordpress.org","service":"wordpress"} /-->`)
	got := renderSocialLink(nil, b, true, false)
	for _, want := range []string{`<li class="wp-social-link wp-social-link-wordpress wp-block-social-link">`, `href="https://wordpress.org"`, `class="wp-block-social-link-anchor"`,
		`rel="noopener nofollow" target="_blank"`, `<svg width="24" height="24" viewBox="0 0 24 24" version="1.1"`, `<span class="wp-block-social-link-label screen-reader-text">WordPress</span>`} {
		if !strings.Contains(got, want) {
			t.Errorf("social link misses %q in %s", want, got)
		}
	}
	if got := renderSocialLink(nil, parseOneBlock(t, `<!-- wp:social-link {"service":"x"} /-->`), false, false); got != "" {
		t.Errorf("no URL should render nothing, got %s", got)
	}
	got = renderSocialLink(nil, parseOneBlock(t, `<!-- wp:social-link {"url":"me@example.test","service":"mail"} /-->`), false, true)
	if !strings.Contains(got, `href="mailto:me@example.test"`) || !strings.Contains(got, `<span class="wp-block-social-link-label">Mail</span>`) {
		t.Errorf("mail link = %s", got)
	}
}

func parseOneBlock(t *testing.T, content string) *blocks.Block {
	t.Helper()
	for _, b := range blocks.Parse(content) {
		if b.BlockName != nil {
			return b
		}
	}
	t.Fatalf("no block in %q", content)
	return nil
}

func TestPhpFloat(t *testing.T) {
	if phpFloat(8) != "8" || phpFloat(15.5) != "15.5" || phpFloat(10.666666666666666) != "10.666666666667" {
		t.Errorf("phpFloat: %s %s %s", phpFloat(8), phpFloat(15.5), phpFloat(10.666666666666666))
	}
}
