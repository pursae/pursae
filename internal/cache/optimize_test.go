package cache

import (
	"strings"
	"testing"
)

func opt(doc string, o OptimizeConfig) string { return string(Optimize([]byte(doc), o)) }

func TestLazyImages(t *testing.T) {
	doc := `<body><IMG src="hero.jpg" alt="a > b"><img src="b.jpg" loading="eager"><img src='c.jpg' alt=x/>` +
		`<img src="d.jpg" decoding="sync" /><pre><img src="pre.jpg"></pre><!-- <img src="comment.jpg"> -->` +
		`<script>var s = '<img src="js.jpg">';</script></body>`
	got := opt(doc, OptimizeConfig{LazyImages: true, LazySkipFirst: 1})
	want := `<body><IMG src="hero.jpg" alt="a > b" fetchpriority="high"><img src="b.jpg" loading="eager"><img src='c.jpg' alt=x/ loading="lazy" decoding="async">` +
		`<img src="d.jpg" decoding="sync" loading="lazy" /><pre><img src="pre.jpg"></pre><!-- <img src="comment.jpg"> -->` +
		`<script>var s = '<img src="js.jpg">';</script></body>`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// LazySkipFirst 0 makes every image lazy.
	if got := opt(`<img src="a">`, OptimizeConfig{LazyImages: true}); got != `<img src="a" loading="lazy" decoding="async">` {
		t.Fatalf("skip 0: %s", got)
	}
}

func TestLazyIframesAndFetchPriority(t *testing.T) {
	got := opt(`<iframe src="v"></iframe><img src="a" loading="lazy"><iframe src="w" loading="eager"></iframe>`, OptimizeConfig{LazyImages: true, LazySkipFirst: 1})
	want := `<iframe src="v" loading="lazy"></iframe><img src="a" loading="lazy"><iframe src="w" loading="eager"></iframe>`
	if got != want { // a first image already marked lazy gets no fetchpriority
		t.Fatalf("got %s", got)
	}
}

func TestDeferScripts(t *testing.T) {
	doc := `<html><head><script src="/head.js" id="jquery-core-js"></script></head><body>` +
		`<script src="/wp-includes/js/jquery/jquery.min.js?ver=3" id="jquery-core-js"></script>` +
		`<script src="/a.js" id="slider-js"></script>` +
		`<script src="/b.js" id="other-js"></script>` +
		`<script src="/c.js" id="comment-reply-js" async></script>` +
		`<script type="module" src="/m.js" id="mod-js"></script>` +
		`<script src="/d.js" id="dep-js"></script><script id="dep-js-after">dep()</script>` +
		`<script>var x = "<script src='/x.js' id='slider-js'>";</script>` +
		`</body></html>`
	got := opt(doc, OptimizeConfig{DeferScripts: true, DeferAllowlist: []string{"jquery", "slider", "comment-reply", "mod", "dep"}})
	for _, want := range []string{
		`<head><script src="/head.js" id="jquery-core-js"></script></head>`, // head scripts untouched
		`<script src="/wp-includes/js/jquery/jquery.min.js?ver=3" id="jquery-core-js" defer></script>`,
		`<script src="/a.js" id="slider-js" defer></script>`,
		`<script src="/b.js" id="other-js"></script>`,
		`<script src="/c.js" id="comment-reply-js" async></script>`,
		`<script type="module" src="/m.js" id="mod-js"></script>`,
		`<script src="/d.js" id="dep-js"></script>`,
		`<script>var x = "<script src='/x.js' id='slider-js'>";</script>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s\nin %s", want, got)
		}
	}
}

func TestCollapseWhitespace(t *testing.T) {
	doc := "<html>\n  <head>\n    <style>\n  a  {  }\n</style>\n  </head>\n<body>\n\n  <p>Hello   world</p>  \n" +
		"<pre>\n  keep   this\n</pre>\n  <textarea>  a\n\n b </textarea>\n\n<script>\n  if (a  <  b) {}\n</script>\n\n</body>\n</html>"
	got := opt(doc, OptimizeConfig{CollapseWhitespace: true})
	want := "<html> <head> <style>\n  a  {  }\n</style> </head> <body> <p>Hello   world</p> " +
		"<pre>\n  keep   this\n</pre> <textarea>  a\n\n b </textarea> <script>\n  if (a  <  b) {}\n</script> </body> </html>"
	if got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

func TestMinifyRemovesComments(t *testing.T) {
	doc := "<body>\n<!-- note -->\n<p>a</p><!--[if lt IE 9]><script src=\"h.js\"></script><![endif]--><!--noptimize-->x<!--/noptimize--></body>"
	got := opt(doc, OptimizeConfig{CollapseWhitespace: true})
	if strings.Contains(got, "note") || !strings.Contains(got, "<!--[if lt IE 9]>") || !strings.Contains(got, "<!--noptimize-->") {
		t.Fatalf("got %q", got)
	}
}

func TestDeferURLFragment(t *testing.T) {
	got := opt(`<body><script src="/wp-content/plugins/x/slider.bundle.js?v=2" id="x-js"></script></body>`,
		OptimizeConfig{DeferScripts: true, DeferAllowlist: []string{"plugins/x/"}})
	if !strings.Contains(got, `id="x-js" defer>`) {
		t.Fatalf("got %s", got)
	}
}

func TestPreload(t *testing.T) {
	doc := `<!doctype html><html><head data-x="1"><meta charset="utf-8"></head><body><header>x</header></body></html>`
	got := opt(doc, OptimizeConfig{Preload: []PreloadAsset{
		{Href: "/fonts/a.woff2", As: "font", Type: "font/woff2", CrossOrigin: true},
		{Href: `/x".css`, As: "style"},
	}})
	want := `<head data-x="1"><link rel="preload" href="/fonts/a.woff2" as="font" type="font/woff2" crossorigin>` +
		`<link rel="preload" href="/x&#34;.css" as="style"><meta charset="utf-8">`
	if !strings.Contains(got, want) || strings.Count(got, "preload") != 2 {
		t.Fatalf("got %s", got)
	}
	// Already preloaded assets are not duplicated.
	again := opt(got, OptimizeConfig{Preload: []PreloadAsset{{Href: "/fonts/a.woff2", As: "font"}}})
	if strings.Count(again, "/fonts/a.woff2") != 1 {
		t.Fatalf("duplicated: %s", again)
	}
}

// A theme font is preloaded only on documents that load the theme (IfDocContains): a builder
// canvas page without the theme's stylesheet gets no preload of a font it never uses.
func TestPreloadIfDocContains(t *testing.T) {
	asset := PreloadAsset{Href: "/wp-content/themes/t/fonts/a.woff2", As: "font", CrossOrigin: true, IfDocContains: "/wp-content/themes/t/"}
	themed := `<html><head><link rel='stylesheet' href='/wp-content/themes/t/style.css'></head><body></body></html>`
	if got := opt(themed, OptimizeConfig{Preload: []PreloadAsset{asset}}); !strings.Contains(got, `rel="preload" href="/wp-content/themes/t/fonts/a.woff2"`) {
		t.Errorf("themed page lost its preload: %s", got)
	}
	canvas := `<html><head><link rel='stylesheet' href='/wp-content/web-builder/site/wb.css'></head><body class="web-builder-page"></body></html>`
	if got := opt(canvas, OptimizeConfig{Preload: []PreloadAsset{asset}}); strings.Contains(got, "preload") {
		t.Errorf("page without the theme got its font preload: %s", got)
	}
}

func TestOptimizeDisabledAndMalformed(t *testing.T) {
	doc := `<body><img src="a"><img src="b`
	if got := opt(doc, OptimizeConfig{}); got != doc {
		t.Fatal("disabled optimizer changed the document")
	}
	if got := opt(doc, OptimizeConfig{LazyImages: true}); !strings.HasSuffix(got, `<img src="b`) {
		t.Fatalf("unterminated tag modified: %s", got)
	}
	unterminated := `<body><script src="/a.js" id="a-js">never closed <img src="x">`
	if got := opt(unterminated, OptimizeConfig{LazyImages: true}); got != unterminated {
		t.Fatalf("content after unterminated script modified: %s", got)
	}
}

func TestLazyImagesSkipsQuotedMarkup(t *testing.T) {
	for _, attribute := range []string{
		`title="<img src='fake'>"`,
		`title='<IMG src="fake">'`,
		`data-template="<iframe src='fake'></iframe>"`,
	} {
		doc := `<body><div ` + attribute + `>label</div><img src="real"></body>`
		got := opt(doc, OptimizeConfig{LazyImages: true, LazySkipFirst: 1})
		if !strings.Contains(got, attribute) || !strings.Contains(got, `src="real" fetchpriority="high"`) {
			t.Errorf("quoted markup was treated as an image: %s", got)
		}
	}
}

func TestStrayLessThanKeepsOptimizing(t *testing.T) {
	doc := `<html><head><title>1 < 2</title></head><body><p>a < b</p><img src="a"><img src="b">` +
		`<script src="/wp-includes/js/jquery/jquery.min.js"></script></body></html>`
	got := opt(doc, OptimizeConfig{LazyImages: true, LazySkipFirst: 1, DeferScripts: true, DeferAllowlist: []string{"jquery"}})
	for _, want := range []string{`<img src="a" fetchpriority="high">`, `<img src="b" loading="lazy" decoding="async">`, ` defer`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q after a stray '<': %s", want, got)
		}
	}
}
