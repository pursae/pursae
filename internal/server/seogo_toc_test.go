package server

import (
	"encoding/json"
	"strings"
	"testing"

	"wpgo/internal/store"
)

func TestSeogoTOCContentBlock(t *testing.T) {
	def := seogoTOCOpts{Title: "Contents", Levels: []int{2, 3}, Numbered: true}
	in := `<p>intro</p>` + seogoTOCMarkerFor(def) + `<h2 class="wp-block-heading">Ne Zaman Çıktı?</h2><p>a</p><h3>Detay &amp; Not</h3><h4>skip</h4><h2 id="own">Second</h2><h2>Ne Zaman Çıktı?</h2>`
	out := seogoTOCContent(in, def, false, 3)
	for _, want := range []string{
		`<h2 class="wp-block-heading" id="ne-zaman-çıktı">`,
		`<h3 id="detay-not">`,
		`<h2 id="ne-zaman-çıktı-2">`,
		`<p class="seogo-toc-title"><strong>Contents</strong></p>`,
		`<ol class="seogo-toc-list"><li><a href="#ne-zaman-%C3%A7%C4%B1kt%C4%B1">Ne Zaman Çıktı?</a><ol><li><a href="#detay-not">Detay &amp; Not</a></li></ol></li><li><a href="#own">Second</a></li>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "skip</a>") || strings.Contains(out, seogoTOCMarker) {
		t.Errorf("h4 listed or marker left:\n%s", out)
	}
}

func TestSeogoTOCContentAuto(t *testing.T) {
	def := seogoTOCOpts{Title: "TOC", Levels: []int{2}, Collapsible: true}
	in := `<p>x</p><h2>A</h2><h2>B</h2>`
	if out := seogoTOCContent(in, def, true, 3); strings.Contains(out, "seogo-toc") {
		t.Error("auto insert ignored the minimum")
	}
	out := seogoTOCContent(in, def, true, 2)
	if !strings.HasPrefix(out, `<p>x</p><nav class="seogo-toc`) || !strings.Contains(out, `<details open><summary class="seogo-toc-title">TOC</summary><ul class="seogo-toc-list">`) {
		t.Errorf("auto insert: %s", out)
	}
	if seogoTOCContent(in, def, false, 1) != in {
		t.Error("content without a block changed")
	}
}

func TestSeogoTOCNesting(t *testing.T) {
	got := seogoTOCHTML(seogoTOCOpts{Numbered: false}, []seogoTOCItem{{2, "a", "A"}, {4, "b", "B"}, {2, "c", "C"}})
	want := `<ul class="seogo-toc-list"><li><a href="#a">A</a><ul><li><ul><li><a href="#b">B</a></li></ul></li></ul></li><li><a href="#c">C</a></li></ul>`
	if !strings.Contains(got, want) {
		t.Errorf("got %s", got)
	}
}

func TestSeogoTOCBlockOpts(t *testing.T) {
	o := seogoTOCBlockOpts(seogoTOCOpts{Title: "T", Levels: []int{2}, Numbered: true}, map[string]any{"levels": []any{3.0, 9.0}, "listStyle": "ul", "title": " "})
	if o.Title != "T" || len(o.Levels) != 1 || o.Levels[0] != 3 || o.Numbered {
		t.Errorf("%+v", o)
	}
}

func TestSEOGoTOCEndToEnd(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	seogoToggle(t, c, base, "activate")
	content := `<!-- wp:seogo/toc {"title":"Contents"} /--><!-- wp:heading --><h2 class="wp-block-heading">First part</h2><!-- /wp:heading --><!-- wp:paragraph --><p><a href="https://example.org/">out</a></p><!-- /wp:paragraph --><!-- wp:heading {"level":3} --><h3 class="wp-block-heading">Sub part</h3><!-- /wp:heading -->`
	_, body := restSend(t, c, base, "POST", "/wp-json/wp/v2/posts", `{"title":"TOC post","status":"publish","content":`+string(must(json.Marshal(content)))+`}`)
	if !strings.Contains(body, `"id"`) {
		t.Fatalf("create: %s", body)
	}
	posts, _, _ := srv.store.QueryPosts(t.Context(), store.PostQuery{Types: []string{"post"}, Search: "TOC post"})
	if len(posts) == 0 {
		t.Fatal("post not found")
	}
	_, front, _ := get(t, c, srv.links(t.Context()).Permalink(posts[0]))
	for _, want := range []string{`<nav class="seogo-toc wp-block-seogo-toc" aria-label="Contents">`, `<a href="#first-part">First part</a>`, `id="first-part"`, `id="sub-part"`} {
		if !strings.Contains(front, want) {
			t.Errorf("front page lacks %q", want)
		}
	}
	_, list, _ := get(t, c, base+"/wp-admin/edit")
	if !strings.Contains(list, "SEO Details") || !strings.Contains(list, "Schema:</strong> Article") || !strings.Contains(list, `&#8599; 1`) {
		t.Error("posts list lacks the SEO Details column values")
	}
	_, tab, _ := get(t, c, base+"/wp-admin/seogo?page=general&tab=toc")
	if !strings.Contains(tab, `name="toc_auto"`) || !strings.Contains(tab, `name="toc_levels" value="2" checked`) {
		t.Error("Table of Contents settings tab missing")
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
